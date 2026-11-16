package models

import "time"

// Chat nội bộ (Nghĩa #5355; mở cho mọi tài khoản nội bộ + nhóm, file, trả lời,
// cảm xúc, thu hồi/sửa, ghim, đã xem ở #5453, 17/11/2026). Dữ liệu nằm trong các
// bảng chat_* MỚI; không bảng cũ nào bị thêm cột hay đổi kiểu. Các cột thêm sau
// đợt đầu (kind, title, pinned_at, reply_to_id…) đều có mặc định, AutoMigrate chỉ
// ADD COLUMN, không sửa dòng cũ.

// Loại cuộc trò chuyện.
const (
	ChatKindDirect = "direct"
	ChatKindGroup  = "group"
)

// Loại tin.
const (
	ChatMsgText   = "text"
	ChatMsgSystem = "system" // "A đã tạo nhóm", "A đã thêm B"… do máy ghi
)

// ChatConversation là một cuộc trò chuyện: 1-1 (direct) hoặc nhóm (group).
type ChatConversation struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// DirectKey = "<id nhỏ>:<id lớn>" với cuộc 1-1: hai người chỉ có MỘT cuộc, kể
	// cả khi cả hai cùng bấm "nhắn tin" một lúc (unique). Nhóm mang khoá ngẫu
	// nhiên "g:<hex>" để giữ cột NOT NULL + unique như cũ.
	DirectKey     string    `json:"-" gorm:"size:40;not null;uniqueIndex"`
	Kind          string    `json:"kind" gorm:"size:10;not null;default:'direct'"`
	Title         string    `json:"title" gorm:"size:120;not null;default:''"`
	CreatedByID   uint      `json:"created_by_id" gorm:"not null"`
	LastMessageID uint      `json:"last_message_id" gorm:"not null;default:0"`
	LastMessageAt time.Time `json:"last_message_at" gorm:"index"`
}

func (ChatConversation) TableName() string { return "chat_conversations" }

// IsGroup: cuộc nhóm.
func (c ChatConversation) IsGroup() bool { return c.Kind == ChatKindGroup }

// ChatMember là một người trong cuộc, kèm mốc đã đọc và ghim của người đó.
type ChatMember struct {
	ID                uint `gorm:"primaryKey"`
	ConversationID    uint `gorm:"not null;uniqueIndex:uniq_chat_member"`
	UserID            uint `gorm:"not null;uniqueIndex:uniq_chat_member;index"`
	JoinedAt          time.Time
	LastReadMessageID uint `gorm:"not null;default:0"`
	LastReadAt        *time.Time
	// PinnedAt: người này ghim cuộc lên đầu danh sách (NULL = không ghim).
	PinnedAt *time.Time
}

func (ChatMember) TableName() string { return "chat_members" }

// ChatMessage là một tin (chữ, kèm file, hoặc dòng hệ thống).
type ChatMessage struct {
	ID             uint      `json:"id" gorm:"primaryKey;index:idx_chat_messages_conv,priority:2"`
	CreatedAt      time.Time `json:"created_at"`
	ConversationID uint      `json:"conversation_id" gorm:"not null;index:idx_chat_messages_conv,priority:1"`
	SenderID       uint      `json:"sender_id" gorm:"not null;index;uniqueIndex:uniq_chat_msg_client,priority:1"`
	Body           string    `json:"body" gorm:"type:text;not null"`
	// ClientID do trình duyệt sinh cho mỗi lần bấm gửi: mạng chập chờn gửi lại
	// thì vẫn chỉ ra MỘT tin (unique theo người gửi; NULL không tính trùng).
	ClientID *string `json:"client_id,omitempty" gorm:"size:64;uniqueIndex:uniq_chat_msg_client,priority:2"`

	Kind string `json:"kind" gorm:"size:10;not null;default:'text'"`
	// ReplyToID: tin được trích dẫn (cùng cuộc).
	ReplyToID *uint `json:"reply_to_id,omitempty"`
	// EditedAt: lần sửa cuối (chỉ người gửi, trong 15 phút đầu).
	EditedAt *time.Time `json:"edited_at,omitempty"`
	// RecalledAt: người gửi đã thu hồi — nội dung, file, cảm xúc bị xoá hẳn.
	RecalledAt *time.Time `json:"recalled_at,omitempty"`
	// Refs: mã đơn / mã batch có thật nhắc trong tin, dạng JSON
	// [{"code":"106788","kind":"order","id":6788}], dò lúc gửi và lúc sửa.
	Refs string `json:"-" gorm:"type:text;not null;default:''"`
}

func (ChatMessage) TableName() string { return "chat_messages" }

// ChatAttachment là một file gửi trong chat. Tải lên trước (MessageID NULL), gắn
// vào tin lúc gửi; file tải lên mà không gửi trong 24 giờ thì bị dọn.
type ChatAttachment struct {
	ID             uint      `json:"id" gorm:"primaryKey"`
	CreatedAt      time.Time `json:"created_at"`
	ConversationID uint      `json:"conversation_id" gorm:"not null;index"`
	UploaderID     uint      `json:"uploader_id" gorm:"not null"`
	MessageID      *uint     `json:"message_id" gorm:"index"`
	// StorageKey: đường dẫn trong kho (đĩa server hoặc bucket Supabase "chat").
	StorageKey string `json:"-" gorm:"size:200;not null"`
	// ThumbKey: ảnh thu nhỏ (JPEG) cho ảnh lớn; rỗng = dùng thẳng file gốc.
	ThumbKey string `json:"-" gorm:"size:200;not null;default:''"`
	Name     string `json:"name" gorm:"size:255;not null"`
	Mime     string `json:"mime" gorm:"size:120;not null"`
	Size     int64  `json:"size" gorm:"not null"`
	Width    int    `json:"width" gorm:"not null;default:0"`
	Height   int    `json:"height" gorm:"not null;default:0"`
}

func (ChatAttachment) TableName() string { return "chat_attachments" }

// ChatReaction là cảm xúc một người thả vào một tin (mỗi người một cảm xúc/tin).
type ChatReaction struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	MessageID uint   `gorm:"not null;uniqueIndex:uniq_chat_reaction,priority:1"`
	UserID    uint   `gorm:"not null;uniqueIndex:uniq_chat_reaction,priority:2"`
	Emoji     string `gorm:"size:16;not null"`
}

func (ChatReaction) TableName() string { return "chat_reactions" }

// ChatPresence: lần cuối một người còn mở chat (để hiện "hoạt động X phút trước").
// "Đang online" thì lấy từ kết nối WebSocket trong RAM, không lưu.
type ChatPresence struct {
	UserID     uint      `gorm:"primaryKey;autoIncrement:false"`
	LastSeenAt time.Time `gorm:"not null"`
}

func (ChatPresence) TableName() string { return "chat_presence" }

// ChatModels là danh sách bảng chat cho AutoMigrate (và cho test).
func ChatModels() []interface{} {
	return []interface{}{
		&ChatConversation{}, &ChatMember{}, &ChatMessage{},
		&ChatAttachment{}, &ChatReaction{}, &ChatPresence{},
	}
}
