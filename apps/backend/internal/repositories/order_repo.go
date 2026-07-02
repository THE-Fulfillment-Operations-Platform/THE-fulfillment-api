package repositories

import (
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
	SellerID     *uint
	StoreID      *uint
	SKUCode      string
	SellerStatus string
	StoreOrderID string
	DateFrom     *time.Time
	DateTo       *time.Time
}

type OrderRepository struct{ db *gorm.DB }

func (r *OrderRepository) Create(o *models.Order) error { return r.db.Create(o).Error }
func (r *OrderRepository) Update(o *models.Order) error { return r.db.Save(o).Error }

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

func (r *OrderRepository) FindBySellerAndStoreOrder(sellerID uint, storeOrderID string) (*models.Order, error) {
	var o models.Order
	err := r.db.Where("seller_id = ? AND store_order_id = ?", sellerID, storeOrderID).First(&o).Error
	if err != nil {
		return nil, err
	}
	return &o, nil
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
	if f.StoreOrderID != "" {
		q = q.Where("orders.store_order_id ILIKE ?", "%"+f.StoreOrderID+"%")
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

// ---------- Order items ----------

// ItemFilter captures item-view filters (store, sku, status, batch, date).
type ItemFilter struct {
	Page
	SellerID       *uint
	StoreID        *uint
	SKUCode        string
	InternalStatus string
	DesignStatus   string
	BatchID        *uint
	NeedsDesign    bool // design queue: design not ready
	DateFrom       *time.Time
	DateTo         *time.Time
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

func (r *OrderItemRepository) FindByCode(code string) (*models.OrderItem, error) {
	var it models.OrderItem
	err := r.db.
		Preload("Order.Seller").
		Preload("SKU.Materials.Material").
		Preload("BatchItems.Batch").
		Preload("BatchItems.Material").
		Where("internal_code = ?", code).First(&it).Error
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func (r *OrderItemRepository) baseQuery(f ItemFilter) *gorm.DB {
	q := r.db.Model(&models.OrderItem{}).
		Joins("JOIN orders ON orders.id = order_items.order_id AND orders.deleted_at IS NULL")
	if f.SellerID != nil {
		q = q.Where("orders.seller_id = ?", *f.SellerID)
	}
	if f.StoreID != nil {
		q = q.Where("orders.store_id = ?", *f.StoreID)
	}
	if f.SKUCode != "" {
		q = q.Where("order_items.sku_code = ?", f.SKUCode)
	}
	if f.InternalStatus != "" {
		q = q.Where("order_items.internal_status = ?", f.InternalStatus)
	}
	if f.DesignStatus != "" {
		q = q.Where("order_items.design_status = ?", f.DesignStatus)
	}
	if f.NeedsDesign {
		q = q.Where("order_items.design_status IN ?", []string{
			string(models.DesignPending), string(models.DesignInProgress), string(models.DesignMissing),
		})
	}
	if f.BatchID != nil {
		q = q.Where("order_items.id IN (?)",
			r.db.Model(&models.BatchItem{}).Select("order_item_id").Where("batch_id = ?", *f.BatchID))
	}
	if f.DateFrom != nil {
		q = q.Where("order_items.created_at >= ?", *f.DateFrom)
	}
	if f.DateTo != nil {
		q = q.Where("order_items.created_at <= ?", *f.DateTo)
	}
	return q
}

func (r *OrderItemRepository) List(f ItemFilter) ([]models.OrderItem, int64, error) {
	var rows []models.OrderItem
	var total int64
	r.baseQuery(f).Count(&total)
	err := r.baseQuery(f).
		Preload("Order.Seller").
		Preload("SKU").
		Preload("BatchItems.Batch").
		Preload("BatchItems.Material").
		Order("order_items.id desc").
		Limit(f.PageSize).Offset(f.Offset()).Find(&rows).Error
	return rows, total, err
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
		Select("oi.id AS order_item_id, sm.material_id AS material_id, m.code AS material_code, m.name AS material_name").
		Joins("JOIN sku_materials sm ON sm.sku_id = oi.sku_id").
		Joins("JOIN materials m ON m.id = sm.material_id").
		Where("oi.deleted_at IS NULL").
		Where("oi.design_status = ?", models.DesignReady).
		Where("NOT EXISTS (?)",
			r.db.Table("batch_items bi").
				Select("1").
				Where("bi.order_item_id = oi.id AND bi.material_id = sm.material_id AND bi.deleted_at IS NULL"))
}

// MaterialBuckets returns the count of design-ready, unbatched item parts per material.
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
// the given material (used to populate the create-batch item table).
func (r *OrderItemRepository) DesignReadyItemsForMaterial(materialID uint, p Page) ([]models.OrderItem, int64, error) {
	sub := r.designReadyUnbatchedSubquery().Where("sm.material_id = ?", materialID)
	var ids []uint
	if err := r.db.Table("(?) AS sub", sub).Select("sub.order_item_id").Scan(&ids).Error; err != nil {
		return nil, 0, err
	}
	total := int64(len(ids))
	if total == 0 {
		return []models.OrderItem{}, 0, nil
	}
	// manual pagination over the id slice
	start := p.Offset()
	if start > len(ids) {
		start = len(ids)
	}
	end := start + p.PageSize
	if end > len(ids) {
		end = len(ids)
	}
	pageIDs := ids[start:end]
	var rows []models.OrderItem
	err := r.db.Preload("Order.Seller").Preload("SKU.Materials.Material").
		Where("id IN ?", pageIDs).Order("id asc").Find(&rows).Error
	return rows, total, err
}
