package services

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Nút "Bảo hành" (task #3092). Pressing it never touches the old order: status
// only moves forward, and the old order is the evidence for a claim against the
// carrier. It creates a NEW order in PENDING_REVIEW, linked to the old one by
// parent_order_id / root_order_id / warranty_seq, with a record of the answers
// the form asked for (models.OrderWarranty). The choices anh Kiên has not
// settled live in WarrantyPolicy (warranty_policy.go).

// WarrantyItemInput picks one product (and how many of it) for a partial re-ship.
type WarrantyItemInput struct {
	ItemID   uint `json:"item_id" binding:"required"`
	Quantity int  `json:"quantity"` // 0 = the whole line
}

// WarrantyAddressInput is the corrected recipient address; empty fields keep the
// old value.
type WarrantyAddressInput struct {
	ShippingName     string `json:"shipping_name"`
	ShippingAddress1 string `json:"shipping_address1"`
	ShippingAddress2 string `json:"shipping_address2"`
	ShippingCity     string `json:"shipping_city"`
	ShippingZip      string `json:"shipping_zip"`
	ShippingProvince string `json:"shipping_province"`
	ShippingCountry  string `json:"shipping_country"`
	ShippingPhone    string `json:"shipping_phone"`
}

// CreateWarrantyInput is the body of POST /orders/:id/warranty.
type CreateWarrantyInput struct {
	Reason     models.WarrantyReason `json:"reason"`
	ReasonNote string                `json:"reason_note"`
	// Items empty = send everything back out; otherwise only these.
	Items []WarrantyItemInput `json:"items"`
	// Address nil = keep the old address.
	Address *WarrantyAddressInput `json:"address"`
}

// WarrantyInfo tells the screen whether the button works for an order, and what
// the next warranty order would be called.
type WarrantyInfo struct {
	// RoleAllowed: the caller's role may press the button at all. False = the
	// screen shows nothing; true with CanWarranty false = a greyed button + Reason.
	RoleAllowed bool   `json:"role_allowed"`
	CanWarranty bool   `json:"can_warranty"`
	Reason      string `json:"reason,omitempty"` // why not, when CanWarranty is false
	// Blocking is the unfinished warranty order that blocks a second one.
	Blocking        *WarrantyRef `json:"blocking,omitempty"`
	CanOverride     bool         `json:"can_override"`
	NextSeq         int          `json:"next_seq"`
	NextStoreOrder  string       `json:"next_store_order_id"`
	BillSeller      bool         `json:"bill_seller"`
	RootOrderID     uint         `json:"root_order_id"`
	WarrantyCount   int          `json:"warranty_count"`
	ReturnedStatuse []string     `json:"returned_tracking_statuses"`
}

// WarrantyRef points at an order.
type WarrantyRef struct {
	ID           uint   `json:"id"`
	InternalCode string `json:"internal_code"`
	StoreOrderID string `json:"store_order_id"`
}

// WarrantyEntry is one row of "các đơn bảo hành": the order and why it exists.
type WarrantyEntry struct {
	Order    models.Order          `json:"order"`
	Warranty *models.OrderWarranty `json:"warranty,omitempty"`
}

func (s *OrderService) loadWarrantyParent(id uint) (*models.Order, error) {
	o, err := s.repo.Order.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Order not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return o, nil
}

func warrantyRoot(o *models.Order) uint {
	if o.RootOrderID != nil {
		return *o.RootOrderID
	}
	return o.ID
}

// WarrantyInfo answers GET /orders/:id/warranty-info.
func (s *OrderService) WarrantyInfo(actor Actor, id uint) (*WarrantyInfo, error) {
	o, err := s.loadWarrantyParent(id)
	if err != nil {
		return nil, err
	}
	root := warrantyRoot(o)
	maxSeq, err := s.repo.Warranty.MaxSeq(root)
	if err != nil {
		return nil, apperr.Internal("warranty lookup failed").Wrap(err)
	}
	rootStore, err := s.rootStoreOrderID(o, root)
	if err != nil {
		return nil, err
	}
	pol := WarrantyPolicy
	info := &WarrantyInfo{
		NextSeq: maxSeq + 1, NextStoreOrder: pol.Code(rootStore, maxSeq+1),
		BillSeller: pol.BillSeller, RootOrderID: root, CanOverride: pol.CanOverride(actor.Role),
		RoleAllowed: pol.CanCreate(actor.Role),
	}
	for _, t := range pol.ReturnedTrackingStatuses {
		info.ReturnedStatuse = append(info.ReturnedStatuse, string(t))
	}
	if counts, cerr := s.repo.Warranty.Counts([]uint{o.ID}); cerr == nil {
		info.WarrantyCount = counts[o.ID]
	}
	if !pol.CanCreate(actor.Role) {
		info.Reason = "Vai trò của bạn không được bấm Bảo hành"
		return info, nil
	}
	if ok, why := pol.Eligible(o); !ok {
		info.Reason = why
		return info, nil
	}
	open, err := s.repo.Warranty.OpenInChain(root, pol.ReturnedTrackingStatuses)
	if err != nil {
		return nil, apperr.Internal("warranty lookup failed").Wrap(err)
	}
	if open != nil {
		info.Blocking = &WarrantyRef{ID: open.ID, InternalCode: open.InternalCode, StoreOrderID: open.StoreOrderID}
		if !pol.CanOverride(actor.Role) {
			info.Reason = fmt.Sprintf("Đơn %s đang bảo hành, chưa giao xong — chưa tạo thêm được", open.StoreOrderID)
			return info, nil
		}
	}
	info.CanWarranty = true
	return info, nil
}

// rootStoreOrderID is the store order id of the chain's first order — the base of
// every warranty code, so a child of a child is SON1-BH2, never SON1-BH1-BH2.
func (s *OrderService) rootStoreOrderID(o *models.Order, root uint) (string, error) {
	if root == o.ID {
		return o.StoreOrderID, nil
	}
	var r models.Order
	if err := s.repo.DB.Unscoped().Select("id", "store_order_id").First(&r, root).Error; err != nil {
		return "", apperr.Internal("root order lookup failed").Wrap(err)
	}
	return r.StoreOrderID, nil
}

var warrantyReasonLabel = map[models.WarrantyReason]string{
	models.WarrantyReasonShippingDamage: "vỡ do vận chuyển",
	models.WarrantyReasonWrongItem:      "sai hàng",
	models.WarrantyReasonMissingItem:    "thiếu hàng",
	models.WarrantyReasonOther:          "khác",
}

// CreateWarranty makes the warranty order. POST /orders/:id/warranty
func (s *OrderService) CreateWarranty(actor Actor, id uint, in CreateWarrantyInput) (*models.Order, error) {
	pol := WarrantyPolicy
	if !pol.CanCreate(actor.Role) {
		return nil, apperr.Forbidden("Vai trò của bạn không được bấm Bảo hành")
	}
	if !in.Reason.Valid() {
		return nil, apperr.Unprocessable("Chọn lý do bảo hành (vỡ do vận chuyển / sai hàng / thiếu hàng / khác)")
	}
	in.ReasonNote = strings.TrimSpace(in.ReasonNote)
	if in.Reason == models.WarrantyReasonOther && in.ReasonNote == "" {
		return nil, apperr.Unprocessable("Lý do \"khác\" cần ghi chú")
	}

	var created *models.Order
	err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		parent, err := txRepo.Order.FindByID(id)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.NotFound("Order not found")
			}
			return err
		}
		root := warrantyRoot(parent)
		// Serialise the chain first; every check below must see a settled state.
		if err := txRepo.Warranty.LockRoot(root); err != nil {
			return err
		}
		if ok, why := pol.Eligible(parent); !ok {
			return apperr.Conflict(why)
		}
		if open, err := txRepo.Warranty.OpenInChain(root, pol.ReturnedTrackingStatuses); err != nil {
			return err
		} else if open != nil && !pol.CanOverride(actor.Role) {
			return apperr.Conflict(fmt.Sprintf("Đơn %s đang bảo hành, chưa giao xong — chưa tạo thêm được", open.StoreOrderID))
		}

		picked, scope, err := pickWarrantyItems(parent, in.Items)
		if err != nil {
			return err
		}
		maxSeq, err := txRepo.Warranty.MaxSeq(root)
		if err != nil {
			return err
		}
		seq := maxSeq + 1
		rootStore := parent.StoreOrderID
		if root != parent.ID {
			var r models.Order
			if err := tx.Unscoped().Select("id", "store_order_id").First(&r, root).Error; err != nil {
				return err
			}
			rootStore = r.StoreOrderID
		}
		storeID := pol.Code(rootStore, seq)

		now := time.Now()
		orderDate := AppDateString(now)
		daily, err := txRepo.Order.NextDailySeq(orderDate, now)
		if err != nil {
			return err
		}
		parentID := parent.ID
		order := &models.Order{
			InternalCode: fmt.Sprintf("TMP-BH-%d-%d", parent.ID, now.UnixNano()),
			StoreOrderID: storeID, StoreOrderRef: storeID,
			SellerID: parent.SellerID, StoreID: parent.StoreID, StoreName: parent.StoreName, Account: parent.Account,
			ShippingName: parent.ShippingName, ShippingAddress1: parent.ShippingAddress1, ShippingAddress2: parent.ShippingAddress2,
			ShippingCity: parent.ShippingCity, ShippingZip: parent.ShippingZip, ShippingProvince: parent.ShippingProvince,
			ShippingCountry: parent.ShippingCountry, ShippingPhone: parent.ShippingPhone,
			SellerStatus: models.SellerStatusProduction,
			// Straight into "chờ duyệt", like any new order.
			ReviewStatus: models.ReviewPending, CancellationStatus: models.CancellationNone,
			TrackingStatus: models.TrackingNone,
			OrderDate:      orderDate, DailySeq: daily, CreatedByID: actor.IDPtr(),
			ParentOrderID: &parentID, RootOrderID: &root, WarrantySeq: seq,
		}
		addressChanged, err := applyWarrantyAddress(order, in.Address)
		if err != nil {
			return err
		}
		order.Note = warrantyOrderNote(seq, in, parent.Note)

		if err := txRepo.Order.Create(order); err != nil {
			return err
		}
		order.InternalCode = internalBaseCode(order.ID)
		if err := tx.Model(&models.Order{}).Where("id = ?", order.ID).Update("internal_code", order.InternalCode).Error; err != nil {
			return err
		}

		items := make([]models.OrderItem, 0, len(picked))
		for i, p := range picked {
			src := p.item
			items = append(items, models.OrderItem{
				OrderID: order.ID, LineNo: i + 1, InternalCode: itemInternalCode(order.ID, i+1, len(picked)),
				SKUID: src.SKUID, SKUCode: src.SKUCode, Quantity: p.qty,
				DesignURL: src.DesignURL, BackDesignURL: src.BackDesignURL, MockupURL: src.MockupURL,
				EngraveText: src.EngraveText, ImageCode: src.ImageCode, QCDescription: src.QCDescription,
				// Production files: the batch and print/cut stations read them from the item.
				PrintFileURL: src.PrintFileURL, CutFileURL: src.CutFileURL, ProductionFileName: src.ProductionFileName,
				InternalStatus: models.StatusPending, DesignStatus: models.DesignReady,
			})
		}
		if err := tx.Create(&items).Error; err != nil {
			return err
		}
		assets, notes := itemAssetsAndNotes(items, actor)
		if len(assets) > 0 {
			if err := tx.Create(&assets).Error; err != nil {
				return err
			}
		}
		if len(notes) > 0 {
			if err := tx.Create(&notes).Error; err != nil {
				return err
			}
		}
		rec := &models.OrderWarranty{
			OrderID: order.ID, ParentOrderID: parent.ID, RootOrderID: root, Seq: seq,
			Reason: in.Reason, ReasonNote: in.ReasonNote,
			Scope: scope, AddressChanged: addressChanged, BillSeller: pol.BillSeller,
			SourceSellerStatus: parent.SellerStatus, SourceTrackingStatus: parent.TrackingStatus,
			SourceTrackingNumber: parent.TrackingNumber, CreatedByID: actor.IDPtr(),
		}
		if err := txRepo.Warranty.Create(rec); err != nil {
			return err
		}
		created = order
		return nil
	})
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			return nil, err
		}
		return nil, apperr.Internal("could not create warranty order").Wrap(err)
	}
	s.audit.Log(actor, "ORDER_WARRANTY", "order", &created.ID,
		fmt.Sprintf("Bảo hành đơn #%d → %s (%s)", id, created.StoreOrderID, created.InternalCode),
		models.JSONMap{"parent_order_id": id, "warranty_seq": created.WarrantySeq,
			"reason": string(in.Reason)})
	return s.GetOrder(created.ID)
}

