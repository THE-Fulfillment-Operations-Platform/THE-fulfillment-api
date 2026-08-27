// Package repositories is the data-access layer. Each repository wraps *gorm.DB
// and exposes intention-revealing methods; services never build raw queries and
// handlers never touch the database directly.
package repositories

import "gorm.io/gorm"

// Page describes pagination input.
type Page struct {
	Page     int
	PageSize int
}

// PageSizeAll is the page size the UI sends for "Tất cả": give me every matching
// row in one page. It reaches GORM as Limit(-1), which cancels the LIMIT clause,
// so no repository needs a special case.
const PageSizeAll = -1

// Normalize clamps page/pageSize to sane defaults. A negative page size is the
// explicit "Tất cả" request: it survives untouched (never clamped to 200) and
// pins the page to 1, because there is only one page.
func (p Page) Normalize() Page {
	if p.PageSize < 0 {
		return Page{Page: 1, PageSize: PageSizeAll}
	}
	if p.Page < 1 {
		p.Page = 1
	}
	if p.PageSize < 1 {
		p.PageSize = 20
	}
	if p.PageSize > 200 {
		p.PageSize = 200
	}
	return p
}

// All reports whether this page asks for the whole result set.
func (p Page) All() bool { return p.PageSize < 0 }

// Offset computes the SQL offset. "Tất cả" starts at row 0 — computing it from a
// negative page size would otherwise produce a bogus negative offset.
func (p Page) Offset() int {
	if p.All() {
		return 0
	}
	return (p.Page - 1) * p.PageSize
}

// Repositories bundles every repository so services receive a single dependency.
type Repositories struct {
	DB           *gorm.DB
	User         *UserRepository
	Seller       *SellerRepository
	Store        *StoreRepository
	Material     *MaterialRepository
	SKU          *SKURepository
	Import       *ImportRepository
	MasterImport *MasterImportRepository
	Order        *OrderRepository
	OrderItem    *OrderItemRepository
	Tracking     *TrackingRepository
	Batch        *BatchRepository
	QC           *QCRepository
	Status       *StatusHistoryRepository
	Package      *PackageRepository
	Handoff      *HandoffRepository
	Note         *NoteRepository
	Audit        *AuditRepository
	Admin        *AdminRepository
}

// New builds the repository bundle from a GORM handle.
func New(db *gorm.DB) *Repositories {
	return &Repositories{
		DB:           db,
		User:         &UserRepository{db: db},
		Seller:       &SellerRepository{db: db},
		Store:        &StoreRepository{db: db},
		Material:     &MaterialRepository{db: db},
		SKU:          &SKURepository{db: db},
		Import:       &ImportRepository{db: db},
		MasterImport: &MasterImportRepository{db: db},
		Order:        &OrderRepository{db: db},
		OrderItem:    &OrderItemRepository{db: db},
		Tracking:     &TrackingRepository{db: db},
		Batch:        &BatchRepository{db: db},
		QC:           &QCRepository{db: db},
		Status:       &StatusHistoryRepository{db: db},
		Package:      &PackageRepository{db: db},
		Handoff:      &HandoffRepository{db: db},
		Note:         &NoteRepository{db: db},
		Audit:        &AuditRepository{db: db},
		Admin:        &AdminRepository{db: db},
	}
}
