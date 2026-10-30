package services

import (
	"strings"
	"time"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// QC result states, per product and per order.
const (
	QCItemPassed  = "PASSED"  // đã QC đạt
	QCItemRework  = "REWORK"  // đã QC fail, đang làm lại
	QCItemWaiting = "WAITING" // chưa QC (đang sản xuất / chờ tới lượt)

	QCOrderDone    = "DONE"    // mọi sản phẩm đã đạt → sẵn sàng đóng gói
	QCOrderPartial = "PARTIAL" // đạt một phần
	QCOrderNone    = "NONE"    // chưa sản phẩm nào đạt
)

// QCResultItem is one product line with its QC verdict.
type QCResultItem struct {
	ItemID         uint                  `json:"item_id"`
	InternalCode   string                `json:"internal_code"`
	SKUCode        string                `json:"sku_code"`
	ProductName    string                `json:"product_name"`
	Quantity       int                   `json:"quantity"`
	InternalStatus models.InternalStatus `json:"internal_status"` // giai đoạn sản xuất
	QCStatus       string                `json:"qc_status"`       // PASSED | REWORK | WAITING
	ReworkCount    int                   `json:"rework_count"`
	MockupURL      string                `json:"mockup_url"`
	// Lần QC gần nhất (nếu có) — ai kiểm, lúc nào, kết quả gì, lỗi gì.
	LastResult    string     `json:"last_result,omitempty"`
	LastDefect    string     `json:"last_defect,omitempty"`
	LastNote      string     `json:"last_note,omitempty"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
	LastCheckedBy string     `json:"last_checked_by,omitempty"`
}

// QCResultOrder is an order with the QC state of every product in it.
type QCResultOrder struct {
	OrderID      uint      `json:"order_id"`
	InternalCode string    `json:"internal_code"`
	StoreOrderID string    `json:"store_order_id"`
	SellerName   string    `json:"seller_name"`
	OrderDate    string    `json:"order_date"`
	DailySeq     int       `json:"daily_seq"`
	CreatedAt    time.Time `json:"created_at"`

	Status string `json:"status"` // DONE | PARTIAL | NONE
	// Shipping side of the same order. The ship queue and the journey screen are
	// both built on this endpoint, so they need to see whether an order has
	// already gone to THE and what its parcel is doing.
	SellerStatus   models.SellerStatus   `json:"seller_status"`
	HandedOver     bool                  `json:"handed_over"`
	TrackingNumber string                `json:"tracking_number,omitempty"`
	TrackingStatus models.TrackingStatus `json:"tracking_status,omitempty"`
	// BasketNumber is the waiting basket (giỏ chờ) the order's finished products
	// sit in, until a picker bundles them and confirms. nil = not in a basket.
	BasketNumber *int `json:"basket_number,omitempty"`
	// THETracking / THELastMile: the label already created on THE for this order
	// (empty = none yet). Creating the label and taking the goods out of the
	// warehouse are two separate steps, so the ship queue shows which orders
	// already have their label and only wait for "Xuất kho".
	THETracking string `json:"the_tracking,omitempty"`
	THELastMile string `json:"the_last_mile,omitempty"`
	// THECancelled is the code of the label THE killed, when the order's latest
	// shipment ended that way: it needs a new label before it may leave.
	THECancelled string `json:"the_cancelled,omitempty"`
	TotalItems   int    `json:"total_items"`
	PassedItems  int    `json:"passed_items"`
	ReworkItems  int    `json:"rework_items"`
	WaitingItems int    `json:"waiting_items"`

	Items []QCResultItem `json:"items"`
}

// QCResults lists orders with their products' QC state — the "đơn nào QC xong,
// đơn nào chưa" view. Four statements per page regardless of how many products
// the orders hold: page of order ids, the orders, their live lines, and the
// latest QC verdict per line.
func (s *QCService) QCResults(f repositories.QCResultFilter) ([]QCResultOrder, repositories.QCResultSummary, int64, error) {
	f.Page = f.Page.Normalize()

	summary, err := s.repo.Order.QCResultSummaryFor(f)
	if err != nil {
		return nil, summary, 0, apperr.Internal("could not summarise QC results").Wrap(err)
	}
	ids, total, err := s.repo.Order.QCResultOrderIDs(f)
	if err != nil {
		return nil, summary, 0, apperr.Internal("could not list QC results").Wrap(err)
	}
	if len(ids) == 0 {
		return []QCResultOrder{}, summary, total, nil
	}

	out, err := s.buildResultOrders(ids)
	if err != nil {
		return nil, summary, 0, err
	}
	return out, summary, total, nil
}

// buildResultOrders turns order ids into QCResultOrder rows (in the given order),
// each with its live lines and their QC verdicts. Shared by the results list and
// the per-order QC scan, so both always agree on what "đã QC" means. Fixed number
// of statements regardless of how many lines the orders hold.
func (s *QCService) buildResultOrders(ids []uint) ([]QCResultOrder, error) {
	orders, err := s.repo.Order.FindByIDsWithSeller(ids)
	if err != nil {
		return nil, apperr.Internal("could not load orders").Wrap(err)
	}
	lines, err := s.repo.OrderItem.QCResultLines(ids)
	if err != nil {
		return nil, apperr.Internal("could not load order lines").Wrap(err)
	}
	itemIDs := make([]uint, 0, len(lines))
	for i := range lines {
		itemIDs = append(itemIDs, lines[i].ID)
	}
	checks, err := s.repo.QC.LatestChecks(itemIDs)
	if err != nil {
		return nil, apperr.Internal("could not load QC records").Wrap(err)
	}

	byOrder := map[uint]*QCResultOrder{}
	out := make([]QCResultOrder, 0, len(ids))
	// Build in the page's order (newest first) so the response needs no re-sorting.
	for _, id := range ids {
		o := orders[id]
		if o == nil {
			continue
		}
		sellerName := o.Seller.Name
		row := QCResultOrder{
			OrderID: o.ID, InternalCode: o.InternalCode, StoreOrderID: o.StoreOrderID,
			SellerName: sellerName, OrderDate: o.OrderDate, DailySeq: o.DailySeq,
			CreatedAt: o.CreatedAt, Items: []QCResultItem{},
			SellerStatus: o.SellerStatus, HandedOver: o.SellerStatus.HandedOver(),
			TrackingNumber: o.TrackingNumber,
		}
		// NONE is our "nothing recorded" placeholder, not a shipment state — leave
		// it out so the screen can tell "no parcel yet" from "waiting first scan".
		if o.TrackingStatus != models.TrackingNone {
			row.TrackingStatus = o.TrackingStatus
		}
		out = append(out, row)
		byOrder[o.ID] = &out[len(out)-1]
	}

	// One lookup for the whole page: product names live on the SKU in master data,
	// not on the line (see models.OrderItem.ProductName). These rows come from a
	// raw projection, so there is no SKU preload to read them off.
	skuNames := map[string]string{}
	if len(lines) > 0 {
		seen := map[string]bool{}
		codes := make([]string, 0, len(lines))
		for i := range lines {
			if c := lines[i].SKUCode; c != "" && !seen[c] {
				seen[c] = true
				codes = append(codes, c)
			}
		}
		infos, infoErr := s.repo.SKU.InfoByCodes(codes)
		if infoErr != nil {
			return nil, apperr.Internal("could not load SKU names").Wrap(infoErr)
		}
		for code, info := range infos {
			skuNames[code] = info.ProductName
		}
	}

	for i := range lines {
		line := &lines[i]
		parent := byOrder[line.OrderID]
		if parent == nil {
			continue
		}
		item := QCResultItem{
			ItemID: line.ID, InternalCode: line.InternalCode, SKUCode: line.SKUCode,
			ProductName: skuNames[line.SKUCode], Quantity: line.Quantity,
			InternalStatus: line.InternalStatus, ReworkCount: line.ReworkCount,
			MockupURL: line.MockupURL,
		}
		if c, ok := checks[line.ID]; ok {
			item.LastResult, item.LastDefect, item.LastNote = c.Result, c.DefectCode, c.Note
			item.LastCheckedAt, item.LastCheckedBy = c.CheckedAt, c.CheckedBy
		}
		// PASSED is the production status, not the last verdict: a product that
		// failed and was re-made and passed is passed. REWORK is "failed and not yet
		// back through", which is exactly rework_count > 0 without a pass.
		switch {
		case line.InternalStatus == models.StatusQCPassed:
			item.QCStatus = QCItemPassed
			parent.PassedItems++
		case line.ReworkCount > 0:
			item.QCStatus = QCItemRework
			parent.ReworkItems++
			parent.WaitingItems++
		default:
			item.QCStatus = QCItemWaiting
			parent.WaitingItems++
		}
		parent.TotalItems++
		parent.Items = append(parent.Items, item)
	}

	baskets, err := s.repo.Basket.LiveByOrderIDs(ids)
	if err != nil {
		return nil, apperr.Internal("could not load waiting baskets").Wrap(err)
	}
	// Display-only enrichment: a failure here must not hide the QC results.
	shipments, _ := s.repo.Carrier.LatestShipmentsByOrder(ids)
	for i := range out {
		o := &out[i]
		if sh, ok := shipments[o.OrderID]; ok && sh.Status == models.ShipmentLabeled {
			o.THETracking, o.THELastMile = sh.TrackingCode, sh.LastMileTracking
		} else if ok && cancelledByTHE(&sh) {
			o.THECancelled = nonEmpty(sh.TrackingCode, sh.OrderNumber)
		}
		if b, ok := baskets[o.OrderID]; ok {
			n := b.Number
			o.BasketNumber = &n
		}
		switch {
		case o.TotalItems > 0 && o.PassedItems == o.TotalItems:
			o.Status = QCOrderDone
		case o.PassedItems > 0:
			o.Status = QCOrderPartial
		default:
			o.Status = QCOrderNone
		}
	}
	return out, nil
}

// OrderScanRef identifies an order by the code on its paper / QR (internal code,
// or the seller's store order id) or by id.
type OrderScanRef struct {
	Code    string `json:"code"`
	OrderID *uint  `json:"order_id"`
}

// QCOrderScan is what the QC station shows when the operator scans a whole ORDER
// instead of one product tem: the order, how many products it holds and where each
// one stands, so the operator sees the whole order first and opens the products one
// by one to judge them.
type QCOrderScan struct {
	QCResultOrder
	OrderNote string `json:"order_note"`
}

// ScanOrder resolves an order code to the order with the QC state of every live
// product in it. Read-only: the verdict itself still goes through Pass / Fail on
// one product at a time.
func (s *QCService) ScanOrder(actor Actor, ref OrderScanRef) (*QCOrderScan, error) {
	id, err := s.resolveOrderID(ref)
	if err != nil {
		return nil, err
	}
	orders, err := s.repo.Order.FindByIDsWithSeller([]uint{id})
	if err != nil {
		return nil, apperr.Internal("Không tra cứu được dữ liệu").Wrap(err)
	}
	o := orders[id]
	if o == nil {
		return nil, apperr.NotFound("Không tìm thấy đơn nào khớp mã vừa quét")
	}
	// Only approved work reaches production, and a cancelled order is never
	// produced — same cut as the QC-results list (qcResultBase).
	if itemCancelled(o.CancellationStatus) {
		return nil, apperr.Conflict("Đơn đã huỷ, không thể QC")
	}
	if o.ReviewStatus != models.ReviewApproved {
		return nil, apperr.Conflict("Đơn chưa được duyệt nên chưa vào sản xuất — chưa có gì để QC")
	}
	rows, err := s.buildResultOrders([]uint{id})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, apperr.NotFound("Không tìm thấy đơn nào khớp mã vừa quét")
	}
	s.audit.Log(actor, "QC_SCAN_ORDER", "order", &o.ID, "Scanned order "+o.InternalCode+" for QC", nil)
	return &QCOrderScan{QCResultOrder: rows[0], OrderNote: strings.TrimSpace(o.Note)}, nil
}

func (s *QCService) resolveOrderID(ref OrderScanRef) (uint, error) {
	if ref.OrderID != nil {
		return *ref.OrderID, nil
	}
	code := strings.TrimSpace(ref.Code)
	if code == "" {
		return 0, apperr.BadRequest("Hãy quét hoặc nhập mã đơn")
	}
	id, found, err := s.repo.Order.IDByInternalCode(code)
	if err != nil {
		return 0, apperr.Internal("Không tra cứu được dữ liệu").Wrap(err)
	}
	if found {
		return id, nil
	}
	// Staff also read the seller's order number off the paperwork. Accept it only
	// when it points at exactly one order — two orders sharing a store id must not
	// be guessed between.
	refs, err := s.repo.Order.RefsByStoreOrderIDs([]string{code})
	if err != nil {
		return 0, apperr.Internal("Không tra cứu được dữ liệu").Wrap(err)
	}
	switch len(refs) {
	case 0:
		return 0, apperr.NotFound("Không tìm thấy đơn nào khớp mã vừa quét")
	case 1:
		return refs[0].ID, nil
	default:
		return 0, apperr.Conflict("Mã này khớp nhiều đơn — quét mã đơn nội bộ trên tem để chọn đúng đơn")
	}
}
