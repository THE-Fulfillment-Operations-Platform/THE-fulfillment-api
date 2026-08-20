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

// FindForCommit loads a job WITHOUT its per-row validation errors. The commit
// path reads the job header and its stored rows and nothing else; a file that
// failed validation on hundreds of rows would otherwise haul every one of those
// error rows across the wire before a single order is created.
func (r *ImportRepository) FindForCommit(id uint) (*models.ImportJob, error) {
	var j models.ImportJob
	if err := r.db.First(&j, id).Error; err != nil {
		return nil, err
	}
	return &j, nil
}

// MarkCommitted records the outcome of a commit by writing only the two columns
// that changed. Update/Save would write the whole row, and an import job's row
// carries RawRows — the JSONB copy of every line of the uploaded file — so
// saving the struct means shipping the entire file back to the database just to
// change a status word.
func (r *ImportRepository) MarkCommitted(id uint, createdCount int) error {
	return r.db.Model(&models.ImportJob{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":        models.ImportCommitted,
			"created_count": createdCount,
		}).Error
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
	// CancellationStatuses is the IN (...) form — the settled-cancellation list asks
	// for "approved OR seller-cancelled OR refused" in one query.
	CancellationStatuses []string
	// CancelBillable narrows to cancellations that are (or are not) still charged.
	// nil = don't care. This is the "đơn huỷ nhưng vẫn phải tính tiền" work list.
	CancelBillable *bool
	StoreOrderID   string
	DateFrom       *time.Time
	DateTo         *time.Time

	// Customer-support lookup fields. Search is the single-box form used by the CS
	// screen: one term matched against every identifier a customer might quote —
	// store order id, our internal code, tracking number, recipient name, phone or
	// email. The narrower fields exist for when the operator knows WHICH of those
	// they are holding and wants an unambiguous result.
	Search         string
	InternalCode   string
	TrackingNumber string
	ShippingName   string
	ShippingPhone  string
	// TrackingStatus filters by the parcel state, e.g. "the CS queue of orders
	// still without a tracking number" (TrackingStatus=NONE).
	TrackingStatus string
	// HasTracking narrows to orders that do (true) or do not (false) carry a
	// tracking number yet. nil = don't care.
	HasTracking *bool
	// HandedOver splits the two halves of an order's life: false = still the
	// factory's, true = already sent to the carrier. The journey screen lists
	// exactly HandedOver=true. nil = don't care.
	HandedOver *bool
	// HandedOverFrom/To bound the factory-exit moment (orders.handed_over_at) —
	// the journey screen's "xuất xưởng theo ngày" filter. Half-open [from, to):
	// the client sends the start of the day AFTER the last selected day, so no
	// midnight-boundary row is counted twice or dropped.
	HandedOverFrom *time.Time
	HandedOverTo   *time.Time
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
	last, err := r.ReserveDailySeq(scopeDate, 1, now)
	return last, err
}

// ReserveDailySeq allocates a BLOCK of n consecutive per-day sequences in one
// statement and returns the LAST one, so the caller owns [last-n+1 … last]. An
// import of a thousand orders reserves once instead of paying a round-trip per
// order, and the single UPDATE keeps the allocation just as race-safe: two
// concurrent imports get disjoint blocks.
func (r *OrderRepository) ReserveDailySeq(scopeDate string, n int, now time.Time) (int, error) {
	if n < 1 {
		n = 1
	}
	var seq int
	err := r.db.Raw(
		`INSERT INTO daily_counters (scope_date, seq, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (scope_date) DO UPDATE SET seq = daily_counters.seq + ?, updated_at = ?
		 RETURNING seq`,
		scopeDate, n, now, n, now,
	).Scan(&seq).Error
	return seq, err
}

