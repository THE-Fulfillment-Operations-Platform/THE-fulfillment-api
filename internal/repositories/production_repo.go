package repositories

import (
	"strings"
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
	// ExcludeClosed drops batches whose production is over because every piece they
	// made was scrapped at QC. The production board sets it — there is no work left
	// in such a batch — while the batch list keeps showing them (greyed, with the
	// reason) so the history stays auditable.
	ExcludeClosed bool
	// Code scopes the list to the batches producing one order: it matches the
	// order's internal code ("100047") or one item's tem code ("100047_1/1") —
	// exact match, these codes are system-generated. Items live in child/flat
	// batches, so when Code is set the default hide-children rule is lifted and
	// the actual batch holding the product is listed.
	Code string
}

type BatchRepository struct{ db *gorm.DB }

// activeBatchItems drops the parts whose order item was cancelled. It filters
// through the OrderItem association join (rather than a hand-written JOIN) so a
// caller can pull the order item's columns in the same statement — the join is
// already there, and joining order_items twice would be a wasted scan.
// activeBatchItems scopes to parts that still count: the order item is not
// cancelled, and the part was not scrapped after a QC fail. A scrapped part stays
// in its batch as the record of what was actually produced, but it must never
// hold a batch back — the batch that made a defective piece still finishes.
//
// The order item's SKU rides along in the same statement: its size (with the
// material's) is what the batch's sheet count is derived from.
func activeBatchItems(db *gorm.DB) *gorm.DB {
	return db.Joins("OrderItem").Joins("OrderItem.SKU").
		Where(`"OrderItem".cancellation_status NOT IN ?`, []models.CancellationStatus{models.CancellationSeller, models.CancellationApproved}).
		Where("batch_items.scrapped_at IS NULL")
}

