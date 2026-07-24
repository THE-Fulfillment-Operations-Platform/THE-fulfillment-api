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

// activeBatchItems drops the parts whose order item was cancelled. It filters
// through the OrderItem association join (rather than a hand-written JOIN) so a
// caller can pull the order item's columns in the same statement — the join is
// already there, and joining order_items twice would be a wasted scan.
func activeBatchItems(db *gorm.DB) *gorm.DB {
	return db.Joins("OrderItem").
		Where(`"OrderItem".cancellation_status NOT IN ?`, []models.CancellationStatus{models.CancellationSeller, models.CancellationApproved})
}

func (r *BatchRepository) Create(b *models.Batch) error { return r.db.Create(b).Error }
func (r *BatchRepository) Update(b *models.Batch) error { return r.db.Save(b).Error }

func (r *BatchRepository) CreateItems(items []models.BatchItem) error {
	if len(items) == 0 {
		return nil
	}
	return r.db.Create(&items).Error
}

// FindByID loads the full batch-detail payload. The database is remote, so the
// cost here is the NUMBER of statements, not their individual runtime: every
// belongs-to association is folded into its parent's query with Joins instead of
// costing an extra round trip, leaving three statements (batch, links, items)
// for a flat batch — down from up to eleven.
func (r *BatchRepository) FindByID(id uint) (*models.Batch, error) {
	var b models.Batch
	err := r.db.
		// belongs-to associations ride along in the batch's own SELECT…
		Joins("Material").
		Joins("CreatedBy").
		// …and in each has-many's SELECT, instead of one round trip apiece.
		Preload("Links", func(db *gorm.DB) *gorm.DB { return db.Joins("UpdatedBy").Order("kind asc") }).
		Preload("Items", func(db *gorm.DB) *gorm.DB {
			return activeBatchItems(db).Joins("OrderItem.Order").Joins("Material")
		}).
		// A parent batch preloads its children (with each child's active items so the
		// detail view can show per-child item counts). Children/flat batches have none,
		// and GORM skips the nested preload once the child list comes back empty.
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

// UpdateBatchItemStatuses moves many parts to the same status in one statement.
// The batch cascade used to call UpdateBatchItemStatus in a loop, so a 40-item
// batch paid 40 network round trips to the (remote) database.
func (r *BatchRepository) UpdateBatchItemStatuses(ids []uint, status models.InternalStatus) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.Model(&models.BatchItem{}).Where("id IN ?", ids).Update("status", status).Error
}

// ActiveBatchItemsForBatch returns the batch's parts minus those whose order item
// was cancelled — the exact set a status cascade may touch. The cancellation
// filter rides on the join, so callers no longer need a second lookup.
func (r *BatchRepository) ActiveBatchItemsForBatch(batchID uint) ([]models.BatchItem, error) {
	var items []models.BatchItem
	err := activeBatchItems(r.db).Where("batch_items.batch_id = ?", batchID).Find(&items).Error
	return items, err
}

// LinkKindsForBatch returns which production links a batch has (PRINT / CUT).
// The guard on entering fabrication only needs their presence, not the rows.
func (r *BatchRepository) LinkKindsForBatch(batchID uint) ([]models.BatchLinkKind, error) {
	var kinds []models.BatchLinkKind
	err := r.db.Model(&models.BatchLink{}).Where("batch_id = ?", batchID).Pluck("kind", &kinds).Error
	return kinds, err
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
		Preload("Links").
		Preload("Items", activeBatchItems).
		Order("id desc").
		Limit(f.PageSize).Offset(f.Offset()).Find(&rows).Error
	return rows, total, err
}

// ---------- Batch links (print / cut) ----------

// FindLink returns the live link of a given kind for a batch, or gorm.ErrRecordNotFound.
func (r *BatchRepository) FindLink(batchID uint, kind models.BatchLinkKind) (*models.BatchLink, error) {
	var l models.BatchLink
	if err := r.db.Where("batch_id = ? AND kind = ?", batchID, kind).First(&l).Error; err != nil {
		return nil, err
	}
	return &l, nil
}

func (r *BatchRepository) CreateLink(l *models.BatchLink) error { return r.db.Create(l).Error }
func (r *BatchRepository) UpdateLink(l *models.BatchLink) error { return r.db.Save(l).Error }

// LinksForBatch lists a batch's links (print/cut), with the updater preloaded.
func (r *BatchRepository) LinksForBatch(batchID uint) ([]models.BatchLink, error) {
	var rows []models.BatchLink
	err := r.db.Preload("UpdatedBy").Where("batch_id = ?", batchID).Order("kind asc").Find(&rows).Error
	return rows, err
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
