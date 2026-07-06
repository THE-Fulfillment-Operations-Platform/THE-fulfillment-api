package models

import "time"

// ImportJob records one order-import run (CSV/JSON). Valid parsed rows are kept
// in RawRows so a PREVIEW can be COMMITTED later without re-uploading the file.
type ImportJob struct {
	Base
	SellerID     *uint           `json:"seller_id" gorm:"index"`
	StoreID      *uint           `json:"store_id" gorm:"index"`
	Filename     string          `json:"filename" gorm:"size:255"`
	Source       string          `json:"source" gorm:"size:20;not null;default:'CSV'"` // CSV | JSON
	Status       ImportJobStatus `json:"status" gorm:"size:20;not null;index"`
	TotalRows    int             `json:"total_rows"`
	ValidRows    int             `json:"valid_rows"`
	ErrorRows    int             `json:"error_rows"`
	CreatedCount int             `json:"created_count"` // orders created on commit
	RawRows      JSONB           `json:"-" gorm:"type:jsonb"`
	CreatedByID  *uint           `json:"created_by_id"`
	Errors       []ImportError   `json:"errors,omitempty" gorm:"foreignKey:ImportJobID"`
}

func (ImportJob) TableName() string { return "import_jobs" }

// ImportError is a single per-row validation failure surfaced to the user with a
// suggested fix (matching the wireframe error table).
type ImportError struct {
	Base
	ImportJobID  uint   `json:"import_job_id" gorm:"index;not null"`
	RowNumber    int    `json:"row_number"`
	StoreOrderID string `json:"store_order_id" gorm:"size:120"`
	SKU          string `json:"sku" gorm:"size:48"`
	Field        string `json:"field" gorm:"size:60"`
	ErrorCode    string `json:"error_code" gorm:"size:60"`
	Message      string `json:"message" gorm:"size:255"`
	Suggestion   string `json:"suggestion" gorm:"size:255"`
}

func (ImportError) TableName() string { return "import_errors" }

// Order is an internal order. One order groups many items (one CSV row = one
// item; rows sharing a StoreOrderID become one order). The seller-facing status
// is the only status a seller ever sees.
type Order struct {
	Base
	InternalCode string `json:"internal_code" gorm:"uniqueIndex;size:48;not null"`
	StoreOrderID string `json:"store_order_id" gorm:"size:120;index;not null"`
	SellerID     uint   `json:"seller_id" gorm:"index;not null;uniqueIndex:idx_seller_store_order"`
	Seller       Seller `json:"seller,omitempty" gorm:"foreignKey:SellerID"`
	StoreID      *uint  `json:"store_id" gorm:"index"`
	StoreName    string `json:"store_name" gorm:"size:160"`

	// Account is the seller's external account reference from the upload template
	// (the "Account" column). It identifies which storefront account the order
	// came from; kept as import metadata alongside StoreName.
	Account string `json:"account" gorm:"size:120"`

	ShippingMethod   string `json:"shipping_method" gorm:"size:80"`
	ShippingName     string `json:"shipping_name" gorm:"size:160"`
	ShippingAddress1 string `json:"shipping_address1" gorm:"size:255"`
	ShippingAddress2 string `json:"shipping_address2" gorm:"size:255"`
	ShippingCity     string `json:"shipping_city" gorm:"size:120"`
	ShippingZip      string `json:"shipping_zip" gorm:"size:40"`
	ShippingProvince string `json:"shipping_province" gorm:"size:120"`
	ShippingCountry  string `json:"shipping_country" gorm:"size:80"`
	ShippingPhone    string `json:"shipping_phone" gorm:"size:60"`
	ShippingEmail    string `json:"shipping_email" gorm:"size:160"`
	IOSS             string `json:"ioss" gorm:"size:60"`
	Note             string `json:"note" gorm:"size:1000"`

	// StoreOrderRef is duplicated into a unique composite index with SellerID so a
	// seller cannot import the same store order twice.
	StoreOrderRef string `json:"-" gorm:"size:120;uniqueIndex:idx_seller_store_order"`

	SellerStatus SellerStatus `json:"seller_status" gorm:"size:20;not null;index;default:'PRODUCTION'"`
	ImportJobID  *uint        `json:"import_job_id" gorm:"index"`
	CreatedByID  *uint        `json:"created_by_id"`

	// Review (intake) state. New orders are PENDING_REVIEW and only enter the
	// design/production flow once APPROVED. The DB default is APPROVED so orders
	// created before this feature (and any legacy rows) remain visible to the
	// existing pipeline; new orders explicitly set PENDING_REVIEW on creation.
	ReviewStatus ReviewStatus `json:"review_status" gorm:"size:20;not null;index;default:'APPROVED'"`
	ReviewedByID *uint        `json:"reviewed_by_id"`
	ReviewedAt   *time.Time   `json:"reviewed_at"`
	ReviewNote   string       `json:"review_note" gorm:"size:1000"`

	// Cancellation state and audit metadata.
	CancellationStatus         CancellationStatus `json:"cancellation_status" gorm:"size:20;not null;index;default:'NONE'"`
	CancellationRequestedByID  *uint              `json:"cancellation_requested_by_id"`
	CancellationRequestedAt    *time.Time         `json:"cancellation_requested_at"`
	CancellationReason         string             `json:"cancellation_reason" gorm:"size:1000"`
	CancellationResolvedByID   *uint              `json:"cancellation_resolved_by_id"`
	CancellationResolvedAt     *time.Time         `json:"cancellation_resolved_at"`
	CancellationResolutionNote string             `json:"cancellation_resolution_note" gorm:"size:1000"`

	Items []OrderItem `json:"items,omitempty" gorm:"foreignKey:OrderID"`
}

