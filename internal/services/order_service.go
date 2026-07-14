package services

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// OrderService covers orders, items, the design queue and the seller view.
type OrderService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// ---------- Orders ----------

func (s *OrderService) ListOrders(f repositories.OrderFilter) ([]models.Order, int64, error) {
	f.Page = f.Page.Normalize()
	rows, total, err := s.repo.Order.List(f)
	if err != nil {
		return rows, total, err
	}
	if err := annotateStoreOrderDupSlice(s.repo, rows); err != nil {
		return rows, total, err
	}
	for i := range rows {
		rows[i].Items = activeOrderItems(rows[i].Items)
	}
	return rows, total, nil
}

func (s *OrderService) GetOrder(id uint) (*models.Order, error) {
	o, err := s.repo.Order.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Order not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return o, nil
}

// GetOperationalOrder is the internal work view. It deliberately hides
// cancelled line items; seller and audit flows use GetOrder to retain history.
func (s *OrderService) GetOperationalOrder(id uint) (*models.Order, error) {
	o, err := s.GetOrder(id)
	if err != nil {
		return nil, err
	}
	o.Items = activeOrderItems(o.Items)
	return o, nil
}

func activeOrderItems(items []models.OrderItem) []models.OrderItem {
	out := make([]models.OrderItem, 0, len(items))
	for _, item := range items {
		if !itemCancelled(item.CancellationStatus) {
			out = append(out, item)
		}
	}
	return out
}

// ---------- Items ----------

func (s *OrderService) ListItems(f repositories.ItemFilter) ([]models.OrderItem, int64, error) {
	f.Page = f.Page.Normalize()
	rows, total, err := s.repo.OrderItem.List(f)
	if err != nil {
		return rows, total, err
	}
	orders := make([]*models.Order, 0, len(rows))
	for i := range rows {
		if rows[i].Order != nil {
			orders = append(orders, rows[i].Order)
		}
	}
	if err := annotateStoreOrderDup(s.repo, orders); err != nil {
		return rows, total, err
	}
	return rows, total, nil
}

// annotateStoreOrderDup sets Order.StoreOrderDup on every order whose StoreOrderID
// is shared by more than one order for the same seller. Used by list endpoints so
// Orders / Chờ duyệt / seller screens highlight repeated store order ids the same
// way the importer does — with a single extra query, stable across pagination.
func annotateStoreOrderDup(repo *repositories.Repositories, orders []*models.Order) error {
	if len(orders) == 0 {
		return nil
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(orders))
	for _, o := range orders {
		if o == nil || o.StoreOrderID == "" || seen[o.StoreOrderID] {
			continue
		}
		seen[o.StoreOrderID] = true
		ids = append(ids, o.StoreOrderID)
	}
	dup, err := repo.Order.DuplicateStoreOrderIDs(ids)
	if err != nil {
		return err
	}
	for _, o := range orders {
		if o != nil && dup[repositories.StoreOrderDupKey(o.SellerID, o.StoreOrderID)] {
			o.StoreOrderDup = true
		}
	}
	return nil
}

// annotateStoreOrderDupSlice is annotateStoreOrderDup for a value slice: it marks
// StoreOrderDup in place on each element via a pointer into the backing array.
func annotateStoreOrderDupSlice(repo *repositories.Repositories, rows []models.Order) error {
	ptrs := make([]*models.Order, len(rows))
	for i := range rows {
		ptrs[i] = &rows[i]
	}
	return annotateStoreOrderDup(repo, ptrs)
}

func (s *OrderService) GetItem(id uint) (*models.OrderItem, error) {
	it, err := s.repo.OrderItem.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Item not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	if itemCancelled(it.CancellationStatus) {
		return nil, apperr.NotFound("Item not found")
	}
	return it, nil
}

// ---------- Design queue ----------

// DesignQueue lists items that still need design work (mockup/print/cut not ready).
func (s *OrderService) DesignQueue(f repositories.ItemFilter) ([]models.OrderItem, int64, error) {
	f.Page = f.Page.Normalize()
	f.NeedsDesign = true
	f.ReviewApproved = true // only approved orders enter the design flow
	return s.repo.OrderItem.List(f)
}