type pickedItem struct {
	item *models.OrderItem
	qty  int
}

// pickWarrantyItems resolves which products go out again: every live line of the
// parent, or the lines (and counts) the form chose.
func pickWarrantyItems(parent *models.Order, in []WarrantyItemInput) ([]pickedItem, models.WarrantyScope, error) {
	live := activeOrderItems(parent.Items)
	if len(live) == 0 {
		return nil, "", apperr.Conflict("Đơn không còn sản phẩm nào để bảo hành")
	}
	byID := map[uint]*models.OrderItem{}
	for i := range live {
		byID[live[i].ID] = &live[i]
	}
	if len(in) == 0 {
		out := make([]pickedItem, 0, len(live))
		for i := range live {
			out = append(out, pickedItem{item: &live[i], qty: maxInt(live[i].Quantity, 1)})
		}
		return out, models.WarrantyScopeFull, nil
	}
	seen := map[uint]bool{}
	out := make([]pickedItem, 0, len(in))
	full := true
	for _, w := range in {
		it, ok := byID[w.ItemID]
		if !ok {
			return nil, "", apperr.BadRequest("Sản phẩm không thuộc đơn này (hoặc đã huỷ)")
		}
		if seen[w.ItemID] {
			return nil, "", apperr.BadRequest("Sản phẩm bị chọn hai lần")
		}
		seen[w.ItemID] = true
		have := maxInt(it.Quantity, 1)
		qty := w.Quantity
		if qty == 0 {
			qty = have
		}
		if qty < 1 || qty > have {
			return nil, "", apperr.Unprocessable(fmt.Sprintf("Số lượng bảo hành của %s phải từ 1 đến %d", it.InternalCode, have))
		}
		if qty != have {
			full = false
		}
		out = append(out, pickedItem{item: it, qty: qty})
	}
	if len(out) != len(live) {
		full = false
	}
	if full {
		return out, models.WarrantyScopeFull, nil
	}
	return out, models.WarrantyScopePartial, nil
}