// CreateMany inserts orders in batches — one statement per batch instead of one
// per order. IDs are filled in on the passed slice.
func (r *OrderRepository) CreateMany(rows []models.Order, batchSize int) error {
	if len(rows) == 0 {
		return nil
	}
	return r.db.CreateInBatches(rows, batchSize).Error
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

// OwnerSellerID returns just the order's seller, for endpoints whose only use of
// the order row is an ownership check. FindByID would answer the same question
// but drags six preloads (seller, items, SKUs, batch items, materials) along with
// it — six extra round trips to decide one integer comparison.
// Returns found=false for a missing order, so callers answer 404 instead of
// comparing against a zero seller id that no real order has.
func (r *OrderRepository) OwnerSellerID(id uint) (sellerID uint, found bool, err error) {
	var out []uint
	if err = r.db.Model(&models.Order{}).Where("id = ?", id).Pluck("seller_id", &out).Error; err != nil {
		return 0, false, err
	}
	if len(out) == 0 {
		return 0, false, nil
	}
	return out[0], true, nil
}

// StampHandedOverAt records the factory-exit moment, once: only a NULL
// handed_over_at is written, so a replayed carrier scan or a second handoff can
// never move the original date. Callers that already hold the row inside a
// transaction set the struct field instead; this is for the column-scoped paths
// (the tracking sync) that must not clobber concurrent edits.
func (r *OrderRepository) StampHandedOverAt(orderID uint, at time.Time) error {
	return r.db.Model(&models.Order{}).
		Where("id = ? AND handed_over_at IS NULL", orderID).
		Update("handed_over_at", at).Error
}

// OrderCodeRef is the light projection the tracking-import matcher works on:
// just enough to identify an order, show it to the operator and decide
// assign / overwrite. Loading full orders (six preloads each) for a
// thousand-row file would answer the same questions a hundred times slower.
type OrderCodeRef struct {
	ID             uint                `json:"id"`
	InternalCode   string              `json:"internal_code"`
	StoreOrderID   string              `json:"store_order_id"`
	ShippingName   string              `json:"shipping_name"`
	TrackingNumber string              `json:"tracking_number"`
	SellerStatus   models.SellerStatus `json:"seller_status"`
	HandedOverAt   *time.Time          `json:"handed_over_at"`
}

const orderCodeRefColumns = "orders.id, orders.internal_code, orders.store_order_id, " +
	"orders.shipping_name, orders.tracking_number, orders.seller_status, orders.handed_over_at"

func (r *OrderRepository) refsWhere(cond string, args ...interface{}) ([]OrderCodeRef, error) {
	var rows []OrderCodeRef
	err := r.db.Model(&models.Order{}).Select(orderCodeRefColumns).
		Where(cond, args...).Find(&rows).Error
	return rows, err
}

// RefsByInternalCodes / RefsByStoreOrderIDs / RefsByTrackingNumbers bulk-resolve
// the identifiers a tracking file may quote — one query per identifier kind for
// the whole file instead of a probe per row.
func (r *OrderRepository) RefsByInternalCodes(codes []string) ([]OrderCodeRef, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	return r.refsWhere("orders.internal_code IN ?", codes)
}

func (r *OrderRepository) RefsByStoreOrderIDs(ids []string) ([]OrderCodeRef, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return r.refsWhere("orders.store_order_id IN ?", ids)
}

func (r *OrderRepository) RefsByTrackingNumbers(numbers []string) ([]OrderCodeRef, error) {
	if len(numbers) == 0 {
		return nil, nil
	}
	return r.refsWhere("orders.tracking_number IN ?", numbers)
}

// HandedOverWithoutTracking lists the orders that left the factory in
// [from, to) and still carry no tracking number — the work list an uploaded
// tracking file is supposed to cover. Returns up to limit refs (newest first)
// plus the exact total, so the caller can say "N missing" even when the list
// itself is capped.
func (r *OrderRepository) HandedOverWithoutTracking(from, to *time.Time, limit int) ([]OrderCodeRef, int64, error) {
	scoped := func() *gorm.DB {
		q := r.db.Model(&models.Order{}).
			Where("orders.seller_status IN ?", models.HandedOverStatuses).
			Where("orders.tracking_number = ''")
		if from != nil {
			q = q.Where("orders.handed_over_at >= ?", *from)
		}
		if to != nil {
			q = q.Where("orders.handed_over_at < ?", *to)
		}
		return q
	}
	var total int64
	if err := scoped().Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []OrderCodeRef
	err := scoped().Select(orderCodeRefColumns).
		Order("orders.id desc").Limit(limit).Find(&rows).Error
	return rows, total, err
}

// IDByInternalCode resolves an order's internal (scan) code to its id — the ship
// station reads a QR, not a database id. Exact match on purpose: internal codes
// are system-generated and unique, so anything fuzzier only invites shipping the
// wrong order. Returns found=false when no order carries the code.
func (r *OrderRepository) IDByInternalCode(code string) (id uint, found bool, err error) {
	var out []uint
	if err = r.db.Model(&models.Order{}).Where("internal_code = ?", code).Pluck("id", &out).Error; err != nil {
		return 0, false, err
	}
	if len(out) == 0 {
		return 0, false, nil
	}
	return out[0], true, nil
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

// reviewApprovable is the guard shared by the bulk-approve UPDATE and the
// history INSERT…SELECT that runs alongside it: only orders still pending
// review / needing correction may be approved. Both statements applying the
// identical predicate is what keeps the two in step when a concurrent writer
// decides some of the orders in between.
func reviewApprovable(db *gorm.DB, ids []uint) *gorm.DB {
	return db.Model(&models.Order{}).
		Where("id IN ? AND review_status IN ?", ids,
			[]string{string(models.ReviewPending), string(models.ReviewNeedsFix)})
}

// ReviewApprovableSource selects (entity_id, from_status) for every order in ids
// that bulk-approve is still allowed to approve. It feeds
// StatusHistoryRepository.RecordEntityTransition, which turns it into history
// rows inside the database — so approving 1000 orders never ships 1000 rows over
// the wire. Must run BEFORE BulkSetReviewApproved, while from_status is still
// the pre-approval value.
func (r *OrderRepository) ReviewApprovableSource(ids []uint) *gorm.DB {
	return reviewApprovable(r.db, ids).Select("id AS entity_id, review_status AS from_status")
}

// BulkSetReviewApproved flips several orders to APPROVED in a single UPDATE,
// stamping the reviewer, note and timestamp. The review_status guard makes it
// safe under concurrency: only orders still pending review / needing correction
// are changed, so a race that already decided an order can't be clobbered.
func (r *OrderRepository) BulkSetReviewApproved(ids []uint, reviewerID *uint, note string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return reviewApprovable(r.db, ids).
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
	if len(f.CancellationStatuses) > 0 {
		q = q.Where("orders.cancellation_status IN ?", f.CancellationStatuses)
	}
	if f.CancelBillable != nil {
		q = q.Where("orders.cancel_billable = ?", *f.CancelBillable)
	}
	if f.StoreOrderID != "" {
		q = whereContains(q, "orders.store_order_id", f.StoreOrderID)
	}
	if f.InternalCode != "" {
		q = whereContains(q, "orders.internal_code", f.InternalCode)
	}
	if f.TrackingNumber != "" {
		q = whereContains(q, "orders.tracking_number", f.TrackingNumber)
	}
	if f.ShippingName != "" {
		q = whereContains(q, "orders.shipping_name", f.ShippingName)
	}
	if f.ShippingPhone != "" {
		q = whereContains(q, "orders.shipping_phone", f.ShippingPhone)
	}
	if f.TrackingStatus != "" {
		q = q.Where("orders.tracking_status = ?", f.TrackingStatus)
	}
	if f.HasTracking != nil {
		if *f.HasTracking {
			q = q.Where("orders.tracking_number <> ''")
		} else {
			q = q.Where("orders.tracking_number = ''")
		}
	}
	if f.HandedOver != nil {
		if *f.HandedOver {
			q = q.Where("orders.seller_status IN ?", models.HandedOverStatuses)
		} else {
			q = q.Where("orders.seller_status NOT IN ?", models.HandedOverStatuses)
		}
	}
	if f.HandedOverFrom != nil {
		q = q.Where("orders.handed_over_at >= ?", *f.HandedOverFrom)
	}
	if f.HandedOverTo != nil {
		q = q.Where("orders.handed_over_at < ?", *f.HandedOverTo)
	}
	// The single-box customer-support search. Everything here is something a
	// customer can quote down the phone, so one term has to try them all — the
	// operator should not need to know in advance whether they were handed a store
	// order id, our code, a tracking number, a name or a phone number.
	if s := strings.ToLower(strings.TrimSpace(f.Search)); s != "" {
		like := "%" + s + "%"
		cols := []string{
			"orders.store_order_id", "orders.internal_code", "orders.tracking_number",
			"orders.shipping_name", "orders.shipping_phone",
		}
		var sb strings.Builder
		args := make([]interface{}, 0, len(cols))
		for i, col := range cols {
			if i > 0 {
				sb.WriteString(" OR ")
			}
			sb.WriteString("LOWER(" + col + ") LIKE ?")
			args = append(args, like)
		}
		q = q.Where("("+sb.String()+")", args...)
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

// whereContains adds a case-insensitive "contains" filter.
//
// LOWER(col) LIKE lower-pattern rather than Postgres' ILIKE: the same predicate
// then runs unchanged on the sqlite engine the tests use. Neither form can use a
// plain btree index on a leading-wildcard search anyway, so nothing is lost.
func whereContains(q *gorm.DB, column, value string) *gorm.DB {
	value = strings.TrimSpace(value)
	if value == "" {
		return q
	}
	return q.Where("LOWER("+column+") LIKE ?", "%"+strings.ToLower(value)+"%")
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

// InProductionIDs returns, for the given orders, the set that already has work in
// flight: any live line item past PENDING or scheduled into a batch.
//
// The cancellation rules turn on exactly this fact ("chưa sản xuất → huỷ tự do,
// đã sản xuất → chờ duyệt"), and a list screen must answer it for a whole page of
// orders at once. Deriving it from preloaded associations would mean pulling every
// item's batch parts into memory per page; one grouped query over the two tables
// answers it for all of them. Cancelled lines are excluded — they are history, not
// work in flight.
func (r *OrderRepository) InProductionIDs(orderIDs []uint) (map[uint]bool, error) {
	out := make(map[uint]bool, len(orderIDs))
	if len(orderIDs) == 0 {
		return out, nil
	}
	var ids []uint
	err := r.db.Model(&models.OrderItem{}).
		Distinct("order_items.order_id").
		Joins("LEFT JOIN batch_items ON batch_items.order_item_id = order_items.id AND batch_items.deleted_at IS NULL").
		Where("order_items.order_id IN ?", orderIDs).
		Where("order_items.cancellation_status NOT IN ?",
			[]models.CancellationStatus{models.CancellationSeller, models.CancellationApproved}).
		Where("order_items.internal_status <> ? OR batch_items.id IS NOT NULL", models.StatusPending).
		Pluck("order_items.order_id", &ids).Error
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
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
	Search         string // partial, case-insensitive match on EITHER internal_code OR sku_code (one search box)
	HasDesignFile  bool   // only items that already have a front or back design file (design-download pick-list)
	InternalStatus string
	DesignStatus   string
	ReviewStatus   string // exact match on the parent order's review_status
	BatchID        *uint
	BatchCode      string // partial, case-insensitive batch code (e.g. #101001)
	MaterialID     *uint  // items whose SKU includes this material (design queue "gom theo NVL")
	IDs            []uint // restrict to specific item ids (e.g. selected rows for a ZIP export)
	NeedsDesign    bool   // design queue: design not ready
	ReviewApproved bool   // only items whose order review_status = APPROVED
	// IncludeCancelled keeps cancelled lines in the result. Off everywhere work is
	// listed; on when the caller is deliberately looking at history (the order list
	// filtered to "Đã huỷ"), which would otherwise come back empty.
	IncludeCancelled bool
	DateFrom         *time.Time
	DateTo           *time.Time
	// Server-side sort. SortBy is whitelisted (see itemOrderClause); anything else
	// falls back to the stable default (newest first). SortDir is asc|desc.
	SortBy  string
	SortDir string

	// WithSKUMaterials adds the SKU → materials chain (3 extra round trips).
	//
	// Off by default because only the design queue reads it: that screen groups the
	// queue by NVL from the SKU's bill of materials. The Orders/Items screen renders
	// nothing from `sku`, so loading it there paid for three queries per request and
	// threw the rows away — on a DB one internet hop away, that is most of the wait.
	//
	// A flag rather than two copies of this method: the filter/sort logic below is
	// long and having it drift between a "lean" and a "full" variant is a worse bug
	// than one boolean.
	WithSKUMaterials bool
	// WithSeller adds the parent order's seller (1 extra round trip). Only screens
	// that print the seller's name need it.
	WithSeller bool
}

// itemOrderClause maps a whitelisted sort key + direction to a safe SQL ORDER BY.
// Every branch appends a deterministic tiebreaker so pagination stays stable when
// the primary key has duplicates (e.g. many items share a SKU). Unknown keys fall
// back to newest-first — this is the only place item sort SQL is built, so a
// client-supplied value can never inject into the query.
// itemOrderClause builds the ORDER BY for the item list.
//
// Every branch tie-breaks on (line_no ASC, id ASC), NOT on id DESC: the rows of
// one order are its lines 1/3, 2/3, 3/3 and must read in that order whichever
// column the user sorts by. Tie-breaking on id DESC printed them backwards
// (3/3, 2/3, 1/3), which reads as broken numbering even though the data is fine.
func itemOrderClause(sortBy, sortDir string) string {
	dir := "DESC"
	if strings.EqualFold(sortDir, "asc") {
		dir = "ASC"
	}
	// Lines within an order: always ascending, and last in the key so it only ever
	// breaks ties.
	const lines = ", order_items.line_no ASC, order_items.id ASC"
	switch strings.ToLower(strings.TrimSpace(sortBy)) {
	case "sku", "sku_code":
		return "order_items.sku_code " + dir + lines
	case "quantity", "qty":
		return "order_items.quantity " + dir + lines
	case "created_at", "date":
		return "order_items.created_at " + dir + lines
	case "stt", "daily_seq":
		return "orders.order_date " + dir + ", orders.daily_seq " + dir + lines
	case "internal_code":
		return "order_items.internal_code " + dir + lines
	case "batch", "batch_code":
		// An item can belong to several material batches. MIN(code) gives it one
		// stable grouping key; unbatched items are placed after batched items in
		// both directions so the production groups remain the first thing shown.
		batchKey := `(SELECT MIN(b.code)
			FROM batch_items bi
			JOIN batches b ON b.id = bi.batch_id AND b.deleted_at IS NULL
			WHERE bi.order_item_id = order_items.id AND bi.deleted_at IS NULL)`
		return "CASE WHEN " + batchKey + " IS NULL THEN 1 ELSE 0 END ASC, " +
			batchKey + " " + dir + lines
	default:
		// Newest order first, but its lines still in order — the default view is a
		// list of orders' items, not a stream of rows in insert order.
		return "orders.created_at DESC, orders.id DESC" + lines
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

// FindByCode is FindByID keyed on the tem code — same association set on purpose.
// The scan stations resolve an item by code and then answer with it, so anything
// FindByID preloads must come back here too; otherwise the caller has to re-load
// the item just to fill the gap, and on a remote database that second load is a
// dozen extra round trips on the operator's clock.
func (r *OrderItemRepository) FindByCode(code string) (*models.OrderItem, error) {
	var it models.OrderItem
	err := r.db.
		Preload("Order.Seller").
		Preload("SKU.Materials.Material").
		Preload("BatchItems.Batch").
		Preload("BatchItems.Material").
		Preload("Assets").
		Where("internal_code = ?", code).First(&it).Error
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// OrderIDByCode resolves an item's tem code to the order that owns it, and
// nothing more. The ship station scans whole orders; when the operator scans an
// item tem instead, the item itself is irrelevant — only which order it belongs
// to matters, so this is a single pluck instead of FindByCode's preload chain.
func (r *OrderItemRepository) OrderIDByCode(code string) (orderID uint, found bool, err error) {
	var out []uint
	if err = r.db.Model(&models.OrderItem{}).Where("internal_code = ?", code).Pluck("order_id", &out).Error; err != nil {
		return 0, false, err
	}
	if len(out) == 0 {
		return 0, false, nil
	}
	return out[0], true, nil
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

// ItemCancelPatch is the terminal cancellation state stamped onto an order's
// remaining live lines when the order as a whole is cancelled.
type ItemCancelPatch struct {
	Status       models.CancellationStatus
	Reason       string
	ResolvedByID *uint
	ResolvedAt   time.Time
	Note         string
	Stage        models.CancelStage
	Billable     bool
}

// CancelActiveForOrder propagates an order-level cancellation down to every line
// that is not already cancelled, and reports how many lines it closed.
//
// This is what actually pulls a cancelled order out of the factory. Every
// operational queue — design, batching, QC, packing — filters on the ITEM's
// cancellation status (see baseQuery, activeBatchItems, fulfillment_repo), so an
// order whose header says CANCELLED while its lines still say NONE reads as
// cancelled to the seller and as live work to production. One UPDATE keeps the
// two in step regardless of how many lines the order has.
//
// A line that carried its own cancellation reason (the seller asked for that one
// product specifically) keeps it; only the empty ones inherit the order's reason.
func (r *OrderItemRepository) CancelActiveForOrder(orderID uint, p ItemCancelPatch) (int64, error) {
	res := r.db.Model(&models.OrderItem{}).
		Where("order_id = ?", orderID).
		Where("cancellation_status NOT IN ?",
			[]models.CancellationStatus{models.CancellationSeller, models.CancellationApproved}).
		Updates(map[string]interface{}{
			"cancellation_status": p.Status,
			"cancellation_reason": gorm.Expr(
				"CASE WHEN cancellation_reason = '' THEN ? ELSE cancellation_reason END", p.Reason),
			"cancellation_requested_at": gorm.Expr(
				"COALESCE(cancellation_requested_at, ?)", p.ResolvedAt),
			"cancellation_resolved_by_id":  p.ResolvedByID,
			"cancellation_resolved_at":     p.ResolvedAt,
			"cancellation_resolution_note": p.Note,
			"cancel_stage":                 p.Stage,
			"cancel_billable":              p.Billable,
		})
	return res.RowsAffected, res.Error
}

func (r *OrderItemRepository) baseQuery(f ItemFilter) *gorm.DB {
	q := r.db.Model(&models.OrderItem{}).
		Joins("JOIN orders ON orders.id = order_items.order_id AND orders.deleted_at IS NULL")
	// This is the operational item list used by Orders/Items, design, batch, QC and
	// packing. Cancelled lines are historical records, never work — so they are out
	// by default. IncludeCancelled is the caller saying "I am looking for history,
	// not work": without it, asking the order list for cancelled orders returns
	// nothing at all, since cancelling an order cancels every line in it.
	if !f.IncludeCancelled {
		q = q.Where("order_items.cancellation_status NOT IN ?",
			[]models.CancellationStatus{models.CancellationSeller, models.CancellationApproved})
	}
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
	if f.Search != "" {
		// One search box over two columns. LOWER(col) LIKE LOWER(pattern) rather than
		// ILIKE so the same clause is case-insensitive on both Postgres and the SQLite
		// used in tests. Grouped so the OR can't leak into the surrounding AND chain
		// (which would widen every other filter).
		like := "%" + strings.ToLower(f.Search) + "%"
		q = q.Where(r.db.Where("LOWER(order_items.internal_code) LIKE ?", like).
			Or("LOWER(order_items.sku_code) LIKE ?", like))
	}
	if f.HasDesignFile {
		// COALESCE guards against NULL (the columns have no NOT NULL constraint), so
		// "has a file" means a non-empty front OR back design URL.
		q = q.Where("COALESCE(order_items.design_url, '') <> '' OR COALESCE(order_items.back_design_url, '') <> ''")
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

// List returns one page of items. Associations are opt-in via the filter (see
// WithSKUMaterials / WithSeller): every Preload here is a separate round trip, and
// the two callers of this method render different things.
func (r *OrderItemRepository) List(f ItemFilter) ([]models.OrderItem, int64, error) {
	var rows []models.OrderItem
	var total int64
	r.baseQuery(f).Count(&total)
	q := r.baseQuery(f).
		// Always needed: every list shows the parent order (code, store order id,
		// status, dates) and the item's batch parts (NVL + batch columns).
		Preload("Order").
		// Batch and Material are belongs-to on BatchItem, so they can ride along in
		// the batch-items query as JOINs instead of costing a round trip each:
		// three statements collapse into one. `Preload("BatchItems.Batch")` would be
		// the same data in three trips.
		Preload("BatchItems", func(db *gorm.DB) *gorm.DB {
			return db.Joins("Batch").Joins("Material")
		})
	if f.WithSeller {
		q = q.Preload("Order.Seller")
	}
	if f.WithSKUMaterials {
		q = q.Preload("SKU.Materials.Material")
	}
	err := q.
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

// ClearProductionFileForBatch is the delete-side inverse of
// SetProductionFileForBatch: it blanks the column on the batch's items, but only
// where the value still IS this batch's link — an item since re-stamped by
// another batch keeps that batch's file. Left in place, the stale URL would
// resurface via the item-level fallback in the production export and the next
// batch could be produced from the deleted batch's file. Must run while the
// batch's parts still exist (the item set comes from batch_items). `column` is
// caller-supplied and must stay a fixed literal, never user input.
func (r *OrderItemRepository) ClearProductionFileForBatch(batchID uint, column, url string) (int64, error) {
	res := r.db.Model(&models.OrderItem{}).
		Where("id IN (?)",
			r.db.Model(&models.BatchItem{}).Select("order_item_id").Where("batch_id = ?", batchID)).
		Where(column+" = ?", url).
		Update(column, "")
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
				// scrapped_at IS NULL: a part written off after a QC fail no longer
				// counts as "already batched", which is what lets the item come back
				// into the create-batch bucket to be re-made.
				Where("bi.order_item_id = oi.id AND bi.material_id = sm.material_id AND bi.deleted_at IS NULL AND bi.scrapped_at IS NULL"))
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

// SKUBucket groups design-queue items by SKU code, with how many items each covers.
type SKUBucket struct {
	SKUCode   string `json:"sku_code"`
	SKUName   string `json:"sku_name"`
	ItemCount int64  `json:"item_count"`
}

// DesignQueueSKUs lists only the SKU codes that actually have items in the design
// queue, with counts — the same idea as DesignQueueMaterials, for the SKU filter.
// Grouping is on order_items.sku_code (the code stamped on the line), so an item
// whose SKU row was removed from the catalog still shows up; the name is joined in
// when the SKU exists. Callers pass the queue's own filter with SKUCode cleared, so
// the facet reflects the other active filters (batch, NVL) but never itself.
func (r *OrderItemRepository) DesignQueueSKUs(f ItemFilter) ([]SKUBucket, error) {
	var rows []SKUBucket
	err := r.baseQuery(f).
		Joins("LEFT JOIN skus s ON s.id = order_items.sku_id AND s.deleted_at IS NULL").
		// MAX() rather than adding s.name to GROUP BY: one code maps to one SKU row,
		// and this keeps the grouping key exactly what the filter sends back.
		Select("order_items.sku_code AS sku_code, COALESCE(MAX(s.name), '') AS sku_name, " +
			"COUNT(DISTINCT order_items.id) AS item_count").
		Group("order_items.sku_code").
		Order("order_items.sku_code ASC").
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
