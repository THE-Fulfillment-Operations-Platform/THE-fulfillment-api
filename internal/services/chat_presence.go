package services

import (
	"encoding/json"
	"fmt"
	"time"
)

// "Đang soạn…" và online / hoạt động X phút trước (#5453). Không lưu gì cho
// "đang soạn"; online lấy từ kết nối WebSocket trong RAM, lần cuối hoạt động lưu
// ở bảng chat_presence để còn sau khi API khởi động lại.

// onInbound nhận khung trình duyệt gửi lên qua WebSocket (trừ ping).
func (s *ChatService) onInbound(userID uint, typ string, raw []byte) {
	if typ != "typing" {
		return
	}
	var in struct {
		ConversationID uint `json:"conversation_id"`
	}
	if json.Unmarshal(raw, &in) != nil || in.ConversationID == 0 {
		return
	}
	key := fmt.Sprintf("%d:%d", userID, in.ConversationID)
	now := s.now()
	if v, ok := s.typingAt.Load(key); ok && now.Sub(v.(time.Time)) < time.Second {
		return
	}
	s.typingAt.Store(key, now)
	if _, err := s.repo.Chat.Member(in.ConversationID, userID); err != nil {
		return // không ở trong cuộc: bỏ qua, không báo lỗi
	}
	ids, err := s.memberIDs(in.ConversationID)
	if err != nil {
		return
	}
	others := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id != userID {
			others = append(others, id)
		}
	}
	s.hub.SendJSON(others, map[string]interface{}{
		"type": "typing", "conversation_id": in.ConversationID, "user_id": userID,
	})
}

// onPresence: một người vừa online (kết nối đầu) / offline (đóng kết nối cuối).
func (s *ChatService) onPresence(userID uint, online bool) {
	now := s.now()
	if err := s.repo.Chat.TouchPresence(userID, now); err != nil {
		chatLog("ghi hiện diện lỗi: %v", err)
	}
	s.hub.Broadcast(map[string]interface{}{
		"type": "presence", "user_id": userID, "online": online, "last_seen_at": now,
	})
}

// ChatPresenceDTO: ai đang online, ai hoạt động lần cuối lúc nào.
type ChatPresenceDTO struct {
	Online   []uint               `json:"online"`
	LastSeen map[string]time.Time `json:"last_seen"`
}

// Presence trả trạng thái hiện diện cho web dựng lần đầu (sau đó theo sự kiện).
func (s *ChatService) Presence(a Actor) (*ChatPresenceDTO, error) {
	if err := s.ensureEnabled(a); err != nil {
		return nil, err
	}
	out := &ChatPresenceDTO{Online: s.hub.OnlineUsers(), LastSeen: map[string]time.Time{}}
	rows, err := s.repo.Chat.AllPresence()
	if err != nil {
		chatLog("đọc hiện diện lỗi: %v", err)
		return out, nil
	}
	for _, r := range rows {
		out.LastSeen[fmt.Sprintf("%d", r.UserID)] = r.LastSeenAt
	}
	return out, nil
}
