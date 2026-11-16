package services

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Thao tác trên một tin đã gửi: sửa (15 phút), thu hồi, thả cảm xúc; và tìm tin.

// loadOwnMessage: tin messageID, người gọi ở trong cuộc của nó.
func (s *ChatService) loadMessageFor(a Actor, messageID uint) (*models.ChatMessage, []uint, error) {
	if err := s.ensureEnabled(a); err != nil {
		return nil, nil, err
	}
	m, err := s.repo.Chat.MessageByID(messageID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, apperr.NotFound("Không tìm thấy tin nhắn")
		}
		return nil, nil, apperr.Internal("could not load message").Wrap(err)
	}
	if _, _, err := s.requireMember(a, m.ConversationID); err != nil {
		return nil, nil, apperr.NotFound("Không tìm thấy tin nhắn")
	}
	ids, err := s.memberIDs(m.ConversationID)
	if err != nil {
		return nil, nil, apperr.Internal("could not load members").Wrap(err)
	}
	return m, ids, nil
}

func (s *ChatService) pushUpdated(ids []uint, m *models.ChatMessage) (*ChatMessageDTO, error) {
	d, err := s.messageDTO(m)
	if err != nil {
		return nil, err
	}
	s.hub.SendJSON(ids, map[string]interface{}{"type": "message.updated", "message": d})
	return d, nil
}

// EditMessage: người gửi sửa chữ của tin mình trong 15 phút đầu.
func (s *ChatService) EditMessage(a Actor, messageID uint, rawBody string) (*ChatMessageDTO, error) {
	m, ids, err := s.loadMessageFor(a, messageID)
	if err != nil {
		return nil, err
	}
	if m.SenderID != a.ID {
		return nil, apperr.Forbidden("Chỉ người gửi mới sửa được tin này")
	}
	if m.Kind == models.ChatMsgSystem || m.RecalledAt != nil {
		return nil, apperr.Unprocessable("Tin này không sửa được")
	}
	if s.now().Sub(m.CreatedAt) > chatEditWindow {
		return nil, apperr.Unprocessable("Chỉ sửa được trong 15 phút sau khi gửi")
	}
	body, err := cleanBody(rawBody)
	if err != nil {
		return nil, err
	}
	if body == "" {
		atts, _ := s.repo.Chat.AttachmentsOfMessages([]uint{m.ID})
		if len(atts) == 0 {
			return nil, apperr.BadRequest("Tin nhắn trống — muốn bỏ hẳn thì dùng Thu hồi")
		}
	}
	if body == m.Body {
		return s.messageDTO(m)
	}
	now := s.now()
	m.Body, m.EditedAt, m.Refs = body, &now, s.resolveRefs(body)
	if err := s.repo.Chat.SaveMessage(m); err != nil {
		return nil, apperr.Internal("could not edit message").Wrap(err)
	}
	return s.pushUpdated(ids, m)
}

// RecallMessage: người gửi thu hồi tin của mình. Chữ, file (cả trong kho) và
// cảm xúc bị xoá hẳn; trong luồng chỉ còn dòng "Tin nhắn đã được thu hồi".
func (s *ChatService) RecallMessage(a Actor, messageID uint) (*ChatMessageDTO, error) {
	m, ids, err := s.loadMessageFor(a, messageID)
	if err != nil {
		return nil, err
	}
	if m.SenderID != a.ID {
		return nil, apperr.Forbidden("Chỉ người gửi mới thu hồi được tin này")
	}
	if m.Kind == models.ChatMsgSystem {
		return nil, apperr.Unprocessable("Tin hệ thống không thu hồi được")
	}
	if m.RecalledAt != nil {
		return s.messageDTO(m)
	}
	atts, err := s.repo.Chat.AttachmentsOfMessages([]uint{m.ID})
	if err != nil {
		return nil, apperr.Internal("could not load files").Wrap(err)
	}
	now := s.now()
	m.RecalledAt, m.Body, m.Refs = &now, "", ""
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		r := repositories.New(tx).Chat
		if err := r.SaveMessage(m); err != nil {
			return err
		}
		if err := r.DeleteReactionsOf(m.ID); err != nil {
			return err
		}
		for _, x := range atts {
			if err := r.DeleteAttachment(x.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, apperr.Internal("could not recall message").Wrap(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, x := range atts {
		s.deleteStored(ctx, x)
	}
	return s.pushUpdated(ids, m)
}

// React: thả / đổi / bỏ cảm xúc. Mỗi người một cảm xúc mỗi tin; bấm lại đúng
// cảm xúc đang có là bỏ.
func (s *ChatService) React(a Actor, messageID uint, emoji string) ([]ChatReactionDTO, error) {
	emoji = strings.TrimSpace(emoji)
	allowed := false
	for _, e := range ChatReactions {
		if e == emoji {
			allowed = true
			break
		}
	}
	if !allowed || utf8.RuneCountInString(emoji) > 4 {
		return nil, apperr.Unprocessable("Cảm xúc không có trong bộ chọn")
	}
	m, ids, err := s.loadMessageFor(a, messageID)
	if err != nil {
		return nil, err
	}
	if m.RecalledAt != nil || m.Kind == models.ChatMsgSystem {
		return nil, apperr.Unprocessable("Tin này không thả cảm xúc được")
	}
	cur, err := s.repo.Chat.ReactionBy(m.ID, a.ID)
	switch {
	case err == nil && cur.Emoji == emoji:
		err = s.repo.Chat.DeleteReaction(cur.ID)
	case err == nil:
		cur.Emoji = emoji
		err = s.repo.Chat.SaveReaction(cur)
	case errors.Is(err, gorm.ErrRecordNotFound):
		err = s.repo.Chat.SaveReaction(&models.ChatReaction{MessageID: m.ID, UserID: a.ID, Emoji: emoji})
	}
	if err != nil {
		return nil, apperr.Internal("could not save reaction").Wrap(err)
	}
	rows, err := s.repo.Chat.ReactionsOf([]uint{m.ID})
	if err != nil {
		return nil, apperr.Internal("could not load reactions").Wrap(err)
	}
	out := groupReactions(rows)
	s.hub.SendJSON(ids, map[string]interface{}{
		"type": "reaction", "conversation_id": m.ConversationID, "message_id": m.ID, "reactions": out,
	})
	return out, nil
}

// Search tìm tin chữ trong các cuộc của người gọi (convID > 0: chỉ cuộc đó).
func (s *ChatService) Search(a Actor, q string, convID uint) ([]ChatMessageDTO, error) {
	if err := s.ensureEnabled(a); err != nil {
		return nil, err
	}
	q = strings.TrimSpace(q)
	if utf8.RuneCountInString(q) < 2 {
		return nil, apperr.BadRequest("Gõ ít nhất 2 ký tự để tìm")
	}
	if convID > 0 {
		if _, _, err := s.requireMember(a, convID); err != nil {
			return nil, err
		}
	}
	if r := []rune(q); len(r) > 100 {
		q = string(r[:100])
	}
	rows, err := s.repo.Chat.SearchMessages(a.ID, q, convID, 50)
	if err != nil {
		return nil, apperr.Internal("could not search").Wrap(err)
	}
	return s.messageDTOs(rows)
}
