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
	// SellerCode labels the row in a multi-seller preview, where one file's rows
	// land on several sellers' jobs. Response-only: each job already has its seller.
	SellerCode string `json:"seller_code,omitempty" gorm:"-"`
}

func (ImportError) TableName() string { return "import_errors" }

// Order is an internal order. One order groups many items (one CSV row = one
// item; rows sharing a StoreOrderID become one order). The seller-facing status
// is the only status a seller ever sees.
type Order struct {
	Base
	InternalCode string `json:"internal_code" gorm:"uniqueIndex;size:48;not null"`
	StoreOrderID string `json:"store_order_id" gorm:"size:120;index;not null"`
	// SellerID is covered by the composite indexes idx_orders_seller_page and
	// idx_orders_seller_store_order (see ensurePerformanceIndexes) — no separate
	// single-column index needed.
	SellerID  uint   `json:"seller_id" gorm:"not null"`
	Seller    Seller `json:"seller,omitempty" gorm:"foreignKey:SellerID"`
	StoreID   *uint  `json:"store_id" gorm:"index"`
	StoreName string `json:"store_name" gorm:"size:160"`

	// Account is the seller's external account reference from the upload template
	// (the "Account" column). It identifies which storefront account the order
	// came from; kept as import metadata alongside StoreName.
	Account string `json:"account" gorm:"size:120"`

	// There is no ShippingMethod: the 2026-08 seller template dropped the column,
	// and the field was only ever an echo of what the seller asked for. THE decides
	// how a parcel travels, so a half-filled column on the order is worse than none.
	ShippingName     string `json:"shipping_name" gorm:"size:160"`
	ShippingAddress1 string `json:"shipping_address1" gorm:"size:255"`
	ShippingAddress2 string `json:"shipping_address2" gorm:"size:255"`
	ShippingCity     string `json:"shipping_city" gorm:"size:120"`
	ShippingZip      string `json:"shipping_zip" gorm:"size:40"`
	ShippingProvince string `json:"shipping_province" gorm:"size:120"`
	ShippingCountry  string `json:"shipping_country" gorm:"size:80"`
	ShippingPhone    string `json:"shipping_phone" gorm:"size:60"`
	// No ShippingEmail and no IOSS either — both left the seller template in the
	// same pass. The legacy columns are NOT dropped from the database: orders
	// imported before the change keep their values, they are simply no longer read
	// or written. Dropping them would destroy history to save nothing.
	Note string `json:"note" gorm:"size:1000"`

	// StoreOrderRef mirrors StoreOrderID. It once backed a unique (seller,
	// store_order) index, but a StoreOrderID is a repeatable reference label — the
	// same store order legitimately spans many items and re-imports — so uniqueness
	// now lives solely on the system-generated InternalCode. Kept as a plain indexed
	// column for lookups; the legacy unique index is dropped in AutoMigrate.
	StoreOrderRef string `json:"-" gorm:"size:120;index"`

	SellerStatus SellerStatus `json:"seller_status" gorm:"size:20;not null;index;default:'PRODUCTION'"`

	// HandedOverAt is the moment the order first left the factory for THE — the
	// HANDED_OFF transition ("xuất xưởng"). Stamped once and never moved: a later
	// carrier scan, a second handoff or a replayed status must not rewrite when
	// the parcel actually went out the door. The journey screen's date filter
	// reads this column (partial index in ensurePerformanceIndexes); legacy
	// orders are backfilled from status_histories in migrate.go.
	HandedOverAt *time.Time `json:"handed_over_at"`

	ImportJobID *uint `json:"import_job_id" gorm:"index"`
	CreatedByID *uint `json:"created_by_id"`

	// OrderDate + DailySeq implement "STT trong ngày" (per-day order number). Both
	// are assigned atomically at creation via the daily_counters allocator, in the
	// business timezone (DB_TIMEZONE), so the number is stable across pagination and
	// sorting and never derived from a front-end loop index. OrderDate is the
	// business calendar day (YYYY-MM-DD); DailySeq starts at 1 each day. They are the
	// single source of truth reused by Batch, Design Queue and design file names.
	OrderDate string `json:"order_date" gorm:"size:10;index"`
	DailySeq  int    `json:"daily_seq" gorm:"not null;default:0;index"`

	// Tracking (YC8). Populated manually via PATCH /orders/:id/tracking or synced
	// from a handoff when it ships. TrackingStatus uses the TrackingStatus enum so
	// the UI can render a consistent badge.
	//
	// There is deliberately NO "shipping company" column. Towards the seller, THE
	// is the shipping company; which transport partner physically carries the
	// parcel is supplier information. A column would be filled in, then rendered,
	// then exported — so the field does not exist at all.
	//
	// TrackingURL is the internal deep link into the tracking provider. It is
	// never included in a seller-facing response for the same reason: the page it
	// opens names the transport partner.
	TrackingNumber    string         `json:"tracking_number" gorm:"size:120;index"`
	TrackingStatus    TrackingStatus `json:"tracking_status" gorm:"size:20;not null;default:'NONE'"`
	TrackingURL       string         `json:"tracking_url" gorm:"size:500"`
	TrackingUpdatedAt *time.Time     `json:"tracking_updated_at"`

	// Provider-sync fields, filled by the tracking integration. Detail/Location
	// mirror the latest scan so a list screen shows "where is it right now"
	// without loading the whole timeline (see OrderTrackingEvent).
	// RawStatus keeps the provider's own wording, which is often more specific
	// than the normalized TrackingStatus badge.
	TrackingDetail      string     `json:"tracking_detail" gorm:"size:500"`
	TrackingLocation    string     `json:"tracking_location" gorm:"size:255"`
	TrackingRawStatus   string     `json:"tracking_raw_status" gorm:"size:120"`
	TrackingDeliveredAt *time.Time `json:"tracking_delivered_at"`
	// TrackingSyncedAt is when the provider was last polled — set even when
	// nothing changed, because it is what the scheduler orders by to pick the
	// least-recently-checked parcels next.
	TrackingSyncedAt  *time.Time `json:"tracking_synced_at" gorm:"index"`
	TrackingSyncError string     `json:"tracking_sync_error" gorm:"size:255"`

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

	// CancelStage is how far the order had progressed when the cancellation was
	// ASKED FOR (not when it was resolved): a request that waits in the ops queue
	// while the factory moves on is still judged by the state the seller acted on.
	// CancelBillable starts as CancelStage.Billable() but is a separate column
	// because Ops/Admin may waive the charge when they resolve the request.
	CancelStage    CancelStage `json:"cancel_stage" gorm:"size:20;not null;default:''"`
	CancelBillable bool        `json:"cancel_billable" gorm:"not null;default:false"`

	Items []OrderItem `json:"items,omitempty" gorm:"foreignKey:OrderID"`

	// StoreOrderDup is a computed, non-persisted flag: true when this StoreOrderID
	// is shared by more than one order for the same seller (a repeated store order
	// id, not merely multiple items of one order). List endpoints set it so every
	// screen can highlight possible duplicates the same way the importer does.
	StoreOrderDup bool `json:"store_order_dup" gorm:"-"`
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

	SKUID   *uint  `json:"sku_id" gorm:"column:sku_id;index"`
	SKU     *SKU   `json:"sku,omitempty" gorm:"foreignKey:SKUID"`
	SKUCode string `json:"sku_code" gorm:"size:48;index"`
	// ProductName is DERIVED, never stored (gorm:"-"): the product's name belongs
	// to the SKU in master data, and the seller template no longer carries a name
	// column at all. Services fill it in from the SKU so the API shape callers
	// already depend on does not change. The legacy order_items.product_name column
	// is left in the database untouched, simply unread.
	ProductName string `json:"product_name" gorm:"-"`
	Quantity    int    `json:"quantity" gorm:"not null;default:1"`

	// DesignURL is the primary/front design (also the "SINGLE" side for one-sided
	// products). BackDesignURL holds the second side for two-sided products; it is
	// optional and only set when a product actually has a back design. Keeping
	// DesignURL as the front/single side preserves backward compatibility with every
	// existing single-design item and flow.
	DesignURL     string `json:"design_url" gorm:"size:500"`
	BackDesignURL string `json:"back_design_url" gorm:"size:500"`
	PrintFileURL  string `json:"print_file_url" gorm:"size:500"`
	CutFileURL    string `json:"cut_file_url" gorm:"size:500"`
	MockupURL     string `json:"mockup_url" gorm:"size:500"` // seller-provided QC reference
	EngraveText   string `json:"engrave_text" gorm:"size:500"`

	// Production-ready fields Ops/Design normalize before an item is produced.
	// They map 1:1 onto the legacy production template columns exported per batch.
	ImageCode          string `json:"image_code" gorm:"size:120"`           // Mã ảnh (seller image code)
	QCDescription      string `json:"qc_description" gorm:"size:500"`       // Mô tả SP để QC
	ProductionSequence int    `json:"production_sequence" gorm:"default:0"` // Số thứ tự
	ProductionFileName string `json:"production_file_name" gorm:"size:255"` // Tên File

	InternalStatus InternalStatus `json:"internal_status" gorm:"size:20;not null;index;default:'PENDING'"`
	DesignStatus   DesignStatus   `json:"design_status" gorm:"size:20;not null;index;default:'PENDING'"`
	// ReworkCount is how many times QC failed this product and sent it back to be
	// re-made. Denormalised from the scrapped batch parts purely so the create-batch
	// bucket and the item list can label and filter rework without a join.
	ReworkCount int `json:"rework_count" gorm:"not null;default:0"`

	// Cancellation is tracked per line item so cancelling one product does not
	// incorrectly cancel every product grouped under the same order.
	CancellationStatus         CancellationStatus `json:"cancellation_status" gorm:"size:20;not null;index;default:'NONE'"`
	CancellationRequestedByID  *uint              `json:"cancellation_requested_by_id"`
	CancellationRequestedAt    *time.Time         `json:"cancellation_requested_at"`
	CancellationReason         string             `json:"cancellation_reason" gorm:"size:1000"`
	CancellationResolvedByID   *uint              `json:"cancellation_resolved_by_id"`
	CancellationResolvedAt     *time.Time         `json:"cancellation_resolved_at"`
	CancellationResolutionNote string             `json:"cancellation_resolution_note" gorm:"size:1000"`

	// Same stage/billing snapshot as on Order — a single product cancelled after
	// its part was printed is charged just like a whole order would be.
	CancelStage    CancelStage `json:"cancel_stage" gorm:"size:20;not null;default:''"`
	CancelBillable bool        `json:"cancel_billable" gorm:"not null;default:false"`

	Assets     []ItemAsset `json:"assets,omitempty" gorm:"foreignKey:OrderItemID"`
	BatchItems []BatchItem `json:"batch_items,omitempty" gorm:"foreignKey:OrderItemID"`
}

func (OrderItem) TableName() string { return "order_items" }

// ItemAsset is a versioned design/print/cut/mockup file attached to an item. The
// "current" URLs are mirrored onto OrderItem for convenience; this table keeps
// the full history.
type ItemAsset struct {
	Base
	OrderItemID uint   `json:"order_item_id" gorm:"index;not null"`
	AssetType   string `json:"asset_type" gorm:"size:20;not null"` // DESIGN | PRINT_FILE | CUT_FILE | MOCKUP | RAW
	// Side is which physical side this asset belongs to (SINGLE | FRONT | BACK).
	// Defaults to SINGLE so existing one-sided assets stay correct. Lets the design
	// history distinguish a front vs back file without adding new asset types.
	Side         DesignSide `json:"side" gorm:"size:10;not null;default:'SINGLE'"`
	URL          string     `json:"url" gorm:"size:500;not null"`
	Version      int        `json:"version" gorm:"not null;default:1"`
	UploadedByID *uint      `json:"uploaded_by_id"`
	Note         string     `json:"note" gorm:"size:255"`
	UploadedAt   time.Time  `json:"uploaded_at"`
}

func (ItemAsset) TableName() string { return "item_assets" }
