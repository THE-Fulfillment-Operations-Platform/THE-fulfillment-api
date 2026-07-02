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

// Normalize clamps page/pageSize to sane defaults.
func (p Page) Normalize() Page {
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

// Offset computes the SQL offset.
func (p Page) Offset() int { return (p.Page - 1) * p.PageSize }

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
	Batch        *BatchRepository
	QC           *QCRepository
	Status       *StatusHistoryRepository
	Package      *PackageRepository
	Handoff      *HandoffRepository
	Note         *NoteRepository
	Audit        *AuditRepository
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
		Batch:        &BatchRepository{db: db},
		QC:           &QCRepository{db: db},
		Status:       &StatusHistoryRepository{db: db},
		Package:      &PackageRepository{db: db},
		Handoff:      &HandoffRepository{db: db},
		Note:         &NoteRepository{db: db},
		Audit:        &AuditRepository{db: db},
	}
}
