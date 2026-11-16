package services

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Nhóm chat (#5453): tạo nhóm, đặt tên, thêm/bớt người, rời nhóm; ghim cuộc.
// Ai quản lý thành viên: người tạo nhóm và Owner/Admin (HasAdminRights) — phải
// đang ở trong nhóm. Đổi tên: mọi thành viên (có dòng hệ thống ghi lại).

func cleanTitle(raw string) (string, error) {
	t := strings.Join(strings.Fields(raw), " ")
	if t == "" {
		return "", apperr.BadRequest("Đặt tên cho nhóm")
	}
	if utf8.RuneCountInString(t) > chatMaxTitle {
		return "", apperr.Unprocessable(fmt.Sprintf("Tên nhóm dài quá %d ký tự", chatMaxTitle))
	}
	return t, nil
}

// pickUsers: những người hợp lệ để thêm vào nhóm (đang hoạt động, được mở chat).
func (s *ChatService) pickUsers(a Actor, ids []uint) (map[uint]chatUser, []uint, error) {
	ids = uniqueUints(ids)
	users, err := s.loadUsers(append([]uint{a.ID}, ids...))
	if err != nil {
		return nil, nil, err
	}
	viewer := s.viewerOf(a, users)
	var ok []uint
	for _, id := range ids {
		if id == a.ID {
			continue
		}
		u, found := users[id]
		if !found || !s.canSee(viewer, u) {
			return nil, nil, apperr.Unprocessable(fmt.Sprintf("Không thêm được người dùng #%d (không có hoặc chưa được mở chat)", id))
		}
		ok = append(ok, id)
	}
	return users, ok, nil
}

func namesOf(users map[uint]chatUser, ids []uint) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, displayName(users[id]))
	}
	return strings.Join(parts, ", ")
}

// CreateGroup tạo nhóm với người gọi + userIDs.
func (s *ChatService) CreateGroup(a Actor, title string, userIDs []uint) (*ChatConversationDTO, error) {
	if err := s.ensureEnabled(a); err != nil {
		return nil, err
	}
	t, err := cleanTitle(title)
	if err != nil {
		return nil, err
	}
	users, ids, err := s.pickUsers(a, userIDs)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, apperr.BadRequest("Chọn ít nhất một người cho nhóm")
	}
	if len(ids)+1 > chatMaxGroupSize {
		return nil, apperr.Unprocessable(fmt.Sprintf("Nhóm tối đa %d người", chatMaxGroupSize))
	}
	now := s.now()
	conv := &models.ChatConversation{
		DirectKey: "g:" + randHex(8), Kind: models.ChatKindGroup, Title: t,
		CreatedByID: a.ID, LastMessageAt: now,
	}
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		r := repositories.New(tx).Chat
		if err := r.CreateConversation(conv); err != nil {
			return err
		}
		for _, id := range append([]uint{a.ID}, ids...) {
			if err := r.CreateMember(&models.ChatMember{ConversationID: conv.ID, UserID: id, JoinedAt: now}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, apperr.Internal("could not create group").Wrap(err)
	}
	all := append([]uint{a.ID}, ids...)
	s.postSystem(conv, a.ID, fmt.Sprintf("%s đã tạo nhóm “%s” với %s", displayName(users[a.ID]), t, namesOf(users, ids)), all)
	s.hub.SendJSON(all, map[string]interface{}{"type": "conversation.updated", "conversation_id": conv.ID})
	return s.GetConversation(a, conv.ID)
}

func (s *ChatService) requireGroup(a Actor, convID uint) (*models.ChatConversation, error) {
	c, _, err := s.requireMember(a, convID)
	if err != nil {
		return nil, err
	}
	if !c.IsGroup() {
		return nil, apperr.BadRequest("Cuộc 1-1 không có thao tác nhóm")
	}
	return c, nil
}

// RenameGroup đổi tên nhóm (mọi thành viên).
func (s *ChatService) RenameGroup(a Actor, convID uint, title string) (*ChatConversationDTO, error) {
	c, err := s.requireGroup(a, convID)
	if err != nil {
		return nil, err
	}
	t, err := cleanTitle(title)
	if err != nil {
		return nil, err
	}
	if t == c.Title {
		return s.GetConversation(a, convID)
	}
	c.Title = t
	if err := s.repo.Chat.SaveConversation(c); err != nil {
		return nil, apperr.Internal("could not rename group").Wrap(err)
	}
	ids, _ := s.memberIDs(convID)
	users, _ := s.loadUsers([]uint{a.ID})
	s.postSystem(c, a.ID, fmt.Sprintf("%s đã đổi tên nhóm thành “%s”", displayName(users[a.ID]), t), ids)
	s.hub.SendJSON(ids, map[string]interface{}{"type": "conversation.updated", "conversation_id": convID})
	return s.GetConversation(a, convID)
}

