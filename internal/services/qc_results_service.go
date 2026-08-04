package services

import (
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

	Status       string `json:"status"` // DONE | PARTIAL | NONE
	// Shipping side of the same order. The ship queue and the journey screen are
	// both built on this endpoint, so they need to see whether an order has
	// already gone to THE and what its parcel is doing.
	SellerStatus   models.SellerStatus   `json:"seller_status"`
	HandedOver     bool                  `json:"handed_over"`
	TrackingNumber string                `json:"tracking_number,omitempty"`
	TrackingStatus models.TrackingStatus `json:"tracking_status,omitempty"`
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

	orders, err := s.repo.Order.FindByIDsWithSeller(ids)
	if err != nil {
		return nil, summary, 0, apperr.Internal("could not load orders").Wrap(err)
	}
	lines, err := s.repo.OrderItem.QCResultLines(ids)
	if err != nil {
		return nil, summary, 0, apperr.Internal("could not load order lines").Wrap(err)
	}
	itemIDs := make([]uint, 0, len(lines))
	for i := range lines {
		itemIDs = append(itemIDs, lines[i].ID)
	}
	checks, err := s.repo.QC.LatestChecks(itemIDs)
	if err != nil {
		return nil, summary, 0, apperr.Internal("could not load QC records").Wrap(err)
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

	for i := range lines {
		line := &lines[i]
		parent := byOrder[line.OrderID]
		if parent == nil {
			continue
		}
		item := QCResultItem{
			ItemID: line.ID, InternalCode: line.InternalCode, SKUCode: line.SKUCode,
			ProductName: line.ProductName, Quantity: line.Quantity,
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

	for i := range out {
		o := &out[i]
		switch {
		case o.TotalItems > 0 && o.PassedItems == o.TotalItems:
			o.Status = QCOrderDone
		case o.PassedItems > 0:
			o.Status = QCOrderPartial
		default:
			o.Status = QCOrderNone
		}
	}
	return out, summary, total, nil
}
