package repositories

import (
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// ---------- Packages ----------

type PackageRepository struct{ db *gorm.DB }

func activePackageItems(db *gorm.DB) *gorm.DB {
	return db.Joins("JOIN order_items ON order_items.id = package_items.order_item_id").
		Where("order_items.cancellation_status NOT IN ?", []models.CancellationStatus{models.CancellationSeller, models.CancellationApproved})
}

func (r *PackageRepository) Create(p *models.Package) error { return r.db.Create(p).Error }
func (r *PackageRepository) Update(p *models.Package) error { return r.db.Save(p).Error }

func (r *PackageRepository) CreateItems(items []models.PackageItem) error {
	if len(items) == 0 {
		return nil
	}
	return r.db.Create(&items).Error
}

func (r *PackageRepository) UpdateItem(it *models.PackageItem) error { return r.db.Save(it).Error }

func (r *PackageRepository) FindByID(id uint) (*models.Package, error) {
	var p models.Package
	err := r.db.Preload("Items", activePackageItems).Preload("Items.OrderItem").Preload("Order").First(&p, id).Error
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PackageRepository) FindOpenByOrder(orderID uint) (*models.Package, error) {
	var p models.Package
	err := r.db.Preload("Items", activePackageItems).Preload("Items.OrderItem").
		Where("order_id = ? AND status = ?", orderID, models.PackageOpen).First(&p).Error
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PackageRepository) List(p Page, orderID *uint) ([]models.Package, int64, error) {
	var rows []models.Package
	var total int64
	q := r.db.Model(&models.Package{})
	if orderID != nil {
		q = q.Where("order_id = ?", *orderID)
	}
	q.Count(&total)
	err := q.Preload("Items", activePackageItems).Order("id desc").Limit(p.PageSize).Offset(p.Offset()).Find(&rows).Error
	return rows, total, err
}

// ---------- Handoffs ----------

type HandoffRepository struct{ db *gorm.DB }

func (r *HandoffRepository) Create(h *models.Handoff) error { return r.db.Create(h).Error }
func (r *HandoffRepository) Update(h *models.Handoff) error { return r.db.Save(h).Error }

func (r *HandoffRepository) FindByID(id uint) (*models.Handoff, error) {
	var h models.Handoff
	if err := r.db.First(&h, id).Error; err != nil {
		return nil, err
	}
	return &h, nil
}

func (r *HandoffRepository) List(p Page) ([]models.Handoff, int64, error) {
	var rows []models.Handoff
	var total int64
	r.db.Model(&models.Handoff{}).Count(&total)
	err := r.db.Order("id desc").Limit(p.PageSize).Offset(p.Offset()).Find(&rows).Error
	return rows, total, err
}

// ---------- Notes / Required Attention ----------

type NoteFilter struct {
	Page
	Status            string
	Severity          string
	EntityType        string
	EntityID          *uint
	RequiredAttention *bool
}

type NoteRepository struct{ db *gorm.DB }

func (r *NoteRepository) Create(n *models.Note) error { return r.db.Create(n).Error }
func (r *NoteRepository) Update(n *models.Note) error { return r.db.Save(n).Error }
func (r *NoteRepository) Delete(id uint) error        { return r.db.Delete(&models.Note{}, id).Error }

func (r *NoteRepository) FindByID(id uint) (*models.Note, error) {
	var n models.Note
	if err := r.db.First(&n, id).Error; err != nil {
		return nil, err
	}
	return &n, nil
}

func (r *NoteRepository) baseQuery(f NoteFilter) *gorm.DB {
	q := r.db.Model(&models.Note{})
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Severity != "" {
		q = q.Where("severity = ?", f.Severity)
	}
	if f.EntityType != "" {
		q = q.Where("entity_type = ?", f.EntityType)
	}
	if f.EntityID != nil {
		q = q.Where("entity_id = ?", *f.EntityID)
	}
	if f.RequiredAttention != nil {
		q = q.Where("is_required_attention = ?", *f.RequiredAttention)
	}
	return q
}

func (r *NoteRepository) List(f NoteFilter) ([]models.Note, int64, error) {
	var rows []models.Note
	var total int64
	r.baseQuery(f).Count(&total)
	err := r.baseQuery(f).Order("id desc").Limit(f.PageSize).Offset(f.Offset()).Find(&rows).Error
	return rows, total, err
}

// ---------- Audit logs ----------

type AuditRepository struct{ db *gorm.DB }

func (r *AuditRepository) Create(a *models.AuditLog) error { return r.db.Create(a).Error }

func (r *AuditRepository) List(p Page) ([]models.AuditLog, int64, error) {
	var rows []models.AuditLog
	var total int64
	r.db.Model(&models.AuditLog{}).Count(&total)
	err := r.db.Order("id desc").Limit(p.PageSize).Offset(p.Offset()).Find(&rows).Error
	return rows, total, err
}
