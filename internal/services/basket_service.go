package services

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// BasketService runs the waiting baskets (giỏ chờ).
//
// The floor's flow (anh Kiên, 20/10/2026):
//  1. An order has several products made from different materials, so they finish
//     at different times. The first one to pass QC goes into a basket: the QC
//     operator presses "Tạo giỏ chờ", the system gives the LOWEST free number
//     (1..100) and prints a basket label to put with the goods.
//  2. Every later product of that order shows its basket number as soon as it is
//     scanned, so the operator knows where its siblings are waiting.
//  3. When the last product passes QC the basket is FULL: the picker takes that
//     product to the basket, counts the order, bundles it, and confirms with one
//     more scan. Only that confirmation frees the number — and only an order whose
//     basket is released can be sent on, so whatever reaches packing is complete.
//
// A basket's state is never stored — it is read from the order every time: OPEN
// while some live product has not passed QC, FULL once all have, CANCELLED when
// the order (or every product in it) was cancelled. A QC fail, an undo or a
// cancellation therefore moves the basket by itself, with no hook to forget.
type BasketService struct {
	repo  *repositories.Repositories
	audit *AuditService
	qc    *QCService
}

// Basket states, derived from the order (see BasketService).
const (
	BasketOpen      = "OPEN"      // đang gom: còn sản phẩm chưa QC đạt
	BasketFull      = "FULL"      // đủ hàng: chờ bó + quét xác nhận
	BasketCancelled = "CANCELLED" // đơn / mọi sản phẩm đã huỷ: bỏ hàng ra, thả giỏ
)

// BasketView is one basket with its order and every product's QC state.
type BasketView struct {
	ID           uint       `json:"id"`
	Number       int        `json:"number"`
	Code         string     `json:"code"` // mã in trên tem giỏ, vd "GIO-37"
	Status       string     `json:"status"`
	OrderID      uint       `json:"order_id"`
	OrderCode    string     `json:"order_code"`
	StoreOrderID string     `json:"store_order_id"`
	SellerName   string     `json:"seller_name"`
	TotalItems   int        `json:"total_items"`
	PassedItems  int        `json:"passed_items"`
	TotalQty     int        `json:"total_qty"`
	CreatedAt    time.Time  `json:"created_at"`
	ReleasedAt   *time.Time `json:"released_at,omitempty"`
	ReleaseNote  string     `json:"release_note,omitempty"`
	// Items are the order's live products — what has to be in the bundle.
	Items []QCResultItem `json:"items"`
}

// BasketCode is the text encoded in a basket label's QR.
func BasketCode(n int) string { return fmt.Sprintf("GIO-%d", n) }

// basketCodeRe reads a scanned basket label: "GIO-37", "gio37", "GIỎ 37", "G37",
// or just the number "37" (a basket number has at most 3 digits, an order code 6).
var basketCodeRe = regexp.MustCompile(`(?i)^(?:gi[oỏ]|g)?[\s\-_#]*(\d{1,3})$`)

func parseBasketCode(code string) (int, bool) {
	m := basketCodeRe.FindStringSubmatch(strings.TrimSpace(code))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 || n > models.MaxBaskets {
		return 0, false
	}
	return n, true
}