// UpdateDesignInput updates an item's design assets and production-ready fields.
// Any field left nil is unchanged. Ops/Design use this from the Pending Review
// detail to normalize a seller item into production-ready data before approval.
type UpdateDesignInput struct {
	PrintFileURL *string `json:"print_file_url"`
	CutFileURL   *string `json:"cut_file_url"`
	MockupURL    *string `json:"mockup_url"`
	DesignURL    *string `json:"design_url"`
	SetReady     *bool   `json:"set_ready"`

	// Production-ready fields (legacy production-template columns).
	ImageCode          *string `json:"image_code"`           // Mã ảnh
	QCDescription      *string `json:"qc_description"`       // Mô tả SP để QC
	ProductionSequence *int    `json:"production_sequence"`  // Số thứ tự
	ProductionFileName *string `json:"production_file_name"` // Tên File
}

// UpdateItemDesign saves print/cut/mockup/design URLs, records versioned assets
// and optionally marks the item design-ready. Designers (and ops/admin) use this.
func (s *OrderService) UpdateItemDesign(actor Actor, itemID uint, in UpdateDesignInput) (*models.OrderItem, error) {
	item, err := s.GetItem(itemID)
	if err != nil {
		return nil, err
	}
	if itemCancelled(item.CancellationStatus) {
		return nil, apperr.Conflict("Sản phẩm đã huỷ, không thể tiếp tục thiết kế")
	}

	now := time.Now()
	addAsset := func(assetType, urlStr string) {
		_ = s.repo.DB.Create(&models.ItemAsset{
			OrderItemID: item.ID, AssetType: assetType, URL: urlStr, Version: 1,
			UploadedByID: actor.IDPtr(), UploadedAt: now,
		}).Error
	}

	// A design asset only counts as "touched" when its URL actually changes, so
	// editing production-only fields (image code / QC description / sequence /
	// file name) from the review screen never re-versions assets or disturbs the
	// item's design_status.
	designTouched := false
	if in.PrintFileURL != nil {
		if v := strings.TrimSpace(*in.PrintFileURL); v != item.PrintFileURL {
			item.PrintFileURL = v
			if v != "" {
				addAsset("PRINT_FILE", v)
			}
			designTouched = true
		}
	}
	if in.CutFileURL != nil {
		if v := strings.TrimSpace(*in.CutFileURL); v != item.CutFileURL {
			item.CutFileURL = v
			if v != "" {
				addAsset("CUT_FILE", v)
			}
			designTouched = true
		}
	}
	if in.DesignURL != nil {
		if v := strings.TrimSpace(*in.DesignURL); v != item.DesignURL {
			item.DesignURL = v
			if v != "" {
				addAsset("DESIGN", v)
			}
			designTouched = true
		}
	}
	if in.MockupURL != nil {
		if v := strings.TrimSpace(*in.MockupURL); v != item.MockupURL {
			item.MockupURL = v
			if v != "" {
				addAsset("MOCKUP", v)
			}
			designTouched = true
		}
	}

	// Production-ready fields (no asset history — plain scalar values).
	if in.ImageCode != nil {
		item.ImageCode = strings.TrimSpace(*in.ImageCode)
	}
	if in.QCDescription != nil {
		item.QCDescription = strings.TrimSpace(*in.QCDescription)
	}
	if in.ProductionSequence != nil {
		item.ProductionSequence = *in.ProductionSequence
	}
	if in.ProductionFileName != nil {
		item.ProductionFileName = strings.TrimSpace(*in.ProductionFileName)
	}

	// Re-evaluate design status only when a design asset actually changed.
	if designTouched && item.DesignStatus != models.DesignReady {
		item.DesignStatus = models.DesignInProgress
		if item.MockupURL == "" {
			item.DesignStatus = models.DesignMissing
		}
	}

	if in.SetReady != nil && *in.SetReady {
		if item.PrintFileURL == "" {
			return nil, apperr.Unprocessable("Cannot set design ready: print file is missing")
		}
		if item.MockupURL == "" {
			return nil, apperr.Unprocessable("Cannot set design ready: mockup URL is missing (QC reference required)")
		}
		item.DesignStatus = models.DesignReady
	}

	if err := s.repo.OrderItem.Update(item); err != nil {
		return nil, apperr.Internal("could not update item design").Wrap(err)
	}
	s.audit.Log(actor, "ITEM_DESIGN_UPDATE", "order_item", &item.ID,
		"Updated design for item "+item.InternalCode+" (status="+string(item.DesignStatus)+")", nil)
	return s.GetItem(item.ID)
}

// ---------- Create-batch helpers ----------

func (s *OrderService) MaterialBuckets() ([]repositories.MaterialBucket, error) {
	return s.repo.OrderItem.MaterialBuckets()
}

