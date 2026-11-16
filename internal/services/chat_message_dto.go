package services

import (
	"encoding/json"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// Hình dạng một tin gửi về trình duyệt (REST và WebSocket dùng chung).

// ChatReactions là bộ cảm xúc cố định (thứ tự hiện trên thanh chọn).
var ChatReactions = []string{"👍", "❤️", "😂", "😮", "😢", "🙏"}

// ChatAttachmentDTO là một file trong tin, với URL đã ký có hạn.
type ChatAttachmentDTO struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	Mime    string `json:"mime"`
	Size    int64  `json:"size"`
	IsImage bool   `json:"is_image"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
	// URL: file gốc (ảnh/PDF mở xem được trong trình duyệt). ThumbURL: ảnh nhỏ cho
	// khung chat. DownloadURL: ép tải về với đúng tên file.
	URL         string `json:"url"`
	ThumbURL    string `json:"thumb_url,omitempty"`
	DownloadURL string `json:"download_url"`
}

// ChatReactionDTO: một cảm xúc và những ai đã thả.
type ChatReactionDTO struct {
	Emoji   string `json:"emoji"`
	UserIDs []uint `json:"user_ids"`
}

// ChatRef là một mã đơn / mã batch có thật trong tin.
type ChatRef struct {
	Code string `json:"code"`
	Kind string `json:"kind"` // "order" | "batch"
	ID   uint   `json:"id"`
}

// ChatReplyDTO: tóm tắt tin được trích dẫn.
type ChatReplyDTO struct {
	ID         uint   `json:"id"`
	SenderID   uint   `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Body       string `json:"body"`
	Recalled   bool   `json:"recalled"`
	// Attachment: tên file đầu tiên (tin chỉ có file); IsImage để hiện "Ảnh".
	Attachment string `json:"attachment,omitempty"`
	IsImage    bool   `json:"is_image,omitempty"`
}

// ChatMessageDTO là một tin.
type ChatMessageDTO struct {
	ID             uint                `json:"id"`
	CreatedAt      time.Time           `json:"created_at"`
	ConversationID uint                `json:"conversation_id"`
	SenderID       uint                `json:"sender_id"`
	SenderName     string              `json:"sender_name"`
	Kind           string              `json:"kind"`
	Body           string              `json:"body"`
	ClientID       *string             `json:"client_id,omitempty"`
	EditedAt       *time.Time          `json:"edited_at,omitempty"`
	RecalledAt     *time.Time          `json:"recalled_at,omitempty"`
	ReplyTo        *ChatReplyDTO       `json:"reply_to,omitempty"`
	Attachments    []ChatAttachmentDTO `json:"attachments"`
	Reactions      []ChatReactionDTO   `json:"reactions"`
	Refs           []ChatRef           `json:"refs"`
}

func chatLog(format string, args ...interface{}) { log.Printf("chat: "+format, args...) }

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func (s *ChatService) messageDTO(m *models.ChatMessage) (*ChatMessageDTO, error) {
	out, err := s.messageDTOs([]models.ChatMessage{*m})
	if err != nil {
		return nil, err
	}
	return &out[0], nil
}

// messageDTOs dựng tin kèm tên người gửi, tin được trả lời, file (URL ký),
// cảm xúc và mã đơn/batch — vài truy vấn cho cả trang, không theo từng tin.
func (s *ChatService) messageDTOs(rows []models.ChatMessage) ([]ChatMessageDTO, error) {
	out := make([]ChatMessageDTO, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]uint, 0, len(rows))
	senders := make([]uint, 0, len(rows))
	var replyIDs []uint
	for _, m := range rows {
		ids = append(ids, m.ID)
		senders = append(senders, m.SenderID)
		if m.ReplyToID != nil {
			replyIDs = append(replyIDs, *m.ReplyToID)
		}
	}
	replies, err := s.repo.Chat.MessagesByIDs(uniqueUints(replyIDs))
	if err != nil {
		return nil, apperr.Internal("could not load replies").Wrap(err)
	}
	replyBy := map[uint]models.ChatMessage{}
	for _, r := range replies {
		replyBy[r.ID] = r
		senders = append(senders, r.SenderID)
	}
	users, err := s.loadUsers(senders)
	if err != nil {
		return nil, err
	}
	name := func(id uint) string {
		if u, ok := users[id]; ok {
			return displayName(u)
		}
		return "Tài khoản đã xoá"
	}
	atts, err := s.repo.Chat.AttachmentsOfMessages(append(append([]uint{}, ids...), replyIDs...))
	if err != nil {
		return nil, apperr.Internal("could not load files").Wrap(err)
	}
	attBy := map[uint][]models.ChatAttachment{}
	for _, a := range atts {
		if a.MessageID != nil {
			attBy[*a.MessageID] = append(attBy[*a.MessageID], a)
		}
	}
	reacts, err := s.repo.Chat.ReactionsOf(ids)
	if err != nil {
		return nil, apperr.Internal("could not load reactions").Wrap(err)
	}
	reactBy := map[uint][]models.ChatReaction{}
	for _, r := range reacts {
		reactBy[r.MessageID] = append(reactBy[r.MessageID], r)
	}
	for _, m := range rows {
		d := ChatMessageDTO{
			ID: m.ID, CreatedAt: m.CreatedAt, ConversationID: m.ConversationID, SenderID: m.SenderID,
			SenderName: name(m.SenderID), Kind: m.Kind, Body: m.Body, ClientID: m.ClientID,
			EditedAt: m.EditedAt, RecalledAt: m.RecalledAt,
			Attachments: []ChatAttachmentDTO{}, Reactions: groupReactions(reactBy[m.ID]), Refs: []ChatRef{},
		}
		if d.Kind == "" {
			d.Kind = models.ChatMsgText
		}
		if m.RecalledAt == nil {
			for _, a := range attBy[m.ID] {
				d.Attachments = append(d.Attachments, s.attachmentDTO(a))
			}
			if m.Refs != "" {
				_ = json.Unmarshal([]byte(m.Refs), &d.Refs)
			}
		}
		if m.ReplyToID != nil {
			if r, ok := replyBy[*m.ReplyToID]; ok {
				rd := &ChatReplyDTO{ID: r.ID, SenderID: r.SenderID, SenderName: name(r.SenderID), Recalled: r.RecalledAt != nil}
				if !rd.Recalled {
					rd.Body = truncateRunes(strings.TrimSpace(r.Body), 200)
					if ra := attBy[r.ID]; len(ra) > 0 {
						rd.Attachment, rd.IsImage = ra[0].Name, isImageMime(ra[0].Mime)
					}
				}
				d.ReplyTo = rd
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// groupReactions gom cảm xúc theo emoji, theo thứ tự bộ cố định.
func groupReactions(rows []models.ChatReaction) []ChatReactionDTO {
	out := []ChatReactionDTO{}
	if len(rows) == 0 {
		return out
	}
	by := map[string][]uint{}
	for _, r := range rows {
		by[r.Emoji] = append(by[r.Emoji], r.UserID)
	}
	for _, e := range ChatReactions {
		if u := by[e]; len(u) > 0 {
			out = append(out, ChatReactionDTO{Emoji: e, UserIDs: u})
		}
	}
	return out
}