// attachPairQuotas loads the DECLARED quota of every (item SKU, batch material)
// pair the given batches (and their loaded children) hold, in ONE statement,
// and hangs it on each item's SKU as its Materials — so FillMaterialUnits sees
// the number the factory typed instead of the size estimate. The pair rows
// can't ride in the items' SELECT (a has-many), hence the separate query; one
// per list / detail call, never one per item.
func (r *BatchRepository) attachPairQuotas(batches []*models.Batch) error {
	type target struct {
		sku   *models.SKU
		matID uint
	}
	var targets []target
	skuSet := map[uint]bool{}
	matSet := map[uint]bool{}
	collect := func(b *models.Batch) {
		for i := range b.Items {
			it := b.Items[i].OrderItem
			if it == nil || it.SKU == nil {
				continue
			}
			targets = append(targets, target{sku: it.SKU, matID: b.MaterialID})
			skuSet[it.SKU.ID] = true
			matSet[b.MaterialID] = true
		}
	}
	for _, b := range batches {
		collect(b)
		for i := range b.ChildBatches {
			collect(&b.ChildBatches[i])
		}
	}
	if len(targets) == 0 {
		return nil
	}
	skuIDs := make([]uint, 0, len(skuSet))
	for id := range skuSet {
		skuIDs = append(skuIDs, id)
	}
	matIDs := make([]uint, 0, len(matSet))
	for id := range matSet {
		matIDs = append(matIDs, id)
	}
	var rows []models.SKUMaterial
	if err := r.db.Where("sku_id IN ? AND material_id IN ?", skuIDs, matIDs).Find(&rows).Error; err != nil {
		return err
	}
	byPair := map[[2]uint]models.SKUMaterial{}
	for _, row := range rows {
		byPair[[2]uint{row.SKUID, row.MaterialID}] = row
	}
	for _, t := range targets {
		if row, ok := byPair[[2]uint{t.sku.ID, t.matID}]; ok {
			t.sku.Materials = []models.SKUMaterial{row}
		}
	}
	return nil
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
			// Seller rides along for the QR label print (tem in tên seller).
			return activeBatchItems(db).Joins("OrderItem.Order").Joins("OrderItem.Order.Seller").Joins("Material")
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
	// Số phần đã huỷ: màn chi tiết phải nói được "0 còn lại · 2 đã huỷ" thay vì
	// hiện một batch trông như trống rỗng. Danh sách đã điền sẵn trường này; chi
	// tiết thì chưa, nên batch vừa bị huỷ mở ra không giải thích được gì.
	if scrapped, err := r.ScrappedCounts([]uint{b.ID}); err == nil {
		b.ScrappedCount = scrapped[b.ID]
	}
	if err := r.attachPairQuotas([]*models.Batch{&b}); err != nil {
		return nil, err
	}
	b.FillMaterialUnits(&b.Material)
	// A parent's sheets are the sum of its children's (children are loaded with
	// their live parts here, so the sum is exact). Any child without a quota
	// leaves the parent at its child count — one sheet per child.
	total := 0
	exact := true
	for i := range b.ChildBatches {
		b.ChildBatches[i].FillMaterialUnits(&b.Material)
		if u := b.ChildBatches[i].MaterialUnits; u != nil {
			total += *u
		} else {
			exact = false
		}
	}
	if b.IsParent && exact && len(b.ChildBatches) > 0 {
		b.MaterialUnits = &total
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

// BatchIDsForOrder returns the distinct batches holding any line of an order.
// Cancelling an order takes its parts out of every batch it was scheduled into,
// and each of those batches then has to re-derive its own status from what is
// left — this is how the caller learns which ones to roll up. Call it BEFORE the
// lines are marked cancelled: afterwards the join no longer finds them.
func (r *BatchRepository) BatchIDsForOrder(orderID uint) ([]uint, error) {
	var ids []uint
	err := r.db.Model(&models.BatchItem{}).
		Distinct("batch_items.batch_id").
		Joins("JOIN order_items ON order_items.id = batch_items.order_item_id").
		Where("order_items.order_id = ?", orderID).
		Pluck("batch_items.batch_id", &ids).Error
	return ids, err
}

// BatchIDsForOrderItem is the single-line counterpart of BatchIDsForOrder, for a
// per-product cancellation.
func (r *BatchRepository) BatchIDsForOrderItem(itemID uint) ([]uint, error) {
	var ids []uint
	err := r.db.Model(&models.BatchItem{}).
		Distinct("batch_id").
		Where("order_item_id = ?", itemID).
		Pluck("batch_id", &ids).Error
	return ids, err
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
	if f.ExcludeClosed {
		q = q.Where("closed_at IS NULL")
	}
	if code := strings.TrimSpace(f.Code); code != "" {
		q = q.Where("batches.id IN (?)", r.db.Model(&models.BatchItem{}).
			Select("batch_items.batch_id").
			Joins("JOIN order_items ON order_items.id = batch_items.order_item_id").
			Joins("JOIN orders ON orders.id = order_items.order_id").
			Where("orders.internal_code = ? OR order_items.internal_code = ?", code, code))
	}
	// Child-scoping: with a parent id, list only that parent's children; otherwise
	// hide children so the default list shows parent + flat batches only — except
	// when searching by code, where the hit IS a child/flat batch and must show.
	if f.ParentBatchID != nil {
		q = q.Where("parent_batch_id = ?", *f.ParentBatchID)
	} else if f.Code == "" {
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
	if err != nil {
		return rows, total, err
	}
	// A batch can legitimately show zero items — every piece it made was scrapped
	// at QC. Ship the scrapped count alongside so the screen says why instead of
	// looking broken.
	ids := make([]uint, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	scrapped, err := r.ScrappedCounts(ids)
	if err != nil {
		return rows, total, err
	}
	ptrs := make([]*models.Batch, 0, len(rows))
	for i := range rows {
		ptrs = append(ptrs, &rows[i])
	}
	if err := r.attachPairQuotas(ptrs); err != nil {
		return rows, total, err
	}
	for i := range rows {
		rows[i].ScrappedCount = scrapped[rows[i].ID]
		rows[i].FillMaterialUnits(&rows[i].Material)
	}
	return rows, total, nil
}

// ---------- Batch links (print / cut) ----------

// FindLink returns the live link of a given kind for a batch, or gorm.ErrRecordNotFound.
// PendingLinkTargets lists the batches a designer can still attach production
// files to: PENDING, not closed, and holding items themselves (flat or child —
// never a parent, which aggregates children and carries no files). Ordered by
// id so the exported sheet is stable between downloads.
func (r *BatchRepository) PendingLinkTargets() ([]models.Batch, error) {
	var rows []models.Batch
	err := r.db.Preload("Material").Preload("Links").
		Where("is_parent = ? AND closed_at IS NULL AND status = ?", false, models.StatusPending).
		Order("id").Find(&rows).Error
	return rows, err
}

// LinkImportRefs loads the batches an Excel import references — material and
// links preloaded — keyed by id so file rows resolve by identifier, never by
// position.
func (r *BatchRepository) LinkImportRefs(ids []uint) (map[uint]*models.Batch, error) {
	ids = dedupeIDs(ids)
	out := make(map[uint]*models.Batch, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []models.Batch
	if err := r.db.Preload("Material").Preload("Links").Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		out[rows[i].ID] = &rows[i]
	}
	return out, nil
}

// BatchLiveCounts tallies one batch's live parts for the link export/preview.
type BatchLiveCounts struct {
	BatchID  uint
	Items    int
	Products int
}

// LiveItemProductCounts counts, per batch, the parts that still count — not
// scrapped, order item not cancelled — and their product total (Σ quantity,
// each item at least 1).
func (r *BatchRepository) LiveItemProductCounts(ids []uint) (map[uint]BatchLiveCounts, error) {
	ids = dedupeIDs(ids)
	out := make(map[uint]BatchLiveCounts, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []BatchLiveCounts
	err := r.db.Model(&models.BatchItem{}).
		Select("batch_items.batch_id AS batch_id, COUNT(*) AS items, "+
			"SUM(CASE WHEN order_items.quantity < 1 THEN 1 ELSE order_items.quantity END) AS products").
		Joins("JOIN order_items ON order_items.id = batch_items.order_item_id AND order_items.deleted_at IS NULL").
		Where("batch_items.batch_id IN ? AND batch_items.scrapped_at IS NULL", ids).
		Where("order_items.cancellation_status NOT IN ?",
			[]models.CancellationStatus{models.CancellationSeller, models.CancellationApproved}).
		Group("batch_items.batch_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.BatchID] = row
	}
	return out, nil
}

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

// LiveBatchItemsForOrderItem returns only the parts still in play (not scrapped),
// newest attempt included — what QC scans and the status roll-up work on.
func (r *BatchRepository) LiveBatchItemsForOrderItem(orderItemID uint) ([]models.BatchItem, error) {
	var items []models.BatchItem
	err := r.db.Preload("Batch").Preload("Material").
		Where("order_item_id = ? AND scrapped_at IS NULL", orderItemID).Find(&items).Error
	return items, err
}

// NextAttempt returns the attempt number a new production part for this
// (item, material) must carry: one past the highest ever used, so re-making a
// piece never collides with the row that recorded the failed one.
func (r *BatchRepository) NextAttempt(orderItemID, materialID uint) (int, error) {
	var maxAttempt *int
	err := r.db.Model(&models.BatchItem{}).
		Where("order_item_id = ? AND material_id = ?", orderItemID, materialID).
		Select("MAX(attempt)").Scan(&maxAttempt).Error
	if err != nil {
		return 0, err
	}
	if maxAttempt == nil {
		return 1, nil
	}
	return *maxAttempt + 1, nil
}

// NextAttempts returns, per order item, the attempt number a NEW part for this
// material must carry (highest used + 1, or 1 if never produced). One query for
// a whole batch instead of a lookup per item.
func (r *BatchRepository) NextAttempts(materialID uint, orderItemIDs []uint) (map[uint]int, error) {
	out := map[uint]int{}
	if len(orderItemIDs) == 0 {
		return out, nil
	}
	type row struct {
		OrderItemID uint
		MaxAttempt  int
	}
	var rows []row
	err := r.db.Model(&models.BatchItem{}).
		Select("order_item_id, MAX(attempt) AS max_attempt").
		Where("material_id = ? AND order_item_id IN ?", materialID, orderItemIDs).
		Group("order_item_id").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.OrderItemID] = x.MaxAttempt + 1
	}
	return out, nil
}

// CloseIfNothingLeft closes a batch that still has parts on paper but none that
// will ever be produced — every one of them was scrapped at QC. Returns whether
// it closed the batch. Idempotent: a batch already closed, or one that still has
// live parts, is left alone. A batch with NO parts at all (freshly created, items
// not attached yet) is also left alone — that is a different situation.
func (r *BatchRepository) CloseIfNothingLeft(batchID uint, reason string, at time.Time) (bool, error) {
	var total, live int64
	if err := r.db.Model(&models.BatchItem{}).Where("batch_id = ?", batchID).Count(&total).Error; err != nil {
		return false, err
	}
	if total == 0 {
		return false, nil
	}
	if err := r.db.Model(&models.BatchItem{}).
		Where("batch_id = ? AND scrapped_at IS NULL", batchID).Count(&live).Error; err != nil {
		return false, err
	}
	if live > 0 {
		return false, nil
	}
	res := r.db.Model(&models.Batch{}).
		Where("id = ? AND closed_at IS NULL", batchID).
		Updates(map[string]any{"closed_at": at, "close_reason": reason})
	return res.RowsAffected > 0, res.Error
}

// ScrappedCounts returns how many parts each batch has written off, so a list can
// explain a batch that shows zero remaining items. One grouped query per page.
func (r *BatchRepository) ScrappedCounts(batchIDs []uint) (map[uint]int, error) {
	out := map[uint]int{}
	if len(batchIDs) == 0 {
		return out, nil
	}
	type row struct {
		BatchID uint
		N       int
	}
	var rows []row
	err := r.db.Model(&models.BatchItem{}).
		Select("batch_id, COUNT(*) AS n").
		Where("batch_id IN ? AND scrapped_at IS NOT NULL", batchIDs).
		Group("batch_id").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.BatchID] = x.N
	}
	return out, nil
}

// ScrapBatchItem writes a part off after a QC fail.
func (r *BatchRepository) ScrapBatchItem(id uint, reason string, byID *uint, at time.Time) error {
	return r.db.Model(&models.BatchItem{}).Where("id = ?", id).Updates(map[string]any{
		"scrapped_at":    at,
		"scrap_reason":   reason,
		"scrapped_by_id": byID,
	}).Error
}

// BatchItemsForBatch returns the parts that still count for the batch's status —
// scrapped ones are excluded, otherwise a batch with one QC-failed piece could
// never reach QC_PASSED and would sit on the production board forever. Parts
// whose order line was cancelled drop out for the same reason: nobody is going to
// print a cancelled product, so leaving it in would pin the batch at PENDING.
func (r *BatchRepository) BatchItemsForBatch(batchID uint) ([]models.BatchItem, error) {
	var items []models.BatchItem
	err := activeBatchItems(r.db).
		Where("batch_items.batch_id = ? AND batch_items.scrapped_at IS NULL", batchID).
		Find(&items).Error
	return items, err
}

// AllBatchItemsForBatch returns every part INCLUDING scrapped ones — for the
// batch detail screen, which must still show what was made and written off.
func (r *BatchRepository) AllBatchItemsForBatch(batchID uint) ([]models.BatchItem, error) {
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
		// Scrapped parts are history, not work in progress: an item whose failed
		// part was written off is back to "needs producing", not stuck at CUT.
		Where("order_item_id IN ? AND scrapped_at IS NULL", orderItemIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.OrderItemID] = append(out[r.OrderItemID], r.Status)
	}
	return out, nil
}

