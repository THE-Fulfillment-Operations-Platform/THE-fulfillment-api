package repositories

import (
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// ---------- Batches ----------

type BatchFilter struct {
	Page
	MaterialID *uint
	Status     string
	Priority   string
	DateFrom   *time.Time
	DateTo     *time.Time
	// ParentBatchID scopes the list to the children of one parent batch. When nil
	// (the default), children are hidden and only parent + flat batches are listed
	// (see baseQuery) so the list isn't cluttered with split sub-batches.
	ParentBatchID *uint
}

type BatchRepository struct{ db *gorm.DB }

func activeBatchItems(db *gorm.DB) *gorm.DB {
	return db.Joins("JOIN order_items ON order_items.id = batch_items.order_item_id").
		Where("order_items.cancellation_status NOT IN ?", []models.CancellationStatus{models.CancellationSeller, models.CancellationApproved})
}

func (r *BatchRepository) Create(b *models.Batch) error { return r.db.Create(b).Error }
func (r *BatchRepository) Update(b *models.Batch) error { return r.db.Save(b).Error }

func (r *BatchRepository) CreateItems(items []models.BatchItem) error {
	if len(items) == 0 {
		return nil
	}
	return r.db.Create(&items).Error
}

func (r *BatchRepository) FindByID(id uint) (*models.Batch, error) {
	var b models.Batch
	err := r.db.
		Preload("Material").
		Preload("CreatedBy").
		Preload("Items", activeBatchItems).
		Preload("Items.OrderItem.Order").
		Preload("Items.Material").
		// A parent batch preloads its children (with each child's active items so the
		// detail view can show per-child item counts). Children/flat batches have none.
		Preload("ChildBatches", func(db *gorm.DB) *gorm.DB { return db.Order("sequence asc") }).
		Preload("ChildBatches.Items", activeBatchItems).
		First(&b, id).Error
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// FindLite loads a batch WITHOUT any association preloads — for status roll-ups
// and guards that only need the batch row itself.
func (r *BatchRepository) FindLite(id uint) (*models.Batch, error) {
	var b models.Batch
	if err := r.db.First(&b, id).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

// UpdateStatusColumn writes only the batch's status (plus updated_at), so a
// roll-up can't clobber concurrent edits to other batch fields.
func (r *BatchRepository) UpdateStatusColumn(id uint, status models.InternalStatus) error {
	return r.db.Model(&models.Batch{}).Where("id = ?", id).Update("status", status).Error
}

// UpdateBatchItemStatus writes only a batch item's status (plus updated_at).
func (r *BatchRepository) UpdateBatchItemStatus(id uint, status models.InternalStatus) error {
	return r.db.Model(&models.BatchItem{}).Where("id = ?", id).Update("status", status).Error
}

// ChildBatchesFor returns the child batches of a parent (id + status are enough
// for the parent status roll-up). Ordered by sequence for stable display.
func (r *BatchRepository) ChildBatchesFor(parentID uint) ([]models.Batch, error) {
	var rows []models.Batch
	err := r.db.Where("parent_batch_id = ?", parentID).Order("sequence asc").Find(&rows).Error
	return rows, err
}

func (r *BatchRepository) FindByCode(code string) (*models.Batch, error) {
	var b models.Batch
	if err := r.db.Preload("Material").Where("code = ?", code).First(&b).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *BatchRepository) baseQuery(f BatchFilter) *gorm.DB {
	q := r.db.Model(&models.Batch{})
	if f.MaterialID != nil {
		q = q.Where("material_id = ?", *f.MaterialID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Priority != "" {
		q = q.Where("priority = ?", f.Priority)
	}
	if f.DateFrom != nil {
		q = q.Where("created_at >= ?", *f.DateFrom)
	}
	if f.DateTo != nil {
		q = q.Where("created_at <= ?", *f.DateTo)
	}
	// Child-scoping: with a parent id, list only that parent's children; otherwise
	// hide children entirely so the default list shows parent + flat batches only.
	if f.ParentBatchID != nil {
		q = q.Where("parent_batch_id = ?", *f.ParentBatchID)
	} else {
		q = q.Where("parent_batch_id IS NULL")
	}
	return q
}

func (r *BatchRepository) List(f BatchFilter) ([]models.Batch, int64, error) {
	var rows []models.Batch
	var total int64
	r.baseQuery(f).Count(&total)
	err := r.baseQuery(f).
		Preload("Material").
		Preload("CreatedBy").
		Preload("Items", activeBatchItems).
		Order("id desc").
		Limit(f.PageSize).Offset(f.Offset()).Find(&rows).Error
	return rows, total, err
}

// ---------- Batch items ----------

func (r *BatchRepository) FindBatchItemByID(id uint) (*models.BatchItem, error) {
	var bi models.BatchItem
	err := r.db.Preload("OrderItem.Order").Preload("Material").Preload("Batch").First(&bi, id).Error
	if err != nil {
		return nil, err
	}
	return &bi, nil
}

func (r *BatchRepository) UpdateBatchItem(bi *models.BatchItem) error { return r.db.Save(bi).Error }

func (r *BatchRepository) BatchItemsForOrderItem(orderItemID uint) ([]models.BatchItem, error) {
	var items []models.BatchItem
	err := r.db.Preload("Batch").Preload("Material").
		Where("order_item_id = ?", orderItemID).Find(&items).Error
	return items, err
}

func (r *BatchRepository) BatchItemsForBatch(batchID uint) ([]models.BatchItem, error) {
	var items []models.BatchItem
	err := r.db.Where("batch_id = ?", batchID).Find(&items).Error
	return items, err
}

// BatchItemStatusesForOrderItems returns, per order item, the statuses of all
// its batch parts — the only inputs the item status roll-up needs. One query
// for any number of items.
func (r *BatchRepository) BatchItemStatusesForOrderItems(orderItemIDs []uint) (map[uint][]models.InternalStatus, error) {
	out := map[uint][]models.InternalStatus{}
	if len(orderItemIDs) == 0 {
		return out, nil
	}
	type row struct {
		OrderItemID uint
		Status      models.InternalStatus
	}
	var rows []row
	err := r.db.Model(&models.BatchItem{}).
		Select("order_item_id, status").
		Where("order_item_id IN ?", orderItemIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.OrderItemID] = append(out[r.OrderItemID], r.Status)
	}
	return out, nil
}

// ExistingMaterialKeys returns the set of order_item_id|material_id pairs already
// scheduled, so the batch creator can skip duplicates instead of failing the unique index.
func (r *BatchRepository) ExistingActiveItemMaterial(orderItemIDs []uint) (map[uint]map[uint]bool, error) {
	out := map[uint]map[uint]bool{}
	if len(orderItemIDs) == 0 {
		return out, nil
	}
	var rows []models.BatchItem
	if err := r.db.Where("order_item_id IN ?", orderItemIDs).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, bi := range rows {
		if out[bi.OrderItemID] == nil {
			out[bi.OrderItemID] = map[uint]bool{}
		}
		out[bi.OrderItemID][bi.MaterialID] = true
	}
	return out, nil
}

// ---------- Status history ----------

type StatusHistoryRepository struct{ db *gorm.DB }

func (r *StatusHistoryRepository) Create(h *models.StatusHistory) error { return r.db.Create(h).Error }

// CreateBulk inserts many history rows in one statement — used by cascades that
// previously wrote one INSERT per affected batch item.
func (r *StatusHistoryRepository) CreateBulk(rows []models.StatusHistory) error {
	if len(rows) == 0 {
		return nil
	}
	return r.db.Create(&rows).Error
}

func (r *StatusHistoryRepository) ListForEntity(entityType models.EntityType, entityID uint) ([]models.StatusHistory, error) {
	var rows []models.StatusHistory
	err := r.db.Where("entity_type = ? AND entity_id = ?", entityType, entityID).
		Order("id asc").Find(&rows).Error
	return rows, err
}

// ---------- QC records ----------

type QCRepository struct{ db *gorm.DB }

func (r *QCRepository) Create(q *models.QCRecord) error { return r.db.Create(q).Error }

func (r *QCRepository) ListForItem(orderItemID uint) ([]models.QCRecord, error) {
	var rows []models.QCRecord
	err := r.db.Where("order_item_id = ?", orderItemID).Order("id desc").Find(&rows).Error
	return rows, err
}
