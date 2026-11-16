package models

import "time"

// Chat nội bộ — chỉ nhắn tin chữ 1-1 (Nghĩa #5355, thu phạm vi ở #5369, chỉ bật
// cho Kỹ thuật viên trước ở #5370; 16/11/2026). Dữ liệu nằm trong các bảng
// chat_* MỚI; không bảng cũ nào bị thêm cột hay đổi kiểu.

// ChatConversation là một cuộc trò chuyện 1-1 giữa hai tài khoản.
type ChatConversation struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// DirectKey = "<id nhỏ>:<id lớn>": hai người chỉ có MỘT cuộc, kể cả khi cả hai
	// cùng bấm "nhắn tin" một lúc (unique).
	DirectKey     string    `json:"-" gorm:"size:40;not null;uniqueIndex"`
	CreatedByID   uint      `json:"created_by_id" gorm:"not null"`
	LastMessageID uint      `json:"last_message_id" gorm:"not null;default:0"`
	LastMessageAt time.Time `json:"last_message_at" gorm:"index"`
}

func (ChatConversation) TableName() string { return "chat_conversations" }

// ChatMember là một người trong cuộc, kèm mốc đã đọc của người đó.
type ChatMember struct {
	ID                uint `gorm:"primaryKey"`
	ConversationID    uint `gorm:"not null;uniqueIndex:uniq_chat_member"`
	UserID            uint `gorm:"not null;uniqueIndex:uniq_chat_member;index"`
	JoinedAt          time.Time
	LastReadMessageID uint `gorm:"not null;default:0"`
	LastReadAt        *time.Time
}

func (ChatMember) TableName() string { return "chat_members" }

// ChatMessage là một tin chữ.
type ChatMessage struct {
	ID             uint      `json:"id" gorm:"primaryKey;index:idx_chat_messages_conv,priority:2"`
	CreatedAt      time.Time `json:"created_at"`
	ConversationID uint      `json:"conversation_id" gorm:"not null;index:idx_chat_messages_conv,priority:1"`
	SenderID       uint      `json:"sender_id" gorm:"not null;index;uniqueIndex:uniq_chat_msg_client,priority:1"`
	Body           string    `json:"body" gorm:"type:text;not null"`
	// ClientID do trình duyệt sinh cho mỗi lần bấm gửi: mạng chập chờn gửi lại
	// thì vẫn chỉ ra MỘT tin (unique theo người gửi; NULL không tính trùng).
	ClientID *string `json:"client_id,omitempty" gorm:"size:64;uniqueIndex:uniq_chat_msg_client,priority:2"`
}

func (ChatMessage) TableName() string { return "chat_messages" }

// ChatModels là danh sách bảng chat cho AutoMigrate (và cho test).
func ChatModels() []interface{} {
	return []interface{}{&ChatConversation{}, &ChatMember{}, &ChatMessage{}}
}
