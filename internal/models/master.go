package models

import "math"

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
	// Permissions is the explicit tick list, or NULL for "the role's defaults"
	// (see permissions.go). EffectivePermissions is what that resolves to —
	// computed for responses, never stored.
	Permissions          PermList `json:"permissions"`
	EffectivePermissions []string `json:"effective_permissions,omitempty" gorm:"-"`
}

func (User) TableName() string { return "users" }

// FillEffectivePermissions resolves EffectivePermissions for a response.
func (u *User) FillEffectivePermissions() {
	u.EffectivePermissions = EffectivePerms(u.Role, u.Permissions)
}

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
	// LengthMM × WidthMM is the size of ONE unit of the material — one sheet —
	// in millimetres. Together with a SKU's own size it yields the production
	// quota (see ProductionQuota). nil = not declared: no quota, batches of this
	// material are never split.
	// (Before 2026-09-18 the quota was typed by hand into products_per_unit on
	// this table and on sku_materials. Those columns still exist in the database
	// — AutoMigrate never drops one — but no code reads or writes them.)
	LengthMM *float64 `json:"length_mm"`
	WidthMM  *float64 `json:"width_mm"`
}

func (Material) TableName() string { return "materials" }

// dimUnit is the integer grid sizes are measured on: hundredths of a millimetre.
// Sizes are stored rounded to 0.01 mm, so every size is a whole number of these.
const dimUnit = 100

// dimCells converts a size to whole hundredths of a millimetre (0 when absent).
func dimCells(v *float64) int64 {
	if v == nil || *v <= 0 {
		return 0
	}
	return int64(math.Round(*v * dimUnit))
}

// ProductionQuota is how many products of `sku` ONE unit (one sheet) of
// `material` yields: ⌊S_sheet / S_product⌋, both areas from the declared D × R.
// This is the customer's own rule (2026-09-18) and it is an upper bound — it
// ignores kerf and how the pieces actually nest — so a real sheet may yield a
// little less. 0 = no quota (either side has no size), which the batch splitter
// reads as "unlimited". A product larger than the sheet yields 1, never 0: a 0
// there would read as unlimited and pack every such product into one batch.
//
// The division is done in whole hundredths of a millimetre so that the exact
// boundary — where a sheet is precisely full — is decided by integer arithmetic
// and not by float rounding (0.3 / 0.1 is 2.9999… in float, ⌊…⌋ = 2, wrong).
func ProductionQuota(sku *SKU, material *Material) int {
	if sku == nil || material == nil {
		return 0
	}
	sheet := dimCells(material.LengthMM) * dimCells(material.WidthMM)
	product := dimCells(sku.LengthMM) * dimCells(sku.WidthMM)
	if sheet == 0 || product == 0 {
		return 0
	}
	if q := sheet / product; q >= 1 {
		return int(q)
	}
	return 1
}

// ProductFitsSheet reports whether a product with the given size fits on one
// sheet of the material at all — false only when both sizes are declared and the
// product is larger than the sheet.
func ProductFitsSheet(sku *SKU, material *Material) bool {
	if sku == nil || material == nil {
		return true
	}
	sheet := dimCells(material.LengthMM) * dimCells(material.WidthMM)
	product := dimCells(sku.LengthMM) * dimCells(sku.WidthMM)
	return sheet == 0 || product == 0 || product <= sheet
}

// SKU is a product setup that is fixed against one or more materials. A combo
// SKU references multiple materials and therefore produces multiple batches.
type SKU struct {
	Base
	Code        string `json:"code" gorm:"uniqueIndex;size:48;not null"`
	Name        string `json:"name" gorm:"size:160;not null"`
	ProductName string `json:"product_name" gorm:"size:200"`
	Description string `json:"description" gorm:"size:500"`
	IsCombo     bool   `json:"is_combo" gorm:"not null;default:false"`
	IsActive    bool   `json:"is_active" gorm:"not null;default:true"`
	// ParentID groups variant SKUs under a parent SKU: the parent "Hộp nhựa"
	// holds the children "Hộp nhựa bé", "Hộp nhựa lớn", "Hộp nhựa vuông"… The
	// tree is exactly TWO levels deep — a parent never has a parent of its own,
	// and a SKU that has children can't be given one (CatalogService enforces
	// it, the importers too). nil = top level: a parent, or a standalone SKU.
	// Deliberately a bare column with no association field, so AutoMigrate adds
	// an index and nothing else to a live table (no self-referencing FK).
	ParentID *uint `json:"parent_id" gorm:"index"`
	// LengthMM × WidthMM is the product's "D x R" (dài × rộng), in millimetres.
	// nil = not declared yet.
	LengthMM  *float64      `json:"length_mm"`
	WidthMM   *float64      `json:"width_mm"`
	Materials []SKUMaterial `json:"materials,omitempty" gorm:"foreignKey:SKUID"`
}

func (SKU) TableName() string { return "skus" }

// SKUMaterial links a SKU to the materials it is built from. The unique index on
// (sku_id, material_id) keeps the material set clean per SKU.
type SKUMaterial struct {
	Base
	SKUID      uint     `json:"sku_id" gorm:"column:sku_id;index:idx_sku_material,unique;not null"`
	MaterialID uint     `json:"material_id" gorm:"index:idx_sku_material,unique;not null"`
	Material   Material `json:"material,omitempty" gorm:"foreignKey:MaterialID"`
	// QuantityPerUnit is how much of this material ONE product consumes — a bill
	// of materials figure. It is NOT the production quota: that is derived from
	// the SKU's and the material's sizes (ProductionQuota), never stored.
	QuantityPerUnit int    `json:"quantity_per_unit" gorm:"not null;default:1"`
	Note            string `json:"note" gorm:"size:255"`
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