// views builds BasketViews for live (or released) baskets in one pass: orders,
// their lines and QC verdicts in a fixed number of statements.
func (s *BasketService) views(baskets []models.Basket) ([]BasketView, error) {
	if len(baskets) == 0 {
		return []BasketView{}, nil
	}
	ids := make([]uint, 0, len(baskets))
	for _, b := range baskets {
		ids = append(ids, b.OrderID)
	}
	rows, err := s.qc.buildResultOrders(ids)
	if err != nil {
		return nil, err
	}
	byOrder := map[uint]*QCResultOrder{}
	for i := range rows {
		byOrder[rows[i].OrderID] = &rows[i]
	}
	orders, err := s.repo.Order.FindByIDsWithSeller(ids)
	if err != nil {
		return nil, apperr.Internal("Không tải được đơn của giỏ").Wrap(err)
	}
	out := make([]BasketView, 0, len(baskets))
	for _, b := range baskets {
		v := BasketView{
			ID: b.ID, Number: b.Number, Code: BasketCode(b.Number), OrderID: b.OrderID,
			CreatedAt: b.CreatedAt, ReleasedAt: b.ReleasedAt, ReleaseNote: b.ReleaseNote,
			Items: []QCResultItem{},
		}
		if r := byOrder[b.OrderID]; r != nil {
			v.OrderCode, v.StoreOrderID, v.SellerName = r.InternalCode, r.StoreOrderID, r.SellerName
			v.TotalItems, v.PassedItems, v.Items = r.TotalItems, r.PassedItems, r.Items
			for _, it := range r.Items {
				v.TotalQty += it.Quantity
			}
		}
		o := orders[b.OrderID]
		switch {
		case o == nil || itemCancelled(o.CancellationStatus) || v.TotalItems == 0:
			v.Status = BasketCancelled
		case v.PassedItems == v.TotalItems:
			v.Status = BasketFull
		default:
			v.Status = BasketOpen
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *BasketService) view(b *models.Basket) (*BasketView, error) {
	vs, err := s.views([]models.Basket{*b})
	if err != nil {
		return nil, err
	}
	return &vs[0], nil
}

// List returns every live basket, lowest number first, plus how many are free.
func (s *BasketService) List() ([]BasketView, error) {
	live, err := s.repo.Basket.ListLive()
	if err != nil {
		return nil, apperr.Internal("Không tải được danh sách giỏ").Wrap(err)
	}
	return s.views(live)
}

// BasketForOrder answers "is this order in a basket, and does it need one" — what
// the QC station shows next to the product being checked.
type BasketForOrder struct {
	OrderID     uint        `json:"order_id"`
	TotalItems  int         `json:"total_items"`
	PassedItems int         `json:"passed_items"`
	Basket      *BasketView `json:"basket"`
	// NeedsBasket: several products, not all passed, and no basket yet.
	NeedsBasket bool `json:"needs_basket"`
}

// ForOrder reports the basket situation of one order.
func (s *BasketService) ForOrder(orderID uint) (*BasketForOrder, error) {
	rows, err := s.qc.buildResultOrders([]uint{orderID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, apperr.NotFound("Không tìm thấy đơn")
	}
	r := rows[0]
	out := &BasketForOrder{OrderID: orderID, TotalItems: r.TotalItems, PassedItems: r.PassedItems}
	b, err := s.repo.Basket.LiveByOrder(orderID)
	if err != nil {
		return nil, apperr.Internal("Không tra được giỏ của đơn").Wrap(err)
	}
	if b != nil {
		if out.Basket, err = s.view(b); err != nil {
			return nil, err
		}
		return out, nil
	}
	out.NeedsBasket = r.TotalItems > 1 && r.PassedItems < r.TotalItems
	return out, nil
}

// Create puts an order into a basket — the lowest free number. Asking again for
// an order that already has one returns that basket (created=false), so a double
// click or two stations pressing at once never hand out two numbers.
func (s *BasketService) Create(actor Actor, orderID uint) (*BasketView, bool, error) {
	orders, err := s.repo.Order.FindByIDsWithSeller([]uint{orderID})
	if err != nil {
		return nil, false, apperr.Internal("Không tải được đơn").Wrap(err)
	}
	o := orders[orderID]
	if o == nil {
		return nil, false, apperr.NotFound("Không tìm thấy đơn")
	}
	if itemCancelled(o.CancellationStatus) {
		return nil, false, apperr.Conflict("Đơn đã huỷ — không cần giỏ chờ")
	}
	if o.ReviewStatus != models.ReviewApproved {
		return nil, false, apperr.Conflict("Đơn chưa được duyệt nên chưa vào sản xuất — chưa cần giỏ chờ")
	}
	if b, err := s.repo.Basket.LiveByOrder(orderID); err != nil {
		return nil, false, apperr.Internal("Không tra được giỏ của đơn").Wrap(err)
	} else if b != nil {
		v, err := s.view(b)
		return v, false, err
	}
	rows, err := s.qc.buildResultOrders([]uint{orderID})
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 || rows[0].TotalItems < 2 {
		return nil, false, apperr.Conflict("Đơn " + o.InternalCode + " chỉ có 1 sản phẩm — không cần giỏ chờ")
	}
	if rows[0].PassedItems >= rows[0].TotalItems {
		return nil, false, apperr.Conflict("Đơn " + o.InternalCode + " đã QC đạt đủ sản phẩm — không cần giỏ chờ, đem đi đóng hàng luôn")
	}

	// Lowest free number. Two stations can pick the same number at the same
	// instant; the partial unique index rejects the second insert and it simply
	// tries again with the numbers as they are now.
	for attempt := 0; attempt < 5; attempt++ {
		used, err := s.repo.Basket.LiveNumbers()
		if err != nil {
			return nil, false, apperr.Internal("Không đọc được giỏ đang dùng").Wrap(err)
		}
		n := lowestFree(used, models.MaxBaskets)
		if n == 0 {
			return nil, false, apperr.Conflict(fmt.Sprintf(
				"Hết giỏ trống — cả %d giỏ đang dùng. Nhặt các giỏ đã đủ hàng (màn Giỏ chờ) rồi tạo lại", models.MaxBaskets))
		}
		b := &models.Basket{Number: n, OrderID: orderID, CreatedByID: actor.IDPtr()}
		if err := s.repo.Basket.Create(b); err != nil {
			// Lost a race. If it was for the same ORDER, the other station already
			// made its basket — that is the answer.
			if existing, _ := s.repo.Basket.LiveByOrder(orderID); existing != nil {
				v, err := s.view(existing)
				return v, false, err
			}
			continue
		}
		s.audit.Log(actor, "BASKET_CREATE", "order", &orderID,
			fmt.Sprintf("Đơn %s vào giỏ chờ số %d", o.InternalCode, n), nil)
		v, err := s.view(b)
		return v, true, err
	}
	return nil, false, apperr.Conflict("Nhiều người đang tạo giỏ cùng lúc — bấm lại")
}

func lowestFree(used []int, max int) int {
	taken := make(map[int]bool, len(used))
	for _, n := range used {
		taken[n] = true
	}
	for n := 1; n <= max; n++ {
		if !taken[n] {
			return n
		}
	}
	return 0
}

// BasketLookup is what one scan at the basket screen finds.
type BasketLookup struct {
	// Basket is set when the code named a live basket, or an order sitting in one.
	Basket *BasketView `json:"basket"`
	// Order is set when the code named an order (or one of its products) that has
	// no basket: what it holds, so the screen can offer "Tạo giỏ chờ".
	Order       *QCResultOrder `json:"order,omitempty"`
	NeedsBasket bool           `json:"needs_basket"`
	Message     string         `json:"message,omitempty"`
}

// Lookup resolves whatever the basket screen's scanner read: a basket label, an
// order code / store order id, or a product tem.
func (s *BasketService) Lookup(code string) (*BasketLookup, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, apperr.BadRequest("Hãy quét mã giỏ, mã đơn hoặc mã tem sản phẩm")
	}
	if n, ok := parseBasketCode(code); ok {
		b, err := s.repo.Basket.LiveByNumber(n)
		if err != nil {
			return nil, apperr.Internal("Không tra được giỏ").Wrap(err)
		}
		if b == nil {
			return nil, apperr.NotFound(fmt.Sprintf("Giỏ số %d đang trống — không có đơn nào trong giỏ này", n))
		}
		v, err := s.view(b)
		if err != nil {
			return nil, err
		}
		return &BasketLookup{Basket: v}, nil
	}

	var orderID uint
	if strings.ContainsAny(code, "_/") {
		id, found, err := s.repo.OrderItem.OrderIDByCode(code)
		if err != nil {
			return nil, apperr.Internal("Không tra cứu được dữ liệu").Wrap(err)
		}
		if !found {
			return nil, apperr.NotFound("Không tìm thấy sản phẩm nào khớp mã vừa quét")
		}
		orderID = id
	} else {
		id, err := s.qc.resolveOrderID(OrderScanRef{Code: code})
		if err != nil {
			return nil, err
		}
		orderID = id
	}

	fo, err := s.ForOrder(orderID)
	if err != nil {
		return nil, err
	}
	if fo.Basket != nil {
		return &BasketLookup{Basket: fo.Basket}, nil
	}
	rows, err := s.qc.buildResultOrders([]uint{orderID})
	if err != nil {
		return nil, err
	}
	out := &BasketLookup{NeedsBasket: fo.NeedsBasket}
	if len(rows) > 0 {
		out.Order = &rows[0]
	}
	switch {
	case fo.NeedsBasket:
		out.Message = "Đơn chưa có giỏ chờ"
	case fo.TotalItems <= 1:
		out.Message = "Đơn chỉ có 1 sản phẩm — không cần giỏ chờ"
	default:
		out.Message = "Đơn đã QC đạt đủ sản phẩm và không nằm trong giỏ nào"
	}
	return out, nil
}

// ReleaseInput confirms a basket was bundled (or frees it by force).
type ReleaseInput struct {
	// Force frees a basket whose order is still incomplete — admin only, with a
	// reason (the goods were taken out for some other reason).
	Force  bool   `json:"force"`
	Reason string `json:"reason"`
}

// Release frees a basket after the picker bundled its order — the "quét xác nhận"
// step. Allowed when the order is complete (FULL) or cancelled; an incomplete
// basket can only be forced free by an admin, with a reason.
func (s *BasketService) Release(actor Actor, id uint, in ReleaseInput) (*BasketView, error) {
	b, err := s.repo.Basket.FindByID(id)
	if err != nil {
		return nil, apperr.Internal("Không tra được giỏ").Wrap(err)
	}
	if b == nil {
		return nil, apperr.NotFound("Không tìm thấy giỏ")
	}
	if b.ReleasedAt != nil {
		return nil, apperr.Conflict(fmt.Sprintf("Giỏ số %d đã được thả rồi", b.Number))
	}
	v, err := s.view(b)
	if err != nil {
		return nil, err
	}
	note := "Đã bó đủ hàng"
	switch v.Status {
	case BasketFull:
	case BasketCancelled:
		note = "Đơn đã huỷ — bỏ hàng ra khỏi giỏ"
	default:
		if !in.Force {
			return nil, apperr.Conflict(fmt.Sprintf(
				"Giỏ số %d chưa đủ hàng: mới QC đạt %d/%d sản phẩm — chưa thả được", b.Number, v.PassedItems, v.TotalItems))
		}
		if !actor.Role.HasAdminRights() {
			return nil, apperr.Forbidden("Chỉ Admin / Chủ sở hữu được thả giỏ khi đơn chưa đủ hàng")
		}
		reason := strings.TrimSpace(in.Reason)
		if reason == "" {
			return nil, apperr.BadRequest("Nhập lý do thả giỏ khi đơn chưa đủ hàng")
		}
		if len([]rune(reason)) > 200 {
			return nil, apperr.BadRequest("Lý do tối đa 200 ký tự")
		}
		note = "Thả khi chưa đủ: " + reason
	}
	now := time.Now()
	n, err := s.repo.Basket.Release(b.ID, actor.IDPtr(), note, now)
	if err != nil {
		return nil, apperr.Internal("Không thả được giỏ").Wrap(err)
	}
	if n == 0 {
		return nil, apperr.Conflict(fmt.Sprintf("Giỏ số %d vừa được người khác thả", b.Number))
	}
	s.audit.Log(actor, "BASKET_RELEASE", "order", &b.OrderID,
		fmt.Sprintf("Thả giỏ chờ số %d của đơn %s — %s", b.Number, v.OrderCode, note), nil)
	v.ReleasedAt, v.ReleaseNote = &now, note
	return v, nil
}

// basketBlockReason refuses to send an order that is still sitting in a basket:
// the picker has not confirmed the bundle, so nobody has checked the order is
// physically complete. Empty when the order is clear to go.
func basketBlockReason(repo *repositories.Repositories, orderID uint) string {
	b, err := repo.Basket.LiveByOrder(orderID)
	if err != nil {
		return "Không kiểm tra được giỏ chờ của đơn — thử lại"
	}
	if b == nil {
		return ""
	}
	return fmt.Sprintf("Đơn đang ở giỏ chờ số %d — ra giỏ bó đủ hàng rồi quét xác nhận ở màn Giỏ chờ trước khi gửi", b.Number)
}
