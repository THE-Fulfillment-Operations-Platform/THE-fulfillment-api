package repositories

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// ---------- Import jobs ----------

type ImportRepository struct{ db *gorm.DB }

func (r *ImportRepository) Create(j *models.ImportJob) error { return r.db.Create(j).Error }
func (r *ImportRepository) Update(j *models.ImportJob) error { return r.db.Save(j).Error }

func (r *ImportRepository) CreateErrors(errs []models.ImportError) error {
	if len(errs) == 0 {
		return nil
	}
	return r.db.Create(&errs).Error
}

func (r *ImportRepository) FindByID(id uint) (*models.ImportJob, error) {
	var j models.ImportJob
	if err := r.db.Preload("Errors").First(&j, id).Error; err != nil {
		return nil, err
	}
	return &j, nil
}

func (r *ImportRepository) List(p Page) ([]models.ImportJob, int64, error) {
	var rows []models.ImportJob
	var total int64
	r.db.Model(&models.ImportJob{}).Count(&total)
	err := r.db.Order("id desc").Limit(p.PageSize).Offset(p.Offset()).Find(&rows).Error
	return rows, total, err
}

// ---------- Orders ----------

// OrderFilter captures the list filters from the wireframe (store, sku, status,
// batch, date range).
type OrderFilter struct {
	Page
	SellerID           *uint
	StoreID            *uint
	SKUCode            string
	SellerStatus       string
	ReviewStatus       string   // exact review_status match
	ReviewStatuses     []string // review_status IN (...) — used by the review queue
	CancellationStatus string
	StoreOrderID       string
	DateFrom           *time.Time
	DateTo             *time.Time
}

type OrderRepository struct{ db *gorm.DB }

func (r *OrderRepository) Create(o *models.Order) error { return r.db.Create(o).Error }
func (r *OrderRepository) Update(o *models.Order) error { return r.db.Save(o).Error }

// SoftDelete soft-deletes an order (sets deleted_at). GORM's default scope hides
// it from every subsequent query; linked rows are preserved.
func (r *OrderRepository) SoftDelete(id uint) error {
	return r.db.Delete(&models.Order{}, id).Error
}

// UpdateTracking writes only the tracking columns (plus updated_at) so it can't
// clobber concurrent edits to unrelated order fields.
func (r *OrderRepository) UpdateTracking(id uint, fields map[string]interface{}) error {
	return r.db.Model(&models.Order{}).Where("id = ?", id).Updates(fields).Error
}

// NextDailySeq atomically allocates the next per-day order sequence
// ("STT trong ngày") for the given business calendar day (YYYY-MM-DD). It upserts
// the daily_counters row and returns the new sequence in a single statement, so
// any number of concurrent order creations each receive a distinct, monotonically
// increasing number without a separate lock round-trip. MUST be called on the
// transaction handle (repositories.New(tx).Order) so the allocation commits with
// the order. Works on both Postgres and the sqlite test engine (both support
// INSERT ... ON CONFLICT DO UPDATE ... RETURNING).
func (r *OrderRepository) NextDailySeq(scopeDate string, now time.Time) (int, error) {
	var seq int
	err := r.db.Raw(
		`INSERT INTO daily_counters (scope_date, seq, updated_at) VALUES (?, 1, ?)
		 ON CONFLICT (scope_date) DO UPDATE SET seq = daily_counters.seq + 1, updated_at = ?
		 RETURNING seq`,
		scopeDate, now, now,
	).Scan(&seq).Error
	return seq, err
}