// AddMembers thêm người vào nhóm (người tạo nhóm / Owner / Admin).
func (s *ChatService) AddMembers(a Actor, convID uint, userIDs []uint) (*ChatConversationDTO, error) {
	c, err := s.requireGroup(a, convID)
	if err != nil {
		return nil, err
	}
	if !canManage(a, c) {
		return nil, apperr.Forbidden("Chỉ người tạo nhóm hoặc Owner/Admin mới thêm người được")
	}
	users, ids, err := s.pickUsers(a, userIDs)
	if err != nil {
		return nil, err
	}
	current, err := s.memberIDs(convID)
	if err != nil {
		return nil, apperr.Internal("could not load members").Wrap(err)
	}
	in := map[uint]bool{}
	for _, id := range current {
		in[id] = true
	}
	var added []uint
	for _, id := range ids {
		if !in[id] {
			added = append(added, id)
		}
	}
	if len(added) == 0 {
		return s.GetConversation(a, convID)
	}
	if len(current)+len(added) > chatMaxGroupSize {
		return nil, apperr.Unprocessable(fmt.Sprintf("Nhóm tối đa %d người", chatMaxGroupSize))
	}
	now := s.now()
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		r := repositories.New(tx).Chat
		for _, id := range added {
			// Người mới vào: tin cũ coi như đã đọc, không đổ một đống "chưa đọc".
			m := &models.ChatMember{ConversationID: convID, UserID: id, JoinedAt: now, LastReadMessageID: c.LastMessageID}
			if err := r.CreateMember(m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, apperr.Internal("could not add members").Wrap(err)
	}
	all := append(current, added...)
	s.postSystem(c, a.ID, fmt.Sprintf("%s đã thêm %s vào nhóm", displayName(users[a.ID]), namesOf(users, added)), all)
	s.hub.SendJSON(all, map[string]interface{}{"type": "conversation.updated", "conversation_id": convID})
	return s.GetConversation(a, convID)
}

// RemoveMember mời một người ra khỏi nhóm (người tạo nhóm / Owner / Admin).
// userID là chính người gọi thì là rời nhóm (ai cũng được).
func (s *ChatService) RemoveMember(a Actor, convID, userID uint) error {
	c, err := s.requireGroup(a, convID)
	if err != nil {
		return err
	}
	self := userID == a.ID
	if !self && !canManage(a, c) {
		return apperr.Forbidden("Chỉ người tạo nhóm hoặc Owner/Admin mới bớt người được")
	}
	if _, err := s.repo.Chat.Member(convID, userID); err != nil {
		return apperr.NotFound("Người này không ở trong nhóm")
	}
	before, err := s.memberIDs(convID)
	if err != nil {
		return apperr.Internal("could not load members").Wrap(err)
	}
	if err := s.repo.Chat.DeleteMember(convID, userID); err != nil {
		return apperr.Internal("could not remove member").Wrap(err)
	}
	users, _ := s.loadUsers([]uint{a.ID, userID})
	var rest []uint
	for _, id := range before {
		if id != userID {
			rest = append(rest, id)
		}
	}
	text := fmt.Sprintf("%s đã rời nhóm", displayName(users[a.ID]))
	if !self {
		text = fmt.Sprintf("%s đã mời %s ra khỏi nhóm", displayName(users[a.ID]), displayName(users[userID]))
	}
	if len(rest) > 0 {
		s.postSystem(c, a.ID, text, rest)
		s.hub.SendJSON(rest, map[string]interface{}{"type": "conversation.updated", "conversation_id": convID})
	}
	s.hub.SendJSON([]uint{userID}, map[string]interface{}{"type": "conversation.removed", "conversation_id": convID})
	return nil
}

// SetPinned ghim / bỏ ghim một cuộc (riêng cho người gọi).
func (s *ChatService) SetPinned(a Actor, convID uint, pinned bool) (*time.Time, error) {
	_, m, err := s.requireMember(a, convID)
	if err != nil {
		return nil, err
	}
	if pinned {
		now := s.now()
		m.PinnedAt = &now
	} else {
		m.PinnedAt = nil
	}
	if err := s.repo.Chat.SaveMember(m); err != nil {
		return nil, apperr.Internal("could not pin").Wrap(err)
	}
	s.hub.SendJSON([]uint{a.ID}, map[string]interface{}{"type": "pin", "conversation_id": convID, "pinned_at": m.PinnedAt})
	return m.PinnedAt, nil
}