func (s *OrderService) DesignReadyItemsForMaterial(materialID uint, page repositories.Page) ([]models.OrderItem, int64, error) {
	return s.repo.OrderItem.DesignReadyItemsForMaterial(materialID, page.Normalize())
}

// ---------- Direct create (convenience / TODO) ----------

// TODO(import): the canonical path is Preview + Commit. CreateOrderDirect is a
// thin convenience for manually keying a single order (e.g. CS hot-fix). It does
// NOT run the full file-level dedup/validation pipeline.
type DirectItemInput struct {
	SKUCode     string `json:"sku_code" binding:"required"`
	ProductName string `json:"product_name"`
	VariantCode string `json:"variant_code"`
	Quantity    int    `json:"quantity"`
	ImageCode   string `json:"image_code"`
	MockupURL   string `json:"mockup_url"`
	EngraveText string `json:"engrave_text"`
}

type DirectOrderInput struct {
	SellerID         uint              `json:"seller_id" binding:"required"`
	StoreOrderID     string            `json:"store_order_id" binding:"required"`
	StoreName        string            `json:"store_name"`
	Account          string            `json:"account"`
	ShippingMethod   string            `json:"shipping_method"`
	ShippingName     string            `json:"shipping_name" binding:"required"`
	ShippingAddress1 string            `json:"shipping_address1" binding:"required"`
	ShippingAddress2 string            `json:"shipping_address2"`
	ShippingCity     string            `json:"shipping_city"`
	ShippingZip      string            `json:"shipping_zip"`
	ShippingProvince string            `json:"shipping_province"`
	ShippingCountry  string            `json:"shipping_country" binding:"required"`
	ShippingPhone    string            `json:"shipping_phone"`
	ShippingEmail    string            `json:"shipping_email"`
	IOSS             string            `json:"ioss"`
	Note             string            `json:"note"`
	Items            []DirectItemInput `json:"items" binding:"required,min=1"`
}

func (s *OrderService) CreateOrderDirect(actor Actor, in DirectOrderInput) (*models.Order, error) {
	if _, err := s.repo.Seller.FindByID(in.SellerID); err != nil {
		return nil, apperr.BadRequest("seller_id does not reference an existing seller")
	}
	// StoreOrderID is a repeatable reference label, not a unique key — so no
	// duplicate check here. Every order gets its own system-generated InternalCode.

	var order *models.Order
	err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		order = &models.Order{
			StoreOrderID: in.StoreOrderID, StoreOrderRef: in.StoreOrderID, SellerID: in.SellerID,
			StoreName: in.StoreName, Account: in.Account, ShippingMethod: in.ShippingMethod, ShippingName: in.ShippingName,
			ShippingAddress1: in.ShippingAddress1, ShippingAddress2: in.ShippingAddress2,
			ShippingCity: in.ShippingCity, ShippingZip: in.ShippingZip, ShippingProvince: in.ShippingProvince,
			ShippingCountry: in.ShippingCountry, ShippingPhone: in.ShippingPhone, ShippingEmail: in.ShippingEmail,
			IOSS: in.IOSS, Note: in.Note, SellerStatus: models.SellerStatusProduction,
			// New orders enter operational review before production.
			ReviewStatus: models.ReviewPending, CancellationStatus: models.CancellationNone,
			CreatedByID: actor.IDPtr(),
		}
		if err := txRepo.Order.Create(order); err != nil {
			return err
		}
		order.InternalCode = internalBaseCode(order.ID)
		if err := txRepo.Order.Update(order); err != nil {
			return err
		}
		for i, it := range in.Items {
			skuCode := models.NormalizeCode(it.SKUCode)
			sku, _ := txRepo.SKU.FindByCode(skuCode)
			var skuID *uint
			if sku != nil {
				skuID = &sku.ID
			}
			ds := models.DesignPending
			if it.MockupURL == "" {
				ds = models.DesignMissing
			}
			item := &models.OrderItem{
				OrderID: order.ID, LineNo: i + 1, InternalCode: itemInternalCode(order.ID, i+1, len(in.Items)),
				SKUID: skuID, SKUCode: skuCode, ProductName: it.ProductName, VariantCode: it.VariantCode,
				Quantity: maxInt(it.Quantity, 1), ImageCode: it.ImageCode, MockupURL: it.MockupURL, EngraveText: it.EngraveText,
				InternalStatus: models.StatusPending, DesignStatus: ds,
			}
			if err := tx.Create(item).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, apperr.Internal("could not create order").Wrap(err)
	}
	s.audit.Log(actor, "ORDER_CREATE_DIRECT", "order", &order.ID, "Created order "+order.InternalCode, nil)
	return s.GetOrder(order.ID)
}

