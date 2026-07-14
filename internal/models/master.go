package models

// Role catalog table. Kept as reference/extensibility data; the active role used
// for permission checks lives on the user as a string for index-friendly lookups.
type RoleRecord struct {
	Base
	Name        string `json:"name" gorm:"uniqueIndex;size:32;not null"`
	Description string `json:"description" gorm:"size:255"`
}

func (RoleRecord) TableName() string { return "roles" }

// User is an authenticated operator or seller account.
type User struct {
	Base
	Email        string  `json:"email" gorm:"uniqueIndex;size:160;not null"`
	PasswordHash string  `json:"-" gorm:"size:255;not null"`
	FullName     string  `json:"full_name" gorm:"size:120;not null"`
	Role         Role    `json:"role" gorm:"size:32;not null;index"`
	SellerID     *uint   `json:"seller_id" gorm:"index"` // set only for SELLER role
	Seller       *Seller `json:"seller,omitempty" gorm:"foreignKey:SellerID"`
	IsActive     bool    `json:"is_active" gorm:"not null;default:true"`
}

func (User) TableName() string { return "users" }

// Seller is a merchant whose orders the factory fulfills.
type Seller struct {
	Base
	Code         string  `json:"code" gorm:"uniqueIndex;size:32;not null"`
	Name         string  `json:"name" gorm:"size:160;not null"`
	ContactEmail string  `json:"contact_email" gorm:"size:160"`
	ContactPhone string  `json:"contact_phone" gorm:"size:40"`
	Status       string  `json:"status" gorm:"size:20;not null;default:'active'"` // active | paused
	Note         string  `json:"note" gorm:"size:500"`
	Stores       []Store `json:"stores,omitempty" gorm:"foreignKey:SellerID"`
}

func (Seller) TableName() string { return "sellers" }

// Store is a seller's storefront (Etsy, Shopify, ...). One seller may own many.
type Store struct {
	Base
	SellerID    uint   `json:"seller_id" gorm:"index;not null"`
	Seller      Seller `json:"-" gorm:"foreignKey:SellerID"`
	Name        string `json:"name" gorm:"size:160;not null"`
	Platform    string `json:"platform" gorm:"size:60"` // Etsy, Shopify, ...
	ExternalRef string `json:"external_ref" gorm:"size:120"`
}

func (Store) TableName() string { return "stores" }

// Material is a raw material / nguyên vật liệu. It is the primary axis the
// factory batches production around (Gỗ, Mica, Acrylic, Metal, ...).
type Material struct {
	Base
	Code        string `json:"code" gorm:"uniqueIndex;size:32;not null"`
	Name        string `json:"name" gorm:"size:120;not null"`
	Description string `json:"description" gorm:"size:255"`
	// ProductsPerUnit is the production quota of one unit of this material: the max
	// number of products a single unit (one sheet / one lot) can yield. When a batch
	// is created whose total products exceed this, it is split into a parent batch
	// plus child batches, each capped at this many products (see Batch.ParentBatchID).
	// nil = unlimited (no split). Only OWNER may set it (guarded in CatalogService).
	ProductsPerUnit *int `json:"products_per_unit"`
}

func (Material) TableName() string { return "materials" }

// SKU is a product setup that is fixed against one or more materials. A combo
// SKU references multiple materials and therefore produces multiple batches.
type SKU struct {
	Base
	Code        string        `json:"code" gorm:"uniqueIndex;size:48;not null"`
	Name        string        `json:"name" gorm:"size:160;not null"`
	ProductName string        `json:"product_name" gorm:"size:200"`
	Description string        `json:"description" gorm:"size:500"`
	IsCombo     bool          `json:"is_combo" gorm:"not null;default:false"`
	IsActive    bool          `json:"is_active" gorm:"not null;default:true"`
	Materials   []SKUMaterial `json:"materials,omitempty" gorm:"foreignKey:SKUID"`
}

func (SKU) TableName() string { return "skus" }

// SKUMaterial links a SKU to the materials it is built from. The unique index on
// (sku_id, material_id) keeps the material set clean per SKU.
type SKUMaterial struct {
	Base
	SKUID           uint     `json:"sku_id" gorm:"column:sku_id;index:idx_sku_material,unique;not null"`
	MaterialID      uint     `json:"material_id" gorm:"index:idx_sku_material,unique;not null"`
	Material        Material `json:"material,omitempty" gorm:"foreignKey:MaterialID"`
	QuantityPerUnit int      `json:"quantity_per_unit" gorm:"not null;default:1"`
	Note            string   `json:"note" gorm:"size:255"`
}

func (SKUMaterial) TableName() string { return "sku_materials" }

// MasterImportJob records one legacy-Excel master-data import run. Unlike an
// order ImportJob it does not create orders — it seeds Materials, SKUs and the
// SKU↔Material mapping from the factory's existing operational spreadsheet
// (columns `SKU` + `Loại VL`). The analysed plan is kept in Plan so a PREVIEW
// can be COMMITTED later without re-uploading the file.
type MasterImportJob struct {
	Base
	Filename string          `json:"filename" gorm:"size:255"`
	Source   string          `json:"source" gorm:"size:20;not null;default:'XLSX'"` // XLSX | CSV | JSON
	Status   ImportJobStatus `json:"status" gorm:"size:20;not null;index"`

	// Analysis counts (filled on PREVIEW).
	TotalRows    int `json:"total_rows"`
	NewMaterials int `json:"new_materials"`
	NewSKUs      int `json:"new_skus"`
	NewMappings  int `json:"new_mappings"`
	ReviewCount  int `json:"review_count"`  // SKUs whose rows disagree on the material set
	MissingCount int `json:"missing_count"` // SKUs present but Loại VL empty
	ErrorRows    int `json:"error_rows"`    // rows with Loại VL but no SKU

	// Applied counts (filled on COMMIT).
	MaterialsCreated int `json:"materials_created"`
	SKUsCreated      int `json:"skus_created"`
	MappingsCreated  int `json:"mappings_created"`

	// Plan is the full analysis (materials + skus + errors) used at commit time.
	Plan        JSONB `json:"-" gorm:"type:jsonb"`
	CreatedByID *uint `json:"created_by_id"`
}

func (MasterImportJob) TableName() string { return "master_import_jobs" }