func (Order) TableName() string { return "orders" }

// OrderItem is a single fulfillable unit. Production, QC and packing all operate
// at this level. InternalStatus is derived from the item's batch parts.
type OrderItem struct {
	Base
	OrderID      uint   `json:"order_id" gorm:"index;not null"`
	Order        *Order `json:"order,omitempty" gorm:"foreignKey:OrderID"`
	InternalCode string `json:"internal_code" gorm:"uniqueIndex;size:64;not null"` // QR/tem for scanning
	LineNo       int    `json:"line_no"`

	SKUID       *uint  `json:"sku_id" gorm:"column:sku_id;index"`
	SKU         *SKU   `json:"sku,omitempty" gorm:"foreignKey:SKUID"`
	SKUCode     string `json:"sku_code" gorm:"size:48;index"`
	ProductName string `json:"product_name" gorm:"size:200"`
	VariantCode string `json:"variant_code" gorm:"size:80"`
	Quantity    int    `json:"quantity" gorm:"not null;default:1"`

	DesignURL    string `json:"design_url" gorm:"size:500"`
	PrintFileURL string `json:"print_file_url" gorm:"size:500"`
	CutFileURL   string `json:"cut_file_url" gorm:"size:500"`
	MockupURL    string `json:"mockup_url" gorm:"size:500"` // seller-provided QC reference
	EngraveText  string `json:"engrave_text" gorm:"size:500"`

	// Production-ready fields Ops/Design normalize before an item is produced.
	// They map 1:1 onto the legacy production template columns exported per batch.
	ImageCode          string `json:"image_code" gorm:"size:120"`           // Mã ảnh (seller image code)
	QCDescription      string `json:"qc_description" gorm:"size:500"`       // Mô tả SP để QC
	ProductionSequence int    `json:"production_sequence" gorm:"default:0"` // Số thứ tự
	ProductionFileName string `json:"production_file_name" gorm:"size:255"` // Tên File

	InternalStatus InternalStatus `json:"internal_status" gorm:"size:20;not null;index;default:'PENDING'"`
	DesignStatus   DesignStatus   `json:"design_status" gorm:"size:20;not null;index;default:'PENDING'"`

	Assets     []ItemAsset `json:"assets,omitempty" gorm:"foreignKey:OrderItemID"`
	BatchItems []BatchItem `json:"batch_items,omitempty" gorm:"foreignKey:OrderItemID"`
}

func (OrderItem) TableName() string { return "order_items" }

// ItemAsset is a versioned design/print/cut/mockup file attached to an item. The
// "current" URLs are mirrored onto OrderItem for convenience; this table keeps
// the full history.
type ItemAsset struct {
	Base
	OrderItemID  uint      `json:"order_item_id" gorm:"index;not null"`
	AssetType    string    `json:"asset_type" gorm:"size:20;not null"` // DESIGN | PRINT_FILE | CUT_FILE | MOCKUP | RAW
	URL          string    `json:"url" gorm:"size:500;not null"`
	Version      int       `json:"version" gorm:"not null;default:1"`
	UploadedByID *uint     `json:"uploaded_by_id"`
	Note         string    `json:"note" gorm:"size:255"`
	UploadedAt   time.Time `json:"uploaded_at"`
}

func (ItemAsset) TableName() string { return "item_assets" }