// BatchItemStatusesForBatches is the batch-side mirror of the above: per batch,
// the statuses of the parts that still count. One query for any number of
// batches, so a roll-up over several batches at once stops costing a query each.
func (r *BatchRepository) BatchItemStatusesForBatches(batchIDs []uint) (map[uint][]models.InternalStatus, error) {
	out := map[uint][]models.InternalStatus{}
	if len(batchIDs) == 0 {
		return out, nil
	}
	type row struct {
		BatchID uint
		Status  models.InternalStatus
	}
	var rows []row
	err := activeBatchItems(r.db).Model(&models.BatchItem{}).
		Select("batch_items.batch_id, batch_items.status").
		Where("batch_items.batch_id IN ?", batchIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.BatchID] = append(out[r.BatchID], r.Status)
	}
	return out, nil
}

// FindLiteMany is FindLite for a set: the bare batch rows (no associations) a
// roll-up needs to compare current status and find parents, in one query.
func (r *BatchRepository) FindLiteMany(ids []uint) ([]models.Batch, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []models.Batch
	err := r.db.Where("id IN ?", ids).Find(&rows).Error
	return rows, err
}

// UpdateStatusColumns is UpdateStatusColumn for every batch landing on the same
// status — one statement instead of one per batch.
func (r *BatchRepository) UpdateStatusColumns(ids []uint, status models.InternalStatus) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.Model(&models.Batch{}).Where("id IN ?", ids).Update("status", status).Error
}

