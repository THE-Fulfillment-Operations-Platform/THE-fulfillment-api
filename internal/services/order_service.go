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

// OrderService covers orders, items, the design queue and the seller view.
type OrderService struct {
	repo  *repositories.Repositories
	audit *AuditService
	// tracking pushes a newly recorded tracking number to the 24hTrack provider
	// so the parcel starts being watched and is tagged with its store order id.
	// Nil-safe: the service itself reports Enabled()=false when unconfigured.
	tracking *TrackingSyncService
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

// ActionCounts returns the sidebar badge numbers (orders to review, cancellation
// requests, notes needing attention) in one query — see repositories.ActionCounts.
func (s *OrderService) ActionCounts() (repositories.ActionCounts, error) {
	return s.repo.ActionCounts()
}

// ---------- Design queue ----------

// DesignQueue lists items that still need design work (mockup/print/cut not ready).
func (s *OrderService) DesignQueue(f repositories.ItemFilter) ([]models.OrderItem, int64, error) {
	f.Page = f.Page.Normalize()
	f.NeedsDesign = true
	f.ReviewApproved = true // only approved orders enter the design flow
	// The design screen groups the queue by NVL taken from the SKU's bill of
	// materials, so this is the one caller that genuinely needs that chain.
	f.WithSKUMaterials = true
	return s.repo.OrderItem.List(f)
}

// DesignDownloadableItems returns EVERY design-queue item that already has a design
// file (front or back), matching the caller's filters (batch, NVL, and a q search
// over internal_code/sku_code) — unpaginated, so it backs the "Tải ZIP" dialog's
// pick-list over the whole queue instead of a single page. The caller passes only
// the filter fields (Search, MaterialID, BatchCode); the queue scoping is forced
// here so the list can never leak items outside the design flow.
func (s *OrderService) DesignDownloadableItems(f repositories.ItemFilter) ([]models.OrderItem, error) {
	f.NeedsDesign = true
	f.ReviewApproved = true
	f.HasDesignFile = true
	return s.repo.OrderItem.ListAll(f)
}

// UpdateDesignInput updates an item's design assets and production-ready fields.
// Any field left nil is unchanged. Ops/Design use this from the Pending Review
// detail to normalize a seller item into production-ready data before approval.
type UpdateDesignInput struct {
	PrintFileURL  *string `json:"print_file_url"`
	CutFileURL    *string `json:"cut_file_url"`
	MockupURL     *string `json:"mockup_url"`
	DesignURL     *string `json:"design_url"`
	BackDesignURL *string `json:"back_design_url"`
	SetReady      *bool   `json:"set_ready"`

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
	addAssetSide := func(assetType string, side models.DesignSide, urlStr string) {
		_ = s.repo.DB.Create(&models.ItemAsset{
			OrderItemID: item.ID, AssetType: assetType, Side: side, URL: urlStr, Version: 1,
			UploadedByID: actor.IDPtr(), UploadedAt: now,
		}).Error
	}
	addAsset := func(assetType, urlStr string) { addAssetSide(assetType, models.DesignSideSingle, urlStr) }

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
				addAssetSide("DESIGN", models.DesignSideFront, v)
			}
			designTouched = true
		}
	}
	if in.BackDesignURL != nil {
		if v := strings.TrimSpace(*in.BackDesignURL); v != item.BackDesignURL {
			item.BackDesignURL = v
			if v != "" {
				addAssetSide("DESIGN", models.DesignSideBack, v)
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
		// No print-file check: print/cut files are produced per production batch —
		// many designs are ganged onto one sheet and the resulting file is attached
		// to the whole batch (BatchService.SetBatchLink fans it back down onto every
		// item), so it cannot exist at item level yet. Requiring it here would also
		// deadlock the flow: an item could never reach READY, never be batched, and
		// so never reach the screen where the shared link is entered. Readiness gates
		// on the mockup (QC reference) only.
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

// DesignQueueMaterials returns the materials present in the design queue so the NVL
// filter only offers ones that would actually return rows. MaterialID is cleared: a
// facet must not narrow by the very field it is offering choices for.
func (s *OrderService) DesignQueueMaterials(f repositories.ItemFilter) ([]repositories.MaterialBucket, error) {
	f.NeedsDesign = true
	f.ReviewApproved = true
	f.MaterialID = nil
	return s.repo.OrderItem.DesignQueueMaterials(f)
}

// DesignQueueSKUs returns the SKU codes present in the design queue so the SKU
// filter only offers ones that would actually return rows. SKUCode is cleared: a
// facet must not narrow by the very field it is offering choices for.
func (s *OrderService) DesignQueueSKUs(f repositories.ItemFilter) ([]repositories.SKUBucket, error) {
	f.NeedsDesign = true
	f.ReviewApproved = true
	f.SKUCode = ""
	return s.repo.OrderItem.DesignQueueSKUs(f)
}

// BulkSetReadyResult reports the outcome of BulkSetDesignReady: which items became
// ready and which were skipped, each with a short reason for the toast.
type BulkSetReadyResult struct {
	ReadyIDs []uint         `json:"ready_ids"`
	Skipped  []BulkSkipItem `json:"skipped"`
}

// BulkSkipItem is one item BulkSetDesignReady left untouched, and why.
type BulkSkipItem struct {
	ItemID   uint   `json:"item_id"`
	ItemCode string `json:"item_code"`
	Reason   string `json:"reason"`
}

// BulkSetDesignReady marks many design-queue items ready in one action, so a
// designer can clear a whole material's worth of orders without opening each one.
// An item becomes ready only when its order is approved, it is not cancelled and it
// carries a mockup (the QC reference) — print/cut files are added later on the batch,
// never here. Anything ineligible is returned as skipped with a reason instead of
// failing the whole call, so one bad row never blocks the rest. Items already ready
// are silently ignored (nothing to do), not counted as skipped.
func (s *OrderService) BulkSetDesignReady(actor Actor, itemIDs []uint) (*BulkSetReadyResult, error) {
	res := &BulkSetReadyResult{ReadyIDs: []uint{}, Skipped: []BulkSkipItem{}}
	if len(itemIDs) == 0 {
		return res, nil
	}
	itemsByID, err := s.repo.OrderItem.FindForBatching(itemIDs)
	if err != nil {
		return nil, apperr.Internal("could not load items for bulk set-ready").Wrap(err)
	}

	skip := func(id uint, code, reason string) {
		res.Skipped = append(res.Skipped, BulkSkipItem{ItemID: id, ItemCode: code, Reason: reason})
	}

	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		for _, id := range itemIDs {
			item, ok := itemsByID[id]
			if !ok {
				skip(id, "", "không tồn tại")
				continue
			}
			switch {
			case itemCancelled(item.CancellationStatus):
				skip(id, item.InternalCode, "đã huỷ")
			case item.Order == nil || item.Order.ReviewStatus != models.ReviewApproved:
				skip(id, item.InternalCode, "chưa duyệt")
			case item.DesignStatus == models.DesignReady:
				// already ready — nothing to do, and not an error
			case strings.TrimSpace(item.MockupURL) == "":
				skip(id, item.InternalCode, "thiếu mockup")
			default:
				item.DesignStatus = models.DesignReady
				if err := txRepo.OrderItem.Update(item); err != nil {
					return apperr.Internal("could not set item design-ready").Wrap(err)
				}
				res.ReadyIDs = append(res.ReadyIDs, id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(res.ReadyIDs) > 0 {
		s.audit.Log(actor, "ITEM_DESIGN_READY_BULK", "order_item", nil,
			fmt.Sprintf("Bulk set %d item(s) design-ready", len(res.ReadyIDs)),
			models.JSONMap{"ready_ids": res.ReadyIDs})
	}
	return res, nil
}

// ---------- Create-batch helpers ----------

func (s *OrderService) MaterialBuckets() ([]repositories.MaterialBucket, error) {
	return s.repo.OrderItem.MaterialBuckets()
}

func (s *OrderService) DesignReadyItemsForMaterial(materialID uint, page repositories.Page, sortBy, sortDir string) ([]models.OrderItem, int64, error) {
	return s.repo.OrderItem.DesignReadyItemsForMaterial(materialID, page.Normalize(), sortBy, sortDir)
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
		now := time.Now()
		orderDate := AppDateString(now)
		seq, seqErr := txRepo.Order.NextDailySeq(orderDate, now)
		if seqErr != nil {
			return seqErr
		}
		order = &models.Order{
			StoreOrderID: in.StoreOrderID, StoreOrderRef: in.StoreOrderID, SellerID: in.SellerID,
			StoreName: in.StoreName, Account: in.Account, ShippingMethod: in.ShippingMethod, ShippingName: in.ShippingName,
			ShippingAddress1: in.ShippingAddress1, ShippingAddress2: in.ShippingAddress2,
			ShippingCity: in.ShippingCity, ShippingZip: in.ShippingZip, ShippingProvince: in.ShippingProvince,
			ShippingCountry: in.ShippingCountry, ShippingPhone: in.ShippingPhone, ShippingEmail: in.ShippingEmail,
			IOSS: in.IOSS, Note: in.Note, SellerStatus: models.SellerStatusProduction,
			OrderDate: orderDate, DailySeq: seq, TrackingStatus: models.TrackingNone,
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
	CanCancel              bool `json:"can_cancel"`
	CanRequestCancellation bool `json:"can_request_cancellation"`
	// CurrentStage is where the order stands right now, and CancelWillBill says
	// whether cancelling from here is still charged. Both describe what WOULD
	// happen, so the seller reads the consequence on the button instead of after
	// the invoice. CancelStage/CancelBillable below are the opposite: what a
	// cancellation that already happened was recorded as.
	CurrentStage   models.CancelStage `json:"current_stage"`
	CancelWillBill bool               `json:"cancel_will_bill"`
	CancelStage    models.CancelStage `json:"cancel_stage"`
	CancelBillable bool               `json:"cancel_billable"`
	// The paper trail of a cancellation that already happened. A seller who is
	// still being charged for a cancelled order is owed the reasons on screen —
	// what they asked for, what Ops answered and when — not just a "Đã huỷ" badge
	// and an invoice line they have to query by email.
	CancellationReason         string           `json:"cancellation_reason,omitempty"`
	CancellationResolutionNote string           `json:"cancellation_resolution_note,omitempty"`
	CancellationRequestedAt    *time.Time       `json:"cancellation_requested_at,omitempty"`
	CancellationResolvedAt     *time.Time       `json:"cancellation_resolved_at,omitempty"`
	ItemCount                  int              `json:"item_count"`
	CreatedAt                  time.Time        `json:"created_at"`
	Items                      []SellerItemView `json:"items,omitempty"`

	// Shipment tracking. A seller who uploaded an order is entitled to know where
	// its parcel is — that is the whole point of collecting the journey — so the
	// state of the shipment crosses the sanitisation boundary.
	//
	// What does NOT cross: anything naming the transport partner. No company
	// name, and no provider deep link (the page behind it names them). To the
	// seller, THE carries the parcel; the partner is our supplier, not their
	// business. Internal bookkeeping (sync errors, raw status) stays back too.
	TrackingNumber    string                `json:"tracking_number,omitempty"`
	TrackingStatus    models.TrackingStatus `json:"tracking_status,omitempty"`
	TrackingDetail    string                `json:"tracking_detail,omitempty"`
	TrackingLocation  string                `json:"tracking_location,omitempty"`
	TrackingUpdatedAt *time.Time            `json:"tracking_updated_at,omitempty"`

	// Recipient. This is the seller's OWN data — they typed it into the import
	// file — so unlike the tracking block above there is nothing to sanitise; it
	// crosses back untouched. Withholding it was never a privacy decision, just an
	// omission, and it cost the seller the one thing they check first when a
	// customer writes in: did the parcel go to the right address.
	//
	// Detail only (see toSellerView): the list renders 20 rows and none of them
	// show an address, so shipping a full address block per row is pure payload.
	ShippingName     string `json:"shipping_name,omitempty"`
	ShippingAddress1 string `json:"shipping_address1,omitempty"`
	ShippingAddress2 string `json:"shipping_address2,omitempty"`
	ShippingCity     string `json:"shipping_city,omitempty"`
	ShippingProvince string `json:"shipping_province,omitempty"`
	ShippingZip      string `json:"shipping_zip,omitempty"`
	ShippingCountry  string `json:"shipping_country,omitempty"`
	ShippingPhone    string `json:"shipping_phone,omitempty"`
	ShippingEmail    string `json:"shipping_email,omitempty"`
	IOSS             string `json:"ioss,omitempty"`
	// ShippingMethod is what the seller ASKED for on the import row, not which
	// company we handed the parcel to — that stays redacted.
	ShippingMethod string `json:"shipping_method,omitempty"`
	Note           string `json:"note,omitempty"`
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
	// Per-line cancellation rules, judged on THIS line's own progress: an
	// untouched product inside a partly produced order can still be dropped free.
	CanCancel              bool               `json:"can_cancel"`
	CanRequestCancellation bool               `json:"can_request_cancellation"`
	CancelWillBill         bool               `json:"cancel_will_bill"`
	CancelStage            models.CancelStage `json:"cancel_stage"`
	CancelBillable         bool               `json:"cancel_billable"`
}

// toSellerView sanitizes one order for the seller portal. inProduction is passed
// in rather than derived here: the detail path reads it off preloaded items,
// while the list path resolves a whole page in one query (InProductionIDs)
// instead of preloading every item's batch parts.
func toSellerView(o models.Order, withItems, inProduction bool) SellerOrderView {
	stage := orderCancelStage(o.SellerStatus, inProduction)
	action := sellerCancelAction(o.ReviewStatus, o.CancellationStatus, stage)
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
		CurrentStage:           stage,
		CancelWillBill:         stage.Billable(),
		CancelStage:            o.CancelStage,
		CancelBillable:         o.CancelBillable,
		ItemCount:              activeItemCount, CreatedAt: o.CreatedAt,
		TrackingNumber: o.TrackingNumber,
		TrackingDetail: redactPartner(o.TrackingDetail),
		// Location is only meaningful next to a real shipment state; showing
		// "JAMAICA, NY" on an order the provider has said nothing about would read
		// as progress.
		TrackingUpdatedAt: o.TrackingUpdatedAt,
	}
	// NONE is our "nothing recorded" placeholder, not a shipment state — omit it so
	// the seller UI can tell "no parcel yet" from "parcel waiting for its first scan".
	if o.TrackingStatus != models.TrackingNone && o.TrackingStatus != "" {
		v.TrackingStatus = o.TrackingStatus
		v.TrackingLocation = redactPartner(o.TrackingLocation)
	}
	// Only carry the paper trail once a cancellation actually exists, so an
	// untouched order stays as small as it was.
	if o.CancellationStatus != models.CancellationNone {
		v.CancellationReason = o.CancellationReason
		v.CancellationResolutionNote = o.CancellationResolutionNote
		v.CancellationRequestedAt = o.CancellationRequestedAt
		v.CancellationResolvedAt = o.CancellationResolvedAt
	}
	// withItems marks the detail path. The recipient block rides along with it for
	// the payload reason spelled out on the struct fields.
	if withItems {
		v.ShippingName = o.ShippingName
		v.ShippingAddress1 = o.ShippingAddress1
		v.ShippingAddress2 = o.ShippingAddress2
		v.ShippingCity = o.ShippingCity
		v.ShippingProvince = o.ShippingProvince
		v.ShippingZip = o.ShippingZip
		v.ShippingCountry = o.ShippingCountry
		v.ShippingPhone = o.ShippingPhone
		v.ShippingEmail = o.ShippingEmail
		v.IOSS = o.IOSS
		v.ShippingMethod = o.ShippingMethod
		v.Note = o.Note

		for _, it := range o.Items {
			itemStage := itemCancelStage(&o, &it)
			itemAction := sellerCancelAction(o.ReviewStatus, o.CancellationStatus, itemStage)
			// Mirrors the service guard: a line waiting on ops, or already gone, has
			// no action left. A REFUSED request does — the seller may ask again.
			if it.CancellationStatus == models.CancellationRequested || itemCancelled(it.CancellationStatus) {
				itemAction = SellerActionNone
			}
			v.Items = append(v.Items, SellerItemView{
				ID:      it.ID,
				SKUCode: it.SKUCode, ProductName: it.ProductName, VariantCode: it.VariantCode,
				Quantity: it.Quantity, MockupURL: it.MockupURL, CancellationStatus: it.CancellationStatus,
				CanCancel:              itemAction == SellerActionCancel,
				CanRequestCancellation: itemAction == SellerActionRequest,
				CancelWillBill:         itemStage.Billable(),
				CancelStage:            it.CancelStage,
				CancelBillable:         it.CancelBillable,
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
	// Which of these orders already has work in flight — one query for the page.
	// The list offers the same cancel buttons as the detail screen, so it has to
	// answer the same question, and answering it per order would be N+1.
	ids := make([]uint, 0, len(orders))
	for i := range orders {
		ids = append(ids, orders[i].ID)
	}
	inProduction, err := s.repo.Order.InProductionIDs(ids)
	if err != nil {
		return nil, 0, apperr.Internal("could not resolve production state").Wrap(err)
	}
	out := make([]SellerOrderView, 0, len(orders))
	for _, o := range orders {
		out = append(out, toSellerView(o, false, inProduction[o.ID]))
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
	v := toSellerView(*o, true, orderInProduction(o))
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