// applyWarrantyAddress: nil = keep the old address. Otherwise the form is the
// whole new address, applied as sent (so Địa chỉ 2, phone… can be cleared). The
// fields an ordinary order also requires — name, address 1, country — must stay
// filled. Reports whether any value changed.
func applyWarrantyAddress(o *models.Order, a *WarrantyAddressInput) (bool, error) {
	if a == nil {
		return false, nil
	}
	n := WarrantyAddressInput{
		ShippingName: strings.TrimSpace(a.ShippingName), ShippingAddress1: strings.TrimSpace(a.ShippingAddress1),
		ShippingAddress2: strings.TrimSpace(a.ShippingAddress2), ShippingCity: strings.TrimSpace(a.ShippingCity),
		ShippingZip: strings.TrimSpace(a.ShippingZip), ShippingProvince: strings.TrimSpace(a.ShippingProvince),
		ShippingCountry: strings.TrimSpace(a.ShippingCountry), ShippingPhone: strings.TrimSpace(a.ShippingPhone),
	}
	if n.ShippingName == "" || n.ShippingAddress1 == "" || n.ShippingCountry == "" {
		return false, apperr.Unprocessable("Địa chỉ mới cần có người nhận, địa chỉ 1 và quốc gia")
	}
	changed := n.ShippingName != o.ShippingName || n.ShippingAddress1 != o.ShippingAddress1 ||
		n.ShippingAddress2 != o.ShippingAddress2 || n.ShippingCity != o.ShippingCity ||
		n.ShippingZip != o.ShippingZip || n.ShippingProvince != o.ShippingProvince ||
		n.ShippingCountry != o.ShippingCountry || n.ShippingPhone != o.ShippingPhone
	o.ShippingName, o.ShippingAddress1, o.ShippingAddress2, o.ShippingCity = n.ShippingName, n.ShippingAddress1, n.ShippingAddress2, n.ShippingCity
	o.ShippingZip, o.ShippingProvince, o.ShippingCountry, o.ShippingPhone = n.ShippingZip, n.ShippingProvince, n.ShippingCountry, n.ShippingPhone
	return changed, nil
}