// ChildBatchStatusesFor returns, per parent batch, its children's statuses — the
// input to the parent roll-up, for many parents in one query.
func (r *BatchRepository) ChildBatchStatusesFor(parentIDs []uint) (map[uint][]models.InternalStatus, error) {
	out := map[uint][]models.InternalStatus{}
	if len(parentIDs) == 0 {
		return out, nil
	}
	type row struct {
		ParentBatchID uint
		Status        models.InternalStatus
	}
	var rows []row
	err := r.db.Model(&models.Batch{}).
		Select("parent_batch_id, status").
		Where("parent_batch_id IN ?", parentIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ParentBatchID] = append(out[r.ParentBatchID], r.Status)
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
	// scrapped_at IS NULL — a part written off after a QC fail is NOT an active
	// schedule. Counting it here was what made a re-made product look "already
	// batched" and kept it out of every new batch, while the create-batch picker
	// (which ignores scrapped parts) happily offered it: the two disagreed and the
	// user got "no eligible items" for an item the screen had just listed.
	if err := r.db.Where("order_item_id IN ? AND scrapped_at IS NULL", orderItemIDs).Find(&rows).Error; err != nil {
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

// ---------- Batch delete ----------

// StartedPartCount counts parts across the given batches that production has
// already touched — status beyond PENDING, or written off at QC. The delete
// guard refuses while this is non-zero: a deleted batch must never take the
// record of produced (or scrapped) physical goods with it. Cancelled lines are
// counted too — their part can only have advanced if production ran.
func (r *BatchRepository) StartedPartCount(batchIDs []uint) (int64, error) {
	var n int64
	err := r.db.Model(&models.BatchItem{}).
		Where("batch_id IN ? AND (status <> ? OR scrapped_at IS NOT NULL)", batchIDs, models.StatusPending).
		Count(&n).Error
	return n, err
}

// OrderItemIDsForBatches returns the distinct order items scheduled into any of
// the given batches. Collect it BEFORE HardDeleteBatchItems — afterwards the
// mapping is gone and the caller can no longer learn which items to roll up.
func (r *BatchRepository) OrderItemIDsForBatches(batchIDs []uint) ([]uint, error) {
	var ids []uint
	err := r.db.Model(&models.BatchItem{}).
		Distinct("order_item_id").
		Where("batch_id IN ?", batchIDs).
		Pluck("order_item_id", &ids).Error
	return ids, err
}

// HardDeleteBatchItems removes the batches' parts for real (Unscoped), not via
// soft delete. idx_item_material_attempt has no deleted_at predicate, so a
// soft-deleted row keeps occupying the (item, material, attempt) slot while
// NextAttempts — which never sees soft-deleted rows — hands the next batch the
// same attempt number: re-batching the item would then die on a duplicate key.
func (r *BatchRepository) HardDeleteBatchItems(batchIDs []uint) error {
	return r.db.Unscoped().Where("batch_id IN ?", batchIDs).Delete(&models.BatchItem{}).Error
}

// SoftDeleteLinks removes the batches' print/cut links. Soft delete is safe
// here: idx_batch_link_kind is partial on deleted_at IS NULL.
func (r *BatchRepository) SoftDeleteLinks(batchIDs []uint) error {
	return r.db.Where("batch_id IN ?", batchIDs).Delete(&models.BatchLink{}).Error
}

// SoftDeleteBatches removes the batch headers. Soft delete keeps the row (and
// its code) findable for audit; codes derive from the ID sequence, so a deleted
// code is never reissued and the unique index never collides.
func (r *BatchRepository) SoftDeleteBatches(ids []uint) error {
	return r.db.Where("id IN ?", ids).Delete(&models.Batch{}).Error
}

// ---------- Status history ----------

type StatusHistoryRepository struct{ db *gorm.DB }

func (r *StatusHistoryRepository) Create(h *models.StatusHistory) error { return r.db.Create(h).Error }

// CreateBulk inserts many history rows in one statement — used by cascades that
// previously wrote one INSERT per affected batch item.
//
// Careful with large row counts: the statement carries 7 placeholders PER ROW,
// so a 1000-row call builds (and, with PrepareStmt on, makes Postgres parse) a
// 7000-placeholder INSERT — measured at ~1.4s against a remote database, and
// never reused because every distinct row count is a different statement. When
// the rows can be derived from a table the database already has, prefer
// RecordEntityTransition below, whose statement size is constant.
func (r *StatusHistoryRepository) CreateBulk(rows []models.StatusHistory) error {
	if len(rows) == 0 {
		return nil
	}
	return r.db.Create(&rows).Error
}

// RecordEntityTransition writes one history row per row matched by `source`,
// reading each entity's id and CURRENT status straight out of its own table via
// INSERT…SELECT. Nothing but the note and actor crosses the wire, so the
// statement stays a few hundred bytes — and one reusable prepared statement —
// whether it covers 5 entities or 5000.
//
// `source` must select exactly two columns: the entity id and its from-status.
// It has to be evaluated BEFORE the caller's UPDATE, while the old status is
// still there; running both under the same WHERE guard inside one transaction
// keeps history and the status change in lockstep even when a concurrent writer
// decides some of the entities first.
// `at` is bound as a parameter rather than read from the database's own clock,
// so the statement runs unchanged on Postgres and on the SQLite the tests use,
// and the history timestamp matches the one stamped on the entity itself.
func (r *StatusHistoryRepository) RecordEntityTransition(
	entityType models.EntityType, to string, actorID *uint, note string, at time.Time, source *gorm.DB,
) error {
	sql := `INSERT INTO status_histories
		(created_at, updated_at, entity_type, entity_id, from_status, to_status, changed_by_id, note)
		SELECT ?, ?, ?, src.entity_id, src.from_status, ?, ?, ? FROM (?) AS src`
	return r.db.Exec(sql, at, at, string(entityType), to, actorID, note, source).Error
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

// CreateBulk writes one QC record per production part in a single statement. A
// combo product is checked once but recorded per part, so the per-part loop it
// replaces cost one round trip per material — paid while the operator stands at
// the scanner waiting for the next item.
func (r *QCRepository) CreateBulk(rows []models.QCRecord) error {
	if len(rows) == 0 {
		return nil
	}
	return r.db.Create(&rows).Error
}

func (r *QCRepository) ListForItem(orderItemID uint) ([]models.QCRecord, error) {
	var rows []models.QCRecord
	err := r.db.Where("order_item_id = ?", orderItemID).Order("id desc").Find(&rows).Error
	return rows, err
}