func (r *OrderRepository) FindByID(id uint) (*models.Order, error) {
	var o models.Order
	err := r.db.
		Preload("Seller").
		Preload("Items", func(db *gorm.DB) *gorm.DB { return db.Order("order_items.line_no asc") }).
		Preload("Items.SKU").
		Preload("Items.BatchItems.Batch").
		Preload("Items.BatchItems.Material").
		First(&o, id).Error
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// FindByIDsForReview loads several orders with only their line items preloaded
// (no Seller / SKU / batch-item preloads). The bulk review path needs just the
// order + item columns its validation reads, so it skips the heavy per-order
// FindByID preload chain: one query for the orders, one for their items — instead
// of ~6 queries per order.
func (r *OrderRepository) FindByIDsForReview(ids []uint) ([]models.Order, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var orders []models.Order
	err := r.db.
		Preload("Items", func(db *gorm.DB) *gorm.DB { return db.Order("order_items.line_no asc") }).
		Where("id IN ?", ids).
		Find(&orders).Error
	return orders, err
}

// BulkSetReviewApproved flips several orders to APPROVED in a single UPDATE,
// stamping the reviewer, note and timestamp. The review_status guard makes it
// safe under concurrency: only orders still pending review / needing correction
// are changed, so a race that already decided an order can't be clobbered.
func (r *OrderRepository) BulkSetReviewApproved(ids []uint, reviewerID *uint, note string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.Model(&models.Order{}).
		Where("id IN ? AND review_status IN ?", ids,
			[]string{string(models.ReviewPending), string(models.ReviewNeedsFix)}).
		Updates(map[string]interface{}{
			"review_status":  models.ReviewApproved,
			"reviewed_by_id": reviewerID,
			"reviewed_at":    at,
			"review_note":    note,
		}).Error
}

func (r *OrderRepository) FindBySellerAndStoreOrder(sellerID uint, storeOrderID string) (*models.Order, error) {
	var o models.Order
	err := r.db.Where("seller_id = ? AND store_order_id = ?", sellerID, storeOrderID).First(&o).Error
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ExistingStoreOrderIDs returns which of the given store order ids already have
// at least one (non-deleted) order for the seller. One query for the whole
// import file, replacing a per-row FindBySellerAndStoreOrder probe.
func (r *OrderRepository) ExistingStoreOrderIDs(sellerID uint, storeOrderIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(storeOrderIDs) == 0 {
		return out, nil
	}
	var ids []string
	err := r.db.Model(&models.Order{}).
		Distinct("store_order_id").
		Where("seller_id = ? AND store_order_id IN ?", sellerID, storeOrderIDs).
		Pluck("store_order_id", &ids).Error
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// UpdateSellerStatusIf atomically advances an order's seller status, but only
// when it still holds the expected current value. The WHERE guard makes the
// transition race-safe: two concurrent workers can't both apply it, and a
// concurrent full-row Save can't be silently overwritten.
func (r *OrderRepository) UpdateSellerStatusIf(orderID uint, from, to models.SellerStatus) (bool, error) {
	res := r.db.Model(&models.Order{}).
		Where("id = ? AND seller_status = ?", orderID, from).
		Update("seller_status", to)
	return res.RowsAffected > 0, res.Error
}

// StoreOrderDupKey builds the lookup key used by DuplicateStoreOrderIDs.
func StoreOrderDupKey(sellerID uint, storeOrderID string) string {
	return fmt.Sprintf("%d|%s", sellerID, storeOrderID)
}

// DuplicateStoreOrderIDs returns the set of (seller_id, store_order_id) pairs —
// keyed via StoreOrderDupKey — that map to MORE THAN ONE order, restricted to the
// given store order ids. It counts across all (non-deleted) orders, not just a
// single page, so the "duplicate" flag is stable regardless of pagination. A store
// order id used by only one order (however many items) is not returned.
func (r *OrderRepository) DuplicateStoreOrderIDs(storeOrderIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(storeOrderIDs) == 0 {
		return out, nil
	}
	type dupRow struct {
		SellerID     uint
		StoreOrderID string
	}
	var rows []dupRow
	err := r.db.Model(&models.Order{}).
		Select("seller_id, store_order_id").
		Where("store_order_id IN ?", storeOrderIDs).
		Group("seller_id, store_order_id").
		Having("COUNT(*) > 1").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[StoreOrderDupKey(row.SellerID, row.StoreOrderID)] = true
	}
	return out, nil
}

func (r *OrderRepository) baseQuery(f OrderFilter) *gorm.DB {
	q := r.db.Model(&models.Order{})
	if f.SellerID != nil {
		q = q.Where("orders.seller_id = ?", *f.SellerID)
	}
	if f.StoreID != nil {
		q = q.Where("orders.store_id = ?", *f.StoreID)
	}
	if f.SellerStatus != "" {
		q = q.Where("orders.seller_status = ?", f.SellerStatus)
	}
	if f.ReviewStatus != "" {
		q = q.Where("orders.review_status = ?", f.ReviewStatus)
	}
	if len(f.ReviewStatuses) > 0 {
		q = q.Where("orders.review_status IN ?", f.ReviewStatuses)
	}
	if f.CancellationStatus != "" {
		q = q.Where("orders.cancellation_status = ?", f.CancellationStatus)
	}
	if f.StoreOrderID != "" {
		q = q.Where("orders.store_order_id ILIKE ?", "%"+f.StoreOrderID+"%")
	}
	if f.DateFrom != nil {
		q = q.Where("orders.created_at >= ?", *f.DateFrom)
	}
	if f.DateTo != nil {
		q = q.Where("orders.created_at <= ?", *f.DateTo)
	}
	if f.SKUCode != "" {
		q = q.Where("orders.id IN (?)",
			r.db.Model(&models.OrderItem{}).Select("order_id").Where("sku_code = ?", f.SKUCode))
	}
	return q
}

func (r *OrderRepository) List(f OrderFilter) ([]models.Order, int64, error) {
	var rows []models.Order
	var total int64
	r.baseQuery(f).Count(&total)
	err := r.baseQuery(f).
		Preload("Seller").
		Preload("Items").
		Order("orders.id desc").
		Limit(f.PageSize).Offset(f.Offset()).Find(&rows).Error
	return rows, total, err
}

// ---------- Order items ----------

// ItemFilter captures item-view filters (store, sku, status, batch, date).
type ItemFilter struct {
	Page
	SellerID       *uint
	StoreID        *uint
	StoreOrderID   string // partial, case-insensitive match on the parent order's store order id
	SKUCode        string
	InternalCode   string // partial, case-insensitive match on the item's internal (QR) code
	InternalStatus string
	DesignStatus   string
	ReviewStatus   string // exact match on the parent order's review_status
	BatchID        *uint
	BatchCode      string // partial, case-insensitive batch code (e.g. #101001)
	MaterialID     *uint  // items whose SKU includes this material (design queue "gom theo NVL")
	IDs            []uint // restrict to specific item ids (e.g. selected rows for a ZIP export)
	NeedsDesign    bool   // design queue: design not ready
	ReviewApproved bool   // only items whose order review_status = APPROVED
	DateFrom       *time.Time
	DateTo         *time.Time
	// Server-side sort. SortBy is whitelisted (see itemOrderClause); anything else
	// falls back to the stable default (newest first). SortDir is asc|desc.
	SortBy  string
	SortDir string
}

// itemOrderClause maps a whitelisted sort key + direction to a safe SQL ORDER BY.
// Every branch appends a deterministic tiebreaker so pagination stays stable when
// the primary key has duplicates (e.g. many items share a SKU). Unknown keys fall
// back to newest-first — this is the only place item sort SQL is built, so a
// client-supplied value can never inject into the query.
func itemOrderClause(sortBy, sortDir string) string {
	dir := "DESC"
	if strings.EqualFold(sortDir, "asc") {
		dir = "ASC"
	}
	switch strings.ToLower(strings.TrimSpace(sortBy)) {
	case "sku", "sku_code":
		return "order_items.sku_code " + dir + ", order_items.id DESC"
	case "quantity", "qty":
		return "order_items.quantity " + dir + ", order_items.id DESC"
	case "created_at", "date":
		return "order_items.created_at " + dir + ", order_items.id DESC"
	case "stt", "daily_seq":
		return "orders.order_date " + dir + ", orders.daily_seq " + dir + ", order_items.id DESC"
	case "internal_code":
		return "order_items.internal_code " + dir + ", order_items.id DESC"
	case "batch", "batch_code":
		// An item can belong to several material batches. MIN(code) gives it one
		// stable grouping key; unbatched items are placed after batched items in
		// both directions so the production groups remain the first thing shown.
		batchKey := `(SELECT MIN(b.code)
			FROM batch_items bi
			JOIN batches b ON b.id = bi.batch_id AND b.deleted_at IS NULL
			WHERE bi.order_item_id = order_items.id AND bi.deleted_at IS NULL)`
		return "CASE WHEN " + batchKey + " IS NULL THEN 1 ELSE 0 END ASC, " +
			batchKey + " " + dir + ", order_items.id DESC"
	default:
		return "order_items.id DESC"
	}
}

type OrderItemRepository struct{ db *gorm.DB }

func (r *OrderItemRepository) Update(i *models.OrderItem) error { return r.db.Save(i).Error }

func (r *OrderItemRepository) FindByID(id uint) (*models.OrderItem, error) {
	var it models.OrderItem
	err := r.db.
		Preload("Order.Seller").
		Preload("SKU.Materials.Material").
		Preload("BatchItems.Batch").
		Preload("BatchItems.Material").
		Preload("Assets").
		First(&it, id).Error
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func (r *OrderItemRepository) FindByCode(code string) (*models.OrderItem, error) {
	var it models.OrderItem
	err := r.db.
		Preload("Order.Seller").
		Preload("SKU.Materials.Material").
		Preload("BatchItems.Batch").
		Preload("BatchItems.Material").
		Where("internal_code = ?", code).First(&it).Error
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// FindForBatching bulk-loads items with exactly the associations batch creation
// checks (order review status, SKU material set) — one query set for the whole
// selection instead of a fully-preloaded FindByID per item.
func (r *OrderItemRepository) FindForBatching(ids []uint) (map[uint]*models.OrderItem, error) {
	out := map[uint]*models.OrderItem{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []models.OrderItem
	err := r.db.
		Preload("Order").
		Preload("SKU.Materials").
		Where("id IN ?", ids).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	for i := range rows {
		out[rows[i].ID] = &rows[i]
	}
	return out, nil
}

// InternalStatusByIDs returns the current internal status per item, for the
// status roll-up recompute (no associations needed).
func (r *OrderItemRepository) InternalStatusByIDs(ids []uint) (map[uint]models.InternalStatus, error) {
	out := map[uint]models.InternalStatus{}
	if len(ids) == 0 {
		return out, nil
	}
	type row struct {
		ID             uint
		InternalStatus models.InternalStatus
	}
	var rows []row
	err := r.db.Model(&models.OrderItem{}).
		Select("id, internal_status").
		Where("id IN ?", ids).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = r.InternalStatus
	}
	return out, nil
}

// UpdateInternalStatus writes only the derived internal_status column (plus
// updated_at). Deliberately not a full Save so a roll-up can never clobber
// concurrent edits to other fields.
func (r *OrderItemRepository) UpdateInternalStatus(id uint, status models.InternalStatus) error {
	return r.db.Model(&models.OrderItem{}).Where("id = ?", id).
		Update("internal_status", status).Error
}

// UpdateInternalStatuses sets the same derived status on many items in one
// statement. The roll-up groups items by their new status and calls this once
// per distinct status (at most four) instead of once per item — the database is
// remote, so statements, not rows, are what cost time.
func (r *OrderItemRepository) UpdateInternalStatuses(ids []uint, status models.InternalStatus) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.Model(&models.OrderItem{}).Where("id IN ?", ids).
		Update("internal_status", status).Error
}

func (r *OrderItemRepository) ListCancellationRequests(p Page) ([]models.OrderItem, int64, error) {
	var rows []models.OrderItem
	var total int64
	q := r.db.Model(&models.OrderItem{}).Where("order_items.cancellation_status = ?", models.CancellationRequested)
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := q.Preload("Order.Seller").Order("order_items.cancellation_requested_at asc").Limit(p.PageSize).Offset(p.Offset()).Find(&rows).Error
	return rows, total, err
}

func (r *OrderItemRepository) baseQuery(f ItemFilter) *gorm.DB {
	q := r.db.Model(&models.OrderItem{}).
		Joins("JOIN orders ON orders.id = order_items.order_id AND orders.deleted_at IS NULL").
		// This is the operational item list used by Orders/Items, design, batch,
		// QC and packing. Cancelled lines are historical records, never work.
		Where("order_items.cancellation_status NOT IN ?", []models.CancellationStatus{models.CancellationSeller, models.CancellationApproved})
	if f.SellerID != nil {
		q = q.Where("orders.seller_id = ?", *f.SellerID)
	}
	if f.StoreID != nil {
		q = q.Where("orders.store_id = ?", *f.StoreID)
	}
	if f.StoreOrderID != "" {
		q = q.Where("orders.store_order_id ILIKE ?", "%"+f.StoreOrderID+"%")
	}
	if f.SKUCode != "" {
		q = q.Where("order_items.sku_code = ?", f.SKUCode)
	}
	if f.InternalCode != "" {
		q = q.Where("order_items.internal_code ILIKE ?", "%"+f.InternalCode+"%")
	}
	if f.InternalStatus != "" {
		q = q.Where("order_items.internal_status = ?", f.InternalStatus)
	}
	if f.DesignStatus != "" {
		q = q.Where("order_items.design_status = ?", f.DesignStatus)
	}
	if f.ReviewStatus != "" {
		q = q.Where("orders.review_status = ?", f.ReviewStatus)
	}
	if f.NeedsDesign {
		// Design work is outstanding in two cases: the item itself is unfinished, or
		// it already sits in a batch that is still missing a production file. Files
		// are ganged per batch (many designs on one sheet) and attached to the batch,
		// so reaching READY does not mean an item is done — its batch still owes both
		// a print file AND a cut file (every product here needs both). Keeping those
		// rows in the queue is what lets a designer filter by batch/material and clear
		// them a batch at a time; they drop off on their own the moment the batch has
		// both links. Grouped so the OR cannot leak into the surrounding AND chain.
		awaitingBatchFiles := r.db.Model(&models.BatchItem{}).
			Select("batch_items.order_item_id").
			Joins("JOIN batches ON batches.id = batch_items.batch_id AND batches.deleted_at IS NULL").
			Joins("LEFT JOIN batch_links bl_print ON bl_print.batch_id = batches.id AND bl_print.kind = ? AND bl_print.deleted_at IS NULL",
				models.BatchLinkPrint).
			Joins("LEFT JOIN batch_links bl_cut ON bl_cut.batch_id = batches.id AND bl_cut.kind = ? AND bl_cut.deleted_at IS NULL",
				models.BatchLinkCut).
			Where("bl_print.id IS NULL OR bl_cut.id IS NULL")

		q = q.Where(
			r.db.Where("order_items.design_status IN ?", []string{
				string(models.DesignPending), string(models.DesignInProgress), string(models.DesignMissing),
			}).Or("order_items.id IN (?)", awaitingBatchFiles),
		)
	}
	if f.ReviewApproved {
		q = q.Where("orders.review_status = ?", string(models.ReviewApproved)).
			Where("order_items.cancellation_status NOT IN ?", []models.CancellationStatus{models.CancellationSeller, models.CancellationApproved})
	}
	if f.BatchID != nil {
		q = q.Where("order_items.id IN (?)",
			r.db.Model(&models.BatchItem{}).Select("order_item_id").Where("batch_id = ?", *f.BatchID))
	}
	if f.BatchCode != "" {
		q = q.Where("order_items.id IN (?)",
			r.db.Model(&models.BatchItem{}).
				Select("batch_items.order_item_id").
				Joins("JOIN batches ON batches.id = batch_items.batch_id AND batches.deleted_at IS NULL").
				Where("batches.code ILIKE ?", "%"+f.BatchCode+"%"))
	}
	if f.MaterialID != nil {
		// "Gom theo NVL": keep items whose SKU is mapped to this material, so the
		// designer sees every order that could go onto one material's sheet before
		// creating a batch. Matched via the SKU→material mapping, not the batch, so
		// it works for items not yet batched.
		q = q.Where("order_items.sku_id IN (?)",
			r.db.Table("sku_materials").
				Select("sku_materials.sku_id").
				Where("sku_materials.material_id = ? AND sku_materials.deleted_at IS NULL", *f.MaterialID))
	}
	if len(f.IDs) > 0 {
		q = q.Where("order_items.id IN ?", f.IDs)
	}
	if f.DateFrom != nil {
		q = q.Where("order_items.created_at >= ?", *f.DateFrom)
	}
	if f.DateTo != nil {
		q = q.Where("order_items.created_at <= ?", *f.DateTo)
	}
	return q
}

func (r *OrderItemRepository) List(f ItemFilter) ([]models.OrderItem, int64, error) {
	var rows []models.OrderItem
	var total int64
	r.baseQuery(f).Count(&total)
	err := r.baseQuery(f).
		Preload("Order.Seller").
		Preload("SKU.Materials.Material").
		Preload("BatchItems.Batch").
		Preload("BatchItems.Material").
		Order(itemOrderClause(f.SortBy, f.SortDir)).
		Limit(f.PageSize).Offset(f.Offset()).Find(&rows).Error
	return rows, total, err
}

// ListAll returns every item matching the filter, ignoring pagination. Used by
// bulk exports (e.g. the design-queue asset ZIP) that must cover the whole set,
// not a single page.
func (r *OrderItemRepository) ListAll(f ItemFilter) ([]models.OrderItem, error) {
	var rows []models.OrderItem
	err := r.baseQuery(f).
		Preload("Order.Seller").
		Preload("SKU").
		Order("order_items.id desc").
		Find(&rows).Error
	return rows, err
}

// SetProductionFileForBatch stamps one production-file column (print_file_url or
// cut_file_url) onto every item scheduled into the given batch, in one statement.
// It is how a batch-level print/cut link fans out to the per-item columns that the
// production export and QC screens read, so "same batch → same print/cut file"
// holds without the link ever being re-entered per item. Cancelled lines are left
// alone — they are never produced. `column` is caller-supplied and must stay a
// fixed literal, never user input.
func (r *OrderItemRepository) SetProductionFileForBatch(batchID uint, column, url string) (int64, error) {
	res := r.db.Model(&models.OrderItem{}).
		Where("id IN (?)",
			r.db.Model(&models.BatchItem{}).Select("order_item_id").Where("batch_id = ?", batchID)).
		Where("cancellation_status NOT IN ?",
			[]models.CancellationStatus{models.CancellationSeller, models.CancellationApproved}).
		Update(column, url)
	return res.RowsAffected, res.Error
}

// MaterialBucket groups design-ready, not-yet-batched item parts by material so
// the "material buckets" panel on the create-batch screen can be built.
type MaterialBucket struct {
	MaterialID   uint   `json:"material_id"`
	MaterialCode string `json:"material_code"`
	MaterialName string `json:"material_name"`
	ItemCount    int64  `json:"item_count"`
}

// designReadyUnbatchedSubquery returns (order_item_id, material_id) pairs for
// design-ready items whose (item, material) is not yet scheduled into any batch.
func (r *OrderItemRepository) designReadyUnbatchedSubquery() *gorm.DB {
	return r.db.Table("order_items oi").
		Select("oi.id AS order_item_id, oi.sku_code AS sku_code, sm.material_id AS material_id, m.code AS material_code, m.name AS material_name").
		Joins("JOIN orders o ON o.id = oi.order_id AND o.deleted_at IS NULL").
		Joins("JOIN sku_materials sm ON sm.sku_id = oi.sku_id").
		Joins("JOIN materials m ON m.id = sm.material_id").
		Where("oi.deleted_at IS NULL").
		Where("o.review_status = ?", models.ReviewApproved).
		Where("oi.cancellation_status NOT IN ?", []models.CancellationStatus{models.CancellationSeller, models.CancellationApproved}).
		Where("oi.design_status = ?", models.DesignReady).
		Where("NOT EXISTS (?)",
			r.db.Table("batch_items bi").
				Select("1").
				Where("bi.order_item_id = oi.id AND bi.material_id = sm.material_id AND bi.deleted_at IS NULL"))
}

// MaterialBuckets returns the count of design-ready, unbatched item parts per material.
// DesignQueueMaterials lists only the materials that actually have work sitting in
// the design queue, with how many items each covers. The NVL filter is built from
// this rather than the full catalog, which is mostly materials with nothing in the
// queue — picking one of those just returned an empty table. Callers pass the
// queue's own filter with MaterialID cleared, so the facet reflects the other active
// filters (e.g. batch) but never narrows by itself.
func (r *OrderItemRepository) DesignQueueMaterials(f ItemFilter) ([]MaterialBucket, error) {
	var rows []MaterialBucket
	err := r.baseQuery(f).
		Joins("JOIN sku_materials sm ON sm.sku_id = order_items.sku_id AND sm.deleted_at IS NULL").
		Joins("JOIN materials m ON m.id = sm.material_id AND m.deleted_at IS NULL").
		Select("m.id AS material_id, m.code AS material_code, m.name AS material_name, " +
			"COUNT(DISTINCT order_items.id) AS item_count").
		Group("m.id, m.code, m.name").
		Order("m.code ASC").
		Scan(&rows).Error
	return rows, err
}

func (r *OrderItemRepository) MaterialBuckets() ([]MaterialBucket, error) {
	var rows []MaterialBucket
	err := r.db.Table("(?) AS sub", r.designReadyUnbatchedSubquery()).
		Select("sub.material_id, sub.material_code, sub.material_name, COUNT(*) AS item_count").
		Group("sub.material_id, sub.material_code, sub.material_name").
		Order("item_count desc").
		Scan(&rows).Error
	return rows, err
}

// DesignReadyItemsForMaterial lists design-ready items that still need a batch for
// the given material (used to populate the create-batch item table). Count and
// page both run in SQL so the working set never has to fit in memory. sortBy/sortDir
// (whitelisted) let the create-batch table sort by SKU before selecting; the same
// order is applied to the page selection and the final hydrate so it is stable
// across pagination.
func (r *OrderItemRepository) DesignReadyItemsForMaterial(materialID uint, p Page, sortBy, sortDir string) ([]models.OrderItem, int64, error) {
	sub := r.designReadyUnbatchedSubquery().Where("sm.material_id = ?", materialID)

	var total int64
	if err := r.db.Table("(?) AS sub", sub).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []models.OrderItem{}, 0, nil
	}

	dir := "ASC"
	if strings.EqualFold(sortDir, "desc") {
		dir = "DESC"
	}
	subOrder := "sub.order_item_id ASC"
	finalOrder := "id ASC"
	if strings.EqualFold(strings.TrimSpace(sortBy), "sku") || strings.EqualFold(strings.TrimSpace(sortBy), "sku_code") {
		subOrder = "sub.sku_code " + dir + ", sub.order_item_id ASC"
		finalOrder = "sku_code " + dir + ", id ASC"
	}

	var pageIDs []uint
	err := r.db.Table("(?) AS sub", sub).
		Select("sub.order_item_id").
		Order(subOrder).
		Limit(p.PageSize).Offset(p.Offset()).
		Scan(&pageIDs).Error
	if err != nil {
		return nil, 0, err
	}
	if len(pageIDs) == 0 {
		return []models.OrderItem{}, total, nil
	}

	var rows []models.OrderItem
	err = r.db.Preload("Order.Seller").Preload("SKU.Materials.Material").
		Where("id IN ?", pageIDs).Order(finalOrder).Find(&rows).Error
	return rows, total, err
}
