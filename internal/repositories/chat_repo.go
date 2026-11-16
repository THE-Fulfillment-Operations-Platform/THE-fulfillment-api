package repositories

import (
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// ChatRepository là phần truy vấn của chat nội bộ (bảng chat_*). Mọi câu chạy
// được cả trên Postgres (production) lẫn SQLite (test).
type ChatRepository struct{ db *gorm.DB }

// ChatUserRow là những gì chat cần biết về một tài khoản.
type ChatUserRow struct {
	ID         uint
	FullName   string
	Email      string
	Role       models.Role
	SellerID   *uint
	IsActive   bool
	SellerName string
}

func (r *ChatRepository) userQuery() *gorm.DB {
	return r.db.Table("users").
		Select("users.id, users.full_name, users.email, users.role, users.seller_id, users.is_active, sellers.name AS seller_name").
		Joins("LEFT JOIN sellers ON sellers.id = users.seller_id").
		Where("users.deleted_at IS NULL")
}

// UsersByIDs nạp thông tin người dùng (cả người đã khoá: tên vẫn phải hiện ở tin cũ).
func (r *ChatRepository) UsersByIDs(ids []uint) ([]ChatUserRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []ChatUserRow
	err := r.userQuery().Where("users.id IN ?", ids).Scan(&rows).Error
	return rows, err
}

// ActiveUsers trả các tài khoản đang hoạt động, lọc theo tên/email (q rỗng = tất cả).
func (r *ChatRepository) ActiveUsers(q string, limit int) ([]ChatUserRow, error) {
	var rows []ChatUserRow
	tx := r.userQuery().Where("users.is_active = ?", true)
	if q = strings.TrimSpace(strings.ToLower(q)); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		tx = tx.Where(`(LOWER(users.full_name) LIKE ? ESCAPE '\' OR LOWER(users.email) LIKE ? ESCAPE '\')`, like, like)
	}
	err := tx.Order("users.full_name asc").Limit(limit).Scan(&rows).Error
	return rows, err
}

// ---------- cuộc trò chuyện & thành viên ----------

func (r *ChatRepository) CreateConversation(c *models.ChatConversation) error {
	return r.db.Create(c).Error
}

func (r *ChatRepository) SaveConversation(c *models.ChatConversation) error {
	return r.db.Save(c).Error
}

func (r *ChatRepository) ConversationByID(id uint) (*models.ChatConversation, error) {
	var c models.ChatConversation
	if err := r.db.First(&c, id).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *ChatRepository) ConversationsByIDs(ids []uint) ([]models.ChatConversation, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []models.ChatConversation
	err := r.db.Where("id IN ?", ids).Find(&rows).Error
	return rows, err
}

func (r *ChatRepository) ConversationByKey(key string) (*models.ChatConversation, error) {
	var c models.ChatConversation
	if err := r.db.Where("direct_key = ?", key).First(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *ChatRepository) CreateMember(m *models.ChatMember) error { return r.db.Create(m).Error }
func (r *ChatRepository) SaveMember(m *models.ChatMember) error   { return r.db.Save(m).Error }

// Member trả dòng thành viên của userID trong cuộc, hoặc ErrRecordNotFound.
func (r *ChatRepository) Member(convID, userID uint) (*models.ChatMember, error) {
	var m models.ChatMember
	if err := r.db.Where("conversation_id = ? AND user_id = ?", convID, userID).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// MembersOf: thành viên của các cuộc.
func (r *ChatRepository) MembersOf(convIDs []uint) ([]models.ChatMember, error) {
	if len(convIDs) == 0 {
		return nil, nil
	}
	var rows []models.ChatMember
	err := r.db.Where("conversation_id IN ?", convIDs).Order("id asc").Find(&rows).Error
	return rows, err
}

// MyMemberships: mọi cuộc người này ở trong.
func (r *ChatRepository) MyMemberships(userID uint) ([]models.ChatMember, error) {
	var rows []models.ChatMember
	err := r.db.Where("user_id = ?", userID).Find(&rows).Error
	return rows, err
}

// UnreadCounts đếm tin chưa đọc của userID theo từng cuộc: tin của người khác có
// id lớn hơn mốc đã đọc.
func (r *ChatRepository) UnreadCounts(userID uint) (map[uint]int, error) {
	type row struct {
		ConversationID uint
		N              int
	}
	var rows []row
	err := r.db.Table("chat_messages AS msg").
		Select("msg.conversation_id AS conversation_id, COUNT(*) AS n").
		Joins("JOIN chat_members AS m ON m.conversation_id = msg.conversation_id").
		Where("m.user_id = ? AND msg.id > m.last_read_message_id AND msg.sender_id <> ?", userID, userID).
		Group("msg.conversation_id").Scan(&rows).Error
	out := map[uint]int{}
	for _, r := range rows {
		out[r.ConversationID] = r.N
	}
	return out, err
}

// ---------- tin nhắn ----------

func (r *ChatRepository) CreateMessage(m *models.ChatMessage) error { return r.db.Create(m).Error }

func (r *ChatRepository) MessagesByIDs(ids []uint) ([]models.ChatMessage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []models.ChatMessage
	err := r.db.Where("id IN ?", ids).Find(&rows).Error
	return rows, err
}

func (r *ChatRepository) MessageByClientID(senderID uint, clientID string) (*models.ChatMessage, error) {
	var m models.ChatMessage
	if err := r.db.Where("sender_id = ? AND client_id = ?", senderID, clientID).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// MessagesBefore: `limit` tin mới nhất có id < beforeID (0 = từ cuối), theo id GIẢM dần.
func (r *ChatRepository) MessagesBefore(convID, beforeID uint, limit int) ([]models.ChatMessage, error) {
	var rows []models.ChatMessage
	tx := r.db.Where("conversation_id = ?", convID)
	if beforeID > 0 {
		tx = tx.Where("id < ?", beforeID)
	}
	err := tx.Order("id desc").Limit(limit).Find(&rows).Error
	return rows, err
}

// MessagesAfter: tối đa `limit` tin có id > afterID, theo id TĂNG dần.
func (r *ChatRepository) MessagesAfter(convID, afterID uint, limit int) ([]models.ChatMessage, error) {
	var rows []models.ChatMessage
	err := r.db.Where("conversation_id = ? AND id > ?", convID, afterID).
		Order("id asc").Limit(limit).Find(&rows).Error
	return rows, err
}