// ---------- Seller view ----------

// SellerOrderView is the sanitized, high-level shape sellers may see. It never
// exposes internal print/cut/QC detail.
type SellerOrderView struct {
	ID           uint   `json:"id"`
	InternalCode string `json:"internal_code"`
	StoreOrderID string `json:"store_order_id"`
	// StoreOrderDup: this store order id is shared by more than one order (repeated
	// upload) — the seller UI flags it so they can spot an accidental re-send.
	StoreOrderDup bool                `json:"store_order_dup"`
	StoreName     string              `json:"store_name"`
	Status        models.SellerStatus `json:"status"` // production phase (only meaningful once approved)
	// Review / cancellation state. The seller UI shows the review status until an
	// order is APPROVED, then falls through to the production Status above.
	ReviewStatus       models.ReviewStatus       `json:"review_status"`
	CancellationStatus models.CancellationStatus `json:"cancellation_status"`
	ReviewNote         string                    `json:"review_note,omitempty"`
	// Allowed cancellation action for this order, so the UI can show exactly one
	// of: cancel directly / request cancellation / (nothing).
	CanCancel              bool             `json:"can_cancel"`
	CanRequestCancellation bool             `json:"can_request_cancellation"`
	ItemCount              int              `json:"item_count"`
	CreatedAt              time.Time        `json:"created_at"`
	Items                  []SellerItemView `json:"items,omitempty"`
}

// SellerItemView only exposes product-level facts, not the factory pipeline.
type SellerItemView struct {
	ID                 uint                      `json:"id"`
	SKUCode            string                    `json:"sku_code"`
	ProductName        string                    `json:"product_name"`
	VariantCode        string                    `json:"variant_code"`
	Quantity           int                       `json:"quantity"`
	MockupURL          string                    `json:"mockup_url"`
	CancellationStatus models.CancellationStatus `json:"cancellation_status"`
}

func toSellerView(o models.Order, withItems bool) SellerOrderView {
	action := sellerCancelActionForOrder(&o)
	activeItemCount := 0
	for i := range o.Items {
		if !itemCancelled(o.Items[i].CancellationStatus) {
			activeItemCount++
		}
	}
	v := SellerOrderView{
		ID: o.ID, InternalCode: o.InternalCode, StoreOrderID: o.StoreOrderID,
		StoreOrderDup: o.StoreOrderDup,
		StoreName:     o.StoreName, Status: o.SellerStatus,
		ReviewStatus: o.ReviewStatus, CancellationStatus: o.CancellationStatus, ReviewNote: o.ReviewNote,
		CanCancel:              action == SellerActionCancel,
		CanRequestCancellation: action == SellerActionRequest,
		ItemCount:              activeItemCount, CreatedAt: o.CreatedAt,
	}
	if withItems {
		for _, it := range o.Items {
			v.Items = append(v.Items, SellerItemView{
				ID:      it.ID,
				SKUCode: it.SKUCode, ProductName: it.ProductName, VariantCode: it.VariantCode,
				Quantity: it.Quantity, MockupURL: it.MockupURL, CancellationStatus: it.CancellationStatus,
			})
		}
	}
	return v
}

// SellerOrders returns the seller-scoped, sanitized order list.
func (s *OrderService) SellerOrders(sellerID uint, f repositories.OrderFilter) ([]SellerOrderView, int64, error) {
	f.Page = f.Page.Normalize()
	f.SellerID = &sellerID
	orders, total, err := s.repo.Order.List(f)
	if err != nil {
		return nil, 0, apperr.Internal("could not list seller orders").Wrap(err)
	}
	if err := annotateStoreOrderDupSlice(s.repo, orders); err != nil {
		return nil, 0, apperr.Internal("could not flag duplicate store orders").Wrap(err)
	}
	out := make([]SellerOrderView, 0, len(orders))
	for _, o := range orders {
		out = append(out, toSellerView(o, false))
	}
	return out, total, nil
}

// SellerOrderDetail returns a single sanitized order, enforcing seller ownership.
func (s *OrderService) SellerOrderDetail(sellerID, orderID uint) (*SellerOrderView, error) {
	o, err := s.GetOrder(orderID)
	if err != nil {
		return nil, err
	}
	if o.SellerID != sellerID {
		return nil, apperr.Forbidden("This order does not belong to your seller account")
	}
	v := toSellerView(*o, true)
	return &v, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
