package repositories

import (
	"strings"
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
	// OwnerRole scopes the list to one role's queue. It is what keeps customer
	// support out of the factory's work: every note the pipeline raises carries
	// the role that must act on it (missing artwork → DESIGNER, QC defect →
	// PRODUCTION, cancellation request → OPS), and CS is confined to its own.
	// Set by the server from the caller's role, never from the query string.
	OwnerRole string
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
	if f.OwnerRole != "" {
		q = q.Where("owner_role = ?", f.OwnerRole)
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

// AuditScope is one "entity type + ids" clause of an audit search.
type AuditScope struct {
	Type string
	IDs  []uint
}

// AuditFilter narrows the audit trail server-side. Groups are ANDed; the scopes
// inside one group are ORed ("this order, or its items, or their batch parts").
// A group with no scopes matches nothing: the code was typed but resolved to no
// entity, and an empty answer is the honest one (not "everything").
type AuditFilter struct {
	Page
	Groups [][]AuditScope
	Actor  string     // substring of the actor's e-mail or full name
	From   *time.Time // created_at >= From
	Before *time.Time // created_at <  Before (exclusive, same as the order list)
}

// escapeLike makes user text literal inside a LIKE pattern (escape char '\').
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (r *AuditRepository) query(f AuditFilter) *gorm.DB {
	q := r.db.Model(&models.AuditLog{})
	for _, group := range f.Groups {
		var conds []string
		var args []interface{}
		for _, sc := range group {
			if len(sc.IDs) == 0 {
				continue
			}
			conds = append(conds, "(entity_type = ? AND entity_id IN ?)")
			args = append(args, sc.Type, sc.IDs)
		}
		if len(conds) == 0 {
			return q.Where("1 = 0")
		}
		q = q.Where("("+strings.Join(conds, " OR ")+")", args...)
	}
	if a := strings.ToLower(strings.TrimSpace(f.Actor)); a != "" {
		like := "%" + escapeLike(a) + "%"
		q = q.Where(`(LOWER(actor_email) LIKE ? ESCAPE '\' OR actor_id IN (SELECT id FROM users WHERE LOWER(full_name) LIKE ? ESCAPE '\'))`, like, like)
	}
	if f.From != nil {
		q = q.Where("created_at >= ?", *f.From)
	}
	if f.Before != nil {
		q = q.Where("created_at < ?", *f.Before)
	}
	return q
}

func (r *AuditRepository) List(p Page) ([]models.AuditLog, int64, error) {
	return r.Search(AuditFilter{Page: p})
}

// Search lists audit rows newest first. Every filter is a WHERE on an indexed
// column (entity_type/entity_id, actor_id, created_at) — never a table load.
func (r *AuditRepository) Search(f AuditFilter) ([]models.AuditLog, int64, error) {
	var rows []models.AuditLog
	var total int64
	if err := r.query(f).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := r.query(f).Order("id desc").Limit(f.PageSize).Offset(f.Offset()).Find(&rows).Error
	return rows, total, err
}

// resolveLimit bounds how many entities one typed code may expand to (a store
// order id can be shared by several orders; this keeps the IN lists small).
const resolveLimit = 200

// ResolveOrderScopes turns an order code (internal code, store order id, or the
// code of one of its items) into the audit scopes for that order, its items and
// their batch parts. Soft-deleted rows are included: ORDER_DELETE lines belong
// to exactly those orders.
func (r *AuditRepository) ResolveOrderScopes(code string) ([]AuditScope, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, nil
	}
	var orderIDs []uint
	if err := r.db.Unscoped().Model(&models.Order{}).
		Where("internal_code = ? OR store_order_id = ? OR id IN (SELECT order_id FROM order_items WHERE internal_code = ?)", code, code, code).
		Limit(resolveLimit).Pluck("id", &orderIDs).Error; err != nil {
		return nil, err
	}
	if len(orderIDs) == 0 {
		return nil, nil
	}
	var itemIDs []uint
	if err := r.db.Unscoped().Model(&models.OrderItem{}).Where("order_id IN ?", orderIDs).Pluck("id", &itemIDs).Error; err != nil {
		return nil, err
	}
	scopes := []AuditScope{{Type: "order", IDs: orderIDs}, {Type: "order_item", IDs: itemIDs}}
	if len(itemIDs) > 0 {
		var biIDs []uint
		if err := r.db.Unscoped().Model(&models.BatchItem{}).Where("order_item_id IN ?", itemIDs).Pluck("id", &biIDs).Error; err != nil {
			return nil, err
		}
		scopes = append(scopes, AuditScope{Type: "batch_item", IDs: biIDs})
	}
	return scopes, nil
}

// ResolveBatchScopes turns "#103000" / "103000" into the audit scopes of that
// batch and its parts.
func (r *AuditRepository) ResolveBatchScopes(code string) ([]AuditScope, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, nil
	}
	bare := strings.TrimLeft(code, "# ")
	var batchIDs []uint
	if err := r.db.Unscoped().Model(&models.Batch{}).
		Where("code IN ?", []string{"#" + bare, bare, code}).Limit(resolveLimit).Pluck("id", &batchIDs).Error; err != nil {
		return nil, err
	}
	if len(batchIDs) == 0 {
		return nil, nil
	}
	var biIDs []uint
	if err := r.db.Unscoped().Model(&models.BatchItem{}).Where("batch_id IN ?", batchIDs).Pluck("id", &biIDs).Error; err != nil {
		return nil, err
	}
	return []AuditScope{{Type: "batch", IDs: batchIDs}, {Type: "batch_item", IDs: biIDs}}, nil
}

// ---------- Sidebar action counts ----------

// ActionCounts is the "work waiting for you" tally behind the sidebar badges.
type ActionCounts struct {
	Review        int64 `json:"review"`        // orders pending review
	Cancellations int64 `json:"cancellations"` // cancellation requests (orders + items)
	Notes         int64 `json:"notes"`         // notes flagged for attention
}

// ActionCounts returns the badge numbers this role is actually shown, in ONE
// round-trip. The sidebar used to poll four list endpoints, each of which ran a
// COUNT and a SELECT and held its own pooled connection — four HTTP requests and
// eight statements just to draw three little numbers, repeated on every tab focus.
//
// The counts are role-scoped because the sidebar is: CS carries the notes item
// ("Ghi chú / Cần xử lý") but neither the review queue nor the cancellation
// queue, so counting those for them would be work done to produce a number no
// screen displays and no CS user may act on.
//
// CS's notes number is narrower still. Their queue is what a customer reported
// against an order — NOT the factory's work. Every note the pipeline raises
// names the role that must act on it (missing artwork → DESIGNER, QC defect →
// PRODUCTION, cancellation request → OPS), so counting notes owned by CS is
// exactly the line between "an order was flagged" and "the workshop has a
// problem". A badge counting the latter would send CS looking at jobs they
// cannot act on and must not touch.
func (r *Repositories) ActionCounts(role models.Role) (ActionCounts, error) {
	var c ActionCounts
	if role == models.RoleCS {
		err := r.DB.Raw(`
			SELECT (SELECT COUNT(*) FROM notes
			          WHERE deleted_at IS NULL AND is_required_attention = true
			            AND owner_role = ?) AS notes`, models.RoleCS).
			Scan(&c).Error
		return c, err
	}
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
