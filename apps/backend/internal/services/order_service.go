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
	return s.repo.Order.List(f)
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

// ---------- Items ----------

func (s *OrderService) ListItems(f repositories.ItemFilter) ([]models.OrderItem, int64, error) {
	f.Page = f.Page.Normalize()
	return s.repo.OrderItem.List(f)
}

func (s *OrderService) GetItem(id uint) (*models.OrderItem, error) {
	it, err := s.repo.OrderItem.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Item not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return it, nil
}

// ---------- Design queue ----------

// DesignQueue lists items that still need design work (mockup/print/cut not ready).
func (s *OrderService) DesignQueue(f repositories.ItemFilter) ([]models.OrderItem, int64, error) {
	f.Page = f.Page.Normalize()
	f.NeedsDesign = true
	return s.repo.OrderItem.List(f)
}

// UpdateDesignInput updates an item's design assets. Any field left nil is unchanged.
type UpdateDesignInput struct {
	PrintFileURL *string `json:"print_file_url"`
	CutFileURL   *string `json:"cut_file_url"`
	MockupURL    *string `json:"mockup_url"`
	DesignURL    *string `json:"design_url"`
	SetReady     *bool   `json:"set_ready"`
}

// UpdateItemDesign saves print/cut/mockup/design URLs, records versioned assets
// and optionally marks the item design-ready. Designers (and ops/admin) use this.
func (s *OrderService) UpdateItemDesign(actor Actor, itemID uint, in UpdateDesignInput) (*models.OrderItem, error) {
	item, err := s.GetItem(itemID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	addAsset := func(assetType, urlStr string) {
		_ = s.repo.DB.Create(&models.ItemAsset{
			OrderItemID: item.ID, AssetType: assetType, URL: urlStr, Version: 1,
			UploadedByID: actor.IDPtr(), UploadedAt: now,
		}).Error
	}

	if in.PrintFileURL != nil {
		item.PrintFileURL = strings.TrimSpace(*in.PrintFileURL)
		if item.PrintFileURL != "" {
			addAsset("PRINT_FILE", item.PrintFileURL)
		}
	}
	if in.CutFileURL != nil {
		item.CutFileURL = strings.TrimSpace(*in.CutFileURL)
		if item.CutFileURL != "" {
			addAsset("CUT_FILE", item.CutFileURL)
		}
	}
	if in.DesignURL != nil {
		item.DesignURL = strings.TrimSpace(*in.DesignURL)
		if item.DesignURL != "" {
			addAsset("DESIGN", item.DesignURL)
		}
	}
	if in.MockupURL != nil {
		item.MockupURL = strings.TrimSpace(*in.MockupURL)
		if item.MockupURL != "" {
			addAsset("MOCKUP", item.MockupURL)
		}
	}

	// Re-evaluate design status.
	if item.DesignStatus != models.DesignReady {
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
	MockupURL   string `json:"mockup_url"`
	EngraveText string `json:"engrave_text"`
}

type DirectOrderInput struct {
	SellerID         uint              `json:"seller_id" binding:"required"`
	StoreOrderID     string            `json:"store_order_id" binding:"required"`
	StoreName        string            `json:"store_name"`
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
	if _, err := s.repo.Order.FindBySellerAndStoreOrder(in.SellerID, in.StoreOrderID); err == nil {
		return nil, apperr.Conflict("An order with this StoreOrderID already exists for the seller")
	}

	var order *models.Order
	err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		order = &models.Order{
			StoreOrderID: in.StoreOrderID, StoreOrderRef: in.StoreOrderID, SellerID: in.SellerID,
			StoreName: in.StoreName, ShippingMethod: in.ShippingMethod, ShippingName: in.ShippingName,
			ShippingAddress1: in.ShippingAddress1, ShippingAddress2: in.ShippingAddress2,
			ShippingCity: in.ShippingCity, ShippingZip: in.ShippingZip, ShippingProvince: in.ShippingProvince,
			ShippingCountry: in.ShippingCountry, ShippingPhone: in.ShippingPhone, ShippingEmail: in.ShippingEmail,
			IOSS: in.IOSS, Note: in.Note, SellerStatus: models.SellerStatusProduction, CreatedByID: actor.IDPtr(),
		}
		if err := txRepo.Order.Create(order); err != nil {
			return err
		}
		order.InternalCode = "ORD-" + pad6(order.ID)
		if err := txRepo.Order.Update(order); err != nil {
			return err
		}
		for i, it := range in.Items {
			skuCode := strings.ToUpper(strings.TrimSpace(it.SKUCode))
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
				OrderID: order.ID, LineNo: i + 1, InternalCode: "ORD-" + pad6(order.ID) + "_" + itoa(i+1),
				SKUID: skuID, SKUCode: skuCode, ProductName: it.ProductName, VariantCode: it.VariantCode,
				Quantity: maxInt(it.Quantity, 1), MockupURL: it.MockupURL, EngraveText: it.EngraveText,
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
	ID           uint                `json:"id"`
	InternalCode string              `json:"internal_code"`
	StoreOrderID string              `json:"store_order_id"`
	StoreName    string              `json:"store_name"`
	Status       models.SellerStatus `json:"status"`
	ItemCount    int                 `json:"item_count"`
	CreatedAt    time.Time           `json:"created_at"`
	Items        []SellerItemView    `json:"items,omitempty"`
}

// SellerItemView only exposes product-level facts, not the factory pipeline.
type SellerItemView struct {
	SKUCode     string `json:"sku_code"`
	ProductName string `json:"product_name"`
	VariantCode string `json:"variant_code"`
	Quantity    int    `json:"quantity"`
	MockupURL   string `json:"mockup_url"`
}

func toSellerView(o models.Order, withItems bool) SellerOrderView {
	v := SellerOrderView{
		ID: o.ID, InternalCode: o.InternalCode, StoreOrderID: o.StoreOrderID,
		StoreName: o.StoreName, Status: o.SellerStatus, ItemCount: len(o.Items), CreatedAt: o.CreatedAt,
	}
	if withItems {
		for _, it := range o.Items {
			v.Items = append(v.Items, SellerItemView{
				SKUCode: it.SKUCode, ProductName: it.ProductName, VariantCode: it.VariantCode,
				Quantity: it.Quantity, MockupURL: it.MockupURL,
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

func pad6(n uint) string {
	s := itoa(int(n))
	for len(s) < 6 {
		s = "0" + s
	}
	return s
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
