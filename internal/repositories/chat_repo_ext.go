package repositories

import (
	"strings"
	"time"

	"gorm.io/gorm/clause"

	"the-fulfillment/backend/internal/models"
)

// Phần truy vấn cho chat đợt 2 (#5453): nhóm, file, cảm xúc, thu hồi/sửa, tìm
// kiếm, hiện diện, dò mã đơn/batch. Chạy được trên Postgres lẫn SQLite (test).

// DeleteMember xoá một người khỏi cuộc (rời nhóm / bị mời ra).
func (r *ChatRepository) DeleteMember(convID, userID uint) error {
	return r.db.Where("conversation_id = ? AND user_id = ?", convID, userID).Delete(&models.ChatMember{}).Error
}

// ---------- tin ----------

func (r *ChatRepository) MessageByID(id uint) (*models.ChatMessage, error) {
	var m models.ChatMessage
	if err := r.db.First(&m, id).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *ChatRepository) SaveMessage(m *models.ChatMessage) error { return r.db.Save(m).Error }

// SearchMessages tìm tin chữ (chưa thu hồi) trong các cuộc userID đang ở, mới nhất trước.
func (r *ChatRepository) SearchMessages(userID uint, q string, convID uint, limit int) ([]models.ChatMessage, error) {
	q = strings.TrimSpace(strings.ToLower(q))
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	tx := r.db.Table("chat_messages AS msg").Select("msg.*").
		Joins("JOIN chat_members AS m ON m.conversation_id = msg.conversation_id AND m.user_id = ?", userID).
		Where("msg.kind = ? AND msg.recalled_at IS NULL", models.ChatMsgText).
		Where(`LOWER(msg.body) LIKE ? ESCAPE '\'`, like)
	if convID > 0 {
		tx = tx.Where("msg.conversation_id = ?", convID)
	}
	var rows []models.ChatMessage
	err := tx.Order("msg.id desc").Limit(limit).Scan(&rows).Error
	return rows, err
}

// ---------- file ----------

func (r *ChatRepository) CreateAttachment(a *models.ChatAttachment) error {
	return r.db.Create(a).Error
}
func (r *ChatRepository) SaveAttachment(a *models.ChatAttachment) error { return r.db.Save(a).Error }
func (r *ChatRepository) DeleteAttachment(id uint) error {
	return r.db.Delete(&models.ChatAttachment{}, id).Error
}

func (r *ChatRepository) AttachmentByID(id uint) (*models.ChatAttachment, error) {
	var a models.ChatAttachment
	if err := r.db.First(&a, id).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *ChatRepository) AttachmentsByIDs(ids []uint) ([]models.ChatAttachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []models.ChatAttachment
	err := r.db.Where("id IN ?", ids).Order("id asc").Find(&rows).Error
	return rows, err
}

func (r *ChatRepository) AttachmentsOfMessages(msgIDs []uint) ([]models.ChatAttachment, error) {
	if len(msgIDs) == 0 {
		return nil, nil
	}
	var rows []models.ChatAttachment
	err := r.db.Where("message_id IN ?", msgIDs).Order("id asc").Find(&rows).Error
	return rows, err
}

// OrphanAttachments: file tải lên mà chưa gắn vào tin nào từ trước mốc `before`.
func (r *ChatRepository) OrphanAttachments(before time.Time, limit int) ([]models.ChatAttachment, error) {
	var rows []models.ChatAttachment
	err := r.db.Where("message_id IS NULL AND created_at < ?", before).Order("id asc").Limit(limit).Find(&rows).Error
	return rows, err
}

// ---------- cảm xúc ----------

func (r *ChatRepository) ReactionsOf(msgIDs []uint) ([]models.ChatReaction, error) {
	if len(msgIDs) == 0 {
		return nil, nil
	}
	var rows []models.ChatReaction
	err := r.db.Where("message_id IN ?", msgIDs).Order("id asc").Find(&rows).Error
	return rows, err
}

func (r *ChatRepository) ReactionBy(msgID, userID uint) (*models.ChatReaction, error) {
	var x models.ChatReaction
	if err := r.db.Where("message_id = ? AND user_id = ?", msgID, userID).First(&x).Error; err != nil {
		return nil, err
	}
	return &x, nil
}

func (r *ChatRepository) SaveReaction(x *models.ChatReaction) error { return r.db.Save(x).Error }
func (r *ChatRepository) DeleteReaction(id uint) error {
	return r.db.Delete(&models.ChatReaction{}, id).Error
}
func (r *ChatRepository) DeleteReactionsOf(msgID uint) error {
	return r.db.Where("message_id = ?", msgID).Delete(&models.ChatReaction{}).Error
}

// ---------- hiện diện ----------

// TouchPresence ghi "lần cuối còn mở chat" của userID.
func (r *ChatRepository) TouchPresence(userID uint, at time.Time) error {
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_seen_at"}),
	}).Create(&models.ChatPresence{UserID: userID, LastSeenAt: at}).Error
}

func (r *ChatRepository) AllPresence() ([]models.ChatPresence, error) {
	var rows []models.ChatPresence
	err := r.db.Find(&rows).Error
	return rows, err
}

// ---------- dò mã đơn / batch ----------

// CodeHit là một mã có thật: kind "order" (id = đơn) hoặc "batch" (id = batch).
type CodeHit struct {
	Code string
	Kind string
	ID   uint
}

// ResolveCodes tra mã đơn (orders.internal_code), mã sản phẩm "106788_1/2"
// (order_items.internal_code → đơn của nó) và mã batch ("#101558"). Bản ghi đã
// xoá mềm không tính.
func (r *ChatRepository) ResolveCodes(orderCodes, itemCodes, batchCodes []string) ([]CodeHit, error) {
	var out []CodeHit
	type row struct {
		ID   uint
		Code string
	}
	if len(orderCodes) > 0 {
		var rows []row
		if err := r.db.Model(&models.Order{}).Select("id, internal_code AS code").
			Where("internal_code IN ?", orderCodes).Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, x := range rows {
			out = append(out, CodeHit{Code: x.Code, Kind: "order", ID: x.ID})
		}
	}
	if len(itemCodes) > 0 {
		var rows []row
		if err := r.db.Model(&models.OrderItem{}).Select("order_id AS id, internal_code AS code").
			Where("internal_code IN ?", itemCodes).Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, x := range rows {
			out = append(out, CodeHit{Code: x.Code, Kind: "order", ID: x.ID})
		}
	}
	if len(batchCodes) > 0 {
		var rows []row
		if err := r.db.Model(&models.Batch{}).Select("id, code").
			Where("code IN ?", batchCodes).Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, x := range rows {
			out = append(out, CodeHit{Code: x.Code, Kind: "batch", ID: x.ID})
		}
	}
	return out, nil
}