func warrantyOrderNote(seq int, in CreateWarrantyInput, parentNote string) string {
	n := fmt.Sprintf("BẢO HÀNH lần %d — %s", seq, warrantyReasonLabel[in.Reason])
	if in.ReasonNote != "" {
		n += ": " + in.ReasonNote
	}
	if p := strings.TrimSpace(parentNote); p != "" {
		n += " | " + p
	}
	if r := []rune(n); len(r) > 1000 {
		n = string(r[:1000])
	}
	return n
}

// ListWarranties answers GET /orders/:id/warranties: the warranty orders under an
// order, each with the record of why.
func (s *OrderService) ListWarranties(id uint) ([]WarrantyEntry, error) {
	if _, err := s.loadWarrantyParent(id); err != nil {
		return nil, err
	}
	rows, err := s.repo.Warranty.Chain(id)
	if err != nil {
		return nil, apperr.Internal("warranty lookup failed").Wrap(err)
	}
	ids := make([]uint, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	recs, err := s.repo.Warranty.ByOrderIDs(ids)
	if err != nil {
		return nil, apperr.Internal("warranty lookup failed").Wrap(err)
	}
	out := make([]WarrantyEntry, 0, len(rows))
	for _, r := range rows {
		e := WarrantyEntry{Order: r}
		if w, ok := recs[r.ID]; ok {
			w := w
			e.Warranty = &w
		}
		out = append(out, e)
	}
	return out, nil
}

// annotateWarrantyCounts fills Order.WarrantyCount ("Đã bảo hành ×N").
func annotateWarrantyCounts(repo *repositories.Repositories, orders []*models.Order) error {
	ids := make([]uint, 0, len(orders))
	for _, o := range orders {
		if o != nil {
			ids = append(ids, o.ID)
		}
	}
	counts, err := repo.Warranty.Counts(ids)
	if err != nil {
		return err
	}
	for _, o := range orders {
		if o != nil {
			o.WarrantyCount = counts[o.ID]
		}
	}
	return nil
}
