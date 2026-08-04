package repositories

import (
	"time"

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

// IncrementScanned atomically bumps a package line's scanned count, guarded by
// the expected quantity. The single UPDATE (instead of read-modify-write) means
// two stations scanning the same line concurrently can never both count the
// same slot: the second scan either takes the next slot or is rejected as an
// over-scan (returns false).
func (r *PackageRepository) IncrementScanned(packageItemID uint) (bool, error) {
	res := r.db.Model(&models.PackageItem{}).
		Where("id = ? AND scanned_qty < expected_qty", packageItemID).
		Update("scanned_qty", gorm.Expr("scanned_qty + 1"))
	return res.RowsAffected > 0, res.Error
}

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

// ResolveOpenForEntity closes every still-open note attached to one entity in a
// single statement — used when the condition a note was raised for is provably
// gone (a QC-failed item that has now been re-made and passed). Returns how many
// notes were closed.
func (r *NoteRepository) ResolveOpenForEntity(entityType models.EntityType, entityID uint, byID *uint, resolution string, at time.Time) (int64, error) {
	return r.resolveOpen(entityType, entityID, "", byID, resolution, at)
}

// ResolveOpenForEntityReason is the narrow form: it closes only the notes raised
// for one specific reason. A settled cancellation request must clear its own
// required-attention note out of the ops inbox, but it says nothing about the
// other exceptions still open on the same order.
func (r *NoteRepository) ResolveOpenForEntityReason(entityType models.EntityType, entityID uint, reasonCode string, byID *uint, resolution string, at time.Time) (int64, error) {
	return r.resolveOpen(entityType, entityID, reasonCode, byID, resolution, at)
}

// resolveOpen closes matching open notes; an empty reasonCode means "any reason".
func (r *NoteRepository) resolveOpen(entityType models.EntityType, entityID uint, reasonCode string, byID *uint, resolution string, at time.Time) (int64, error) {
	q := r.db.Model(&models.Note{}).
		Where("entity_type = ? AND entity_id = ? AND status <> ?", entityType, entityID, models.NoteResolved)
	if reasonCode != "" {
		q = q.Where("reason_code = ?", reasonCode)
	}
	res := q.Updates(map[string]any{
		"status":                models.NoteResolved,
		"is_required_attention": false,
		"resolved_at":           at,
		"resolved_by_id":        byID,
		"resolution":            resolution,
	})
	return res.RowsAffected, res.Error
}

// ListByIDs returns the notes with these ids, one query per chunk.
func (r *NoteRepository) ListByIDs(ids []uint) ([]models.Note, error) {
	var out []models.Note
	for start := 0; start < len(ids); start += idChunk {
		end := start + idChunk
		if end > len(ids) {
			end = len(ids)
		}
		var rows []models.Note
		if err := r.db.Where("id IN ?", ids[start:end]).Order("id asc").Find(&rows).Error; err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// DeleteMany soft-deletes notes by id — one statement per chunk instead of one
// request per note. Notes reference other rows but nothing references a note, so
// there is no in-use guard here: a note can always go.
func (r *NoteRepository) DeleteMany(ids []uint) (int64, error) {
	var affected int64
	for start := 0; start < len(ids); start += idChunk {
		end := start + idChunk
		if end > len(ids) {
			end = len(ids)
		}
		res := r.db.Where("id IN ?", ids[start:end]).Delete(&models.Note{})
		if res.Error != nil {
			return affected, res.Error
		}
		affected += res.RowsAffected
	}
	return affected, nil
}

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

// CountByFilter reports how many notes match, ignoring pagination — the number a
// "select everything that matches" action operates on.
func (r *NoteRepository) CountByFilter(f NoteFilter) (int64, error) {
	var n int64
	err := r.baseQuery(f).Count(&n).Error
	return n, err
}

// DeleteByFilter soft-deletes EVERY note matching the filter in one statement,
// however many there are. This is what "xoá tất cả" runs on: shipping tens of
// thousands of ids to the server just to name the same set would be slower, and
// would silently truncate at whatever request/limit the client hits first.
func (r *NoteRepository) DeleteByFilter(f NoteFilter) (int64, error) {
	res := r.baseQuery(f).Delete(&models.Note{})
	return res.RowsAffected, res.Error
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

// ---------- Sidebar action counts ----------

// ActionCounts is the "work waiting for you" tally behind the sidebar badges.
type ActionCounts struct {
	Review        int64 `json:"review"`        // orders pending review
	Cancellations int64 `json:"cancellations"` // cancellation requests (orders + items)
	Notes         int64 `json:"notes"`         // notes flagged for attention
}

// ActionCounts returns all three badge numbers in ONE round-trip. The sidebar
// used to poll four list endpoints, each of which ran a COUNT and a SELECT and
// held its own pooled connection — four HTTP requests and eight statements just
// to draw three little numbers, repeated on every tab focus.
func (r *Repositories) ActionCounts() (ActionCounts, error) {
	var c ActionCounts
	err := r.DB.Raw(`
		SELECT
			(SELECT COUNT(*) FROM orders
			   WHERE deleted_at IS NULL AND review_status = ?) AS review,
			(SELECT COUNT(*) FROM orders
			   WHERE deleted_at IS NULL AND cancellation_status = ?)
			+ (SELECT COUNT(*) FROM order_items
			   WHERE deleted_at IS NULL AND cancellation_status = ?) AS cancellations,
			(SELECT COUNT(*) FROM notes
			   WHERE deleted_at IS NULL AND is_required_attention = true) AS notes`,
		models.ReviewPending, models.CancellationRequested, models.CancellationRequested,
	).Scan(&c).Error
	return c, err
}
