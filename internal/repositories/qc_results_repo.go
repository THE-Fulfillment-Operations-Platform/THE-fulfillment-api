package repositories

import (
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// ---------- QC results (đơn nào QC xong, đơn nào chưa) ----------
//
// The QC station answers "is THIS product good?", one scan at a time. The
// question the floor asks next has the opposite shape: "which ORDERS can be
// packed now, and for the ones that can't, what exactly is missing?" That is what
// these queries serve — orders with the QC state of their lines, in a fixed
// number of statements per page however many lines an order has.

// QCResultFilter drives the QC-results screen. State is evaluated per ORDER over
// its live (non-cancelled) lines.
type QCResultFilter struct {
	Page
	// State: "done" (mọi sản phẩm đã đạt), "partial" (đạt một phần), "none" (chưa
	// sản phẩm nào đạt), "rework" (có sản phẩm đang làm lại). Blank = tất cả.
	State    string
	Search   string // mã đơn nội bộ / store order id / mã sản phẩm / SKU
	SellerID *uint
	DateFrom *time.Time
	DateTo   *time.Time
	// HandedOver splits the QC-done work into "chưa gửi cho THE" (false) and "đã
	// gửi" (true). The ship queue is exactly State=done + HandedOver=false; the
	// journey screen is HandedOver=true. nil = don't care.
	HandedOver *bool
}

// QCResultSummary counts the WHOLE filtered set, not just the page, so the totals
// on screen don't shift as the user pages through.
type QCResultSummary struct {
	Orders        int64 `json:"orders"`
	OrdersDone    int64 `json:"orders_done"`
	OrdersPartial int64 `json:"orders_partial"`
	OrdersNone    int64 `json:"orders_none"`
	ItemsPassed   int64 `json:"items_passed"`
	ItemsWaiting  int64 `json:"items_waiting"`
	ItemsRework   int64 `json:"items_rework"`
}

// liveItemsSQL is the set of lines QC cares about: a cancelled line is never
// produced or checked, so counting it would leave its order looking permanently
// unfinished.
const liveItemsSQL = `oi.deleted_at IS NULL AND oi.cancellation_status NOT IN ('SELLER_CANCELLED','APPROVED')`

// QCLastCheck is the most recent QC verdict on one item.
type QCLastCheck struct {
	OrderItemID uint       `gorm:"column:order_item_id" json:"order_item_id"`
	Result      string     `gorm:"column:result" json:"result"`
	DefectCode  string     `gorm:"column:defect_code" json:"defect_code"`
	Note        string     `gorm:"column:note" json:"note"`
	CheckedAt   *time.Time `gorm:"column:checked_at" json:"checked_at"`
	CheckedBy   string     `gorm:"column:checked_by" json:"checked_by"`
}

// qcResultBase selects the orders the screen lists: approved (only approved work
// reaches production, so only it can have QC results) and not cancelled, plus the
// caller's filters.
func (r *OrderRepository) qcResultBase(f QCResultFilter) *gorm.DB {
	q := r.db.Model(&models.Order{}).
		Where("orders.review_status = ?", models.ReviewApproved).
		Where("orders.cancellation_status NOT IN ?",
			[]models.CancellationStatus{models.CancellationSeller, models.CancellationApproved})

	if f.SellerID != nil {
		q = q.Where("orders.seller_id = ?", *f.SellerID)
	}
	if f.HandedOver != nil {
		if *f.HandedOver {
			q = q.Where("orders.seller_status IN ?", models.HandedOverStatuses)
		} else {
			q = q.Where("orders.seller_status NOT IN ?", models.HandedOverStatuses)
		}
	}
	if f.DateFrom != nil {
		q = q.Where("orders.created_at >= ?", *f.DateFrom)
	}
	if f.DateTo != nil {
		q = q.Where("orders.created_at <= ?", *f.DateTo)
	}
	if s := strings.ToLower(strings.TrimSpace(f.Search)); s != "" {
		like := "%" + s + "%"
		// One box searches the order AND its lines: staff type whichever code is on
		// the paper in front of them (mã đơn, store order id, mã tem sản phẩm, SKU).
		itemHit := r.db.Table("order_items oi").Select("1").
			Where("oi.order_id = orders.id").
			Where("LOWER(oi.internal_code) LIKE ? OR LOWER(oi.sku_code) LIKE ?", like, like)
		q = q.Where(
			r.db.Where("LOWER(orders.internal_code) LIKE ?", like).
				Or("LOWER(orders.store_order_id) LIKE ?", like).
				Or("EXISTS (?)", itemHit),
		)
	}

	lines := func(extra string, args ...any) *gorm.DB {
		return r.db.Table("order_items oi").Select("1").
			Where("oi.order_id = orders.id").
			Where(liveItemsSQL).
			Where(extra, args...)
	}
	anyLine := lines("1 = 1")
	passed := lines("oi.internal_status = ?", models.StatusQCPassed)
	notPassed := lines("oi.internal_status <> ?", models.StatusQCPassed)

	switch strings.ToLower(strings.TrimSpace(f.State)) {
	case "done": // đã QC đủ → sẵn sàng đóng gói
		q = q.Where("EXISTS (?)", anyLine).Where("NOT EXISTS (?)", notPassed)
	case "partial":
		q = q.Where("EXISTS (?)", passed).Where("EXISTS (?)", notPassed)
	case "none":
		q = q.Where("EXISTS (?)", anyLine).Where("NOT EXISTS (?)", passed)
	case "rework":
		// "Đang làm lại" = đã fail và CHƯA quay lại đạt. Một sản phẩm fail rồi làm
		// lại và đã đạt thì thuộc nhóm đạt, không còn là việc phải theo.
		q = q.Where("EXISTS (?)", lines("oi.rework_count > 0 AND oi.internal_status <> ?", models.StatusQCPassed))
	}
	return q
}

// QCResultOrderIDs returns one page of matching order ids (newest first) plus the
// total, so the caller can load exactly those orders and their lines.
func (r *OrderRepository) QCResultOrderIDs(f QCResultFilter) ([]uint, int64, error) {
	var total int64
	if err := r.qcResultBase(f).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var ids []uint
	err := r.qcResultBase(f).
		Order("orders.id DESC").
		Limit(f.PageSize).Offset(f.Offset()).
		Pluck("orders.id", &ids).Error
	return ids, total, err
}

// QCResultSummaryFor computes the screen's totals over the whole filtered set in
// ONE statement: per-order aggregates first, then rolled into the seven numbers.
// Plain CASE/SUM (no FILTER) so the sqlite test engine runs it too.
func (r *OrderRepository) QCResultSummaryFor(f QCResultFilter) (QCResultSummary, error) {
	var out QCResultSummary
	// The tiles are how the user PICKS a state, so they must keep showing every
	// bucket's size — the state filter is deliberately dropped here.
	base := f
	base.State = ""
	sub := r.qcResultBase(base).Select("orders.id")

	err := r.db.Raw(`
		SELECT
			COUNT(*) AS orders,
			COALESCE(SUM(CASE WHEN total > 0 AND passed = total THEN 1 ELSE 0 END), 0) AS orders_done,
			COALESCE(SUM(CASE WHEN passed > 0 AND passed < total THEN 1 ELSE 0 END), 0) AS orders_partial,
			COALESCE(SUM(CASE WHEN total > 0 AND passed = 0 THEN 1 ELSE 0 END), 0) AS orders_none,
			COALESCE(SUM(passed), 0) AS items_passed,
			COALESCE(SUM(total - passed), 0) AS items_waiting,
			COALESCE(SUM(rework), 0) AS items_rework
		FROM (
			SELECT oi.order_id AS order_id,
				COUNT(oi.id) AS total,
				SUM(CASE WHEN oi.internal_status = ? THEN 1 ELSE 0 END) AS passed,
				SUM(CASE WHEN oi.rework_count > 0 AND oi.internal_status <> ? THEN 1 ELSE 0 END) AS rework
			FROM order_items oi
			WHERE oi.order_id IN (?) AND `+liveItemsSQL+`
			GROUP BY oi.order_id
		) t`, models.StatusQCPassed, models.StatusQCPassed, sub).Scan(&out).Error
	return out, err
}

// FindByIDsWithSeller loads orders by id with just the seller preloaded, keyed by
// id — the QC-results screen needs the seller's name and nothing else, so it
// skips the heavy item/SKU/batch preload chain of FindByID.
func (r *OrderRepository) FindByIDsWithSeller(ids []uint) (map[uint]*models.Order, error) {
	out := map[uint]*models.Order{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []models.Order
	if err := r.db.Preload("Seller").Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		out[rows[i].ID] = &rows[i]
	}
	return out, nil
}

// QCResultLines loads the live lines of the given orders, in reading order.
func (r *OrderItemRepository) QCResultLines(orderIDs []uint) ([]models.OrderItem, error) {
	if len(orderIDs) == 0 {
		return nil, nil
	}
	var rows []models.OrderItem
	err := r.db.Table("order_items AS oi").
		Select("oi.*").
		Where("oi.order_id IN ?", orderIDs).
		Where(liveItemsSQL).
		Order("oi.order_id DESC, oi.line_no ASC, oi.id ASC").
		Scan(&rows).Error
	return rows, err
}

// LatestChecks returns the most recent QC verdict per item — one query for the
// whole page (MAX(id) per item), not a lookup per line.
func (r *QCRepository) LatestChecks(orderItemIDs []uint) (map[uint]QCLastCheck, error) {
	out := map[uint]QCLastCheck{}
	if len(orderItemIDs) == 0 {
		return out, nil
	}
	latest := r.db.Table("qc_records").
		Select("order_item_id, MAX(id) AS id").
		Where("order_item_id IN ?", orderItemIDs).
		Group("order_item_id")

	var rows []QCLastCheck
	err := r.db.Table("qc_records AS q").
		Select(`q.order_item_id AS order_item_id, q.result AS result, q.defect_code AS defect_code,
			q.note AS note, q.created_at AS checked_at, COALESCE(u.full_name, u.email, '') AS checked_by`).
		Joins("JOIN (?) AS m ON m.id = q.id", latest).
		Joins("LEFT JOIN users u ON u.id = q.checked_by_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.OrderItemID] = row
	}
	return out, nil
}
