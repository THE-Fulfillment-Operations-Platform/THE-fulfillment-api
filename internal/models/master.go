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
	// in millimetres. Together with a SKU's own size it gives the ESTIMATED
	// production quota (EstimatedQuota) for pairs the factory has not declared
	// one for. nil = not declared.
	// (materials.products_per_unit, the pre-18/09 material-level quota, still
	// exists in the database — AutoMigrate never drops a column — but nothing
	// reads or writes it; the declared quota lives on sku_materials.)
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

// QuotaSource says where a pair's production quota came from.
type QuotaSource string

const (
	// QuotaDeclared: typed by the factory for this (SKU, material) pair — the
	// number from its real layout file. Always wins.
	QuotaDeclared QuotaSource = "declared"
	// QuotaEstimated: derived from the two sizes by grid packing. An estimate:
	// it ignores kerf, margins and clever nesting.
	QuotaEstimated QuotaSource = "estimated"
	// QuotaNone: nothing declared and a size missing on either side.
	QuotaNone QuotaSource = ""
)

// DeclaredQuota is the quota the factory declared for (sku, materialID), or 0.
// It reads sku.Materials, so the caller must have loaded them.
func DeclaredQuota(sku *SKU, materialID uint) int {
	if sku == nil {
		return 0
	}
	for i := range sku.Materials {
		m := &sku.Materials[i]
		if m.MaterialID == materialID && m.ProductsPerUnit != nil && *m.ProductsPerUnit > 0 {
			return *m.ProductsPerUnit
		}
	}
	return 0
}

// EstimatedQuota is how many products of `sku` ONE sheet of `material` yields
// by laying the product's D × R box out on a grid, in the better of the two
// orientations: ⌊L/l⌋·⌊W/w⌋ or ⌊L/w⌋·⌊W/l⌋. 0 = a size is missing on either
// side. A product larger than the sheet yields 1, never 0: a 0 would read as
// "no quota" and pack every such product into one batch.
//
// Grid, not area: the customer showed (2026-09-24) that ⌊S_sheet / S_product⌋
// overstates — a 600×800 mica sheet holds 24 pieces of 127×127 laid out, not
// 29 — and asked to declare the real number per pair instead (DeclaredQuota).
// This estimate only fills in for pairs nobody has declared yet.
//
// Sizes are compared in whole hundredths of a millimetre so the boundary —
// a piece that fits exactly — is decided by integer arithmetic, not float
// rounding.
func EstimatedQuota(sku *SKU, material *Material) int {
	if sku == nil || material == nil {
		return 0
	}
	sheetL, sheetW := dimCells(material.LengthMM), dimCells(material.WidthMM)
	prodL, prodW := dimCells(sku.LengthMM), dimCells(sku.WidthMM)
	if sheetL == 0 || sheetW == 0 || prodL == 0 || prodW == 0 {
		return 0
	}
	upright := (sheetL / prodL) * (sheetW / prodW)
	rotated := (sheetL / prodW) * (sheetW / prodL)
	if rotated > upright {
		upright = rotated
	}
	if upright < 1 {
		return 1
	}
	return int(upright)
}

// ProductionQuotaSource is the quota the batch splitter uses for (sku,
// material) and where it came from: the declared number when the factory
// typed one, else the grid estimate, else 0 (no quota → the SKU is never split).
func ProductionQuotaSource(sku *SKU, material *Material) (int, QuotaSource) {
	if sku == nil || material == nil {
		return 0, QuotaNone
	}
	if q := DeclaredQuota(sku, material.ID); q > 0 {
		return q, QuotaDeclared
	}
	if q := EstimatedQuota(sku, material); q > 0 {
		return q, QuotaEstimated
	}
	return 0, QuotaNone
}

// ProductionQuota is ProductionQuotaSource without the source.
func ProductionQuota(sku *SKU, material *Material) int {
	q, _ := ProductionQuotaSource(sku, material)
	return q
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
	// of materials figure. It is NOT the production quota.
	QuantityPerUnit int `json:"quantity_per_unit" gorm:"not null;default:1"`
	// ProductsPerUnit is the DECLARED production quota of this (SKU, material)
	// pair: how many products of the SKU one sheet of the material yields, as
	// the factory knows it from its real layout file (customer, 2026-09-24: the
	// size-derived number overstates, so the declared one always wins — see
	// ProductionQuotaSource). nil / ≤0 = not declared → estimated from sizes.
	// Same nullable column the pre-18/09 code used, so AutoMigrate leaves the
	// live table alone.
	ProductsPerUnit *int   `json:"products_per_unit"`
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
