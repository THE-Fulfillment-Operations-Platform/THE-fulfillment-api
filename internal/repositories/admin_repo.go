package repositories

import (
	"strings"

	"gorm.io/gorm"
)

// AdminRepository holds destructive maintenance operations (data reset). It is
// deliberately separate from the per-entity repositories so the blast radius of
// a wipe is easy to review in one place.
type AdminRepository struct{ db *gorm.DB }

// transactionalTables lists every table holding order / production data, ordered
// child-before-parent (TRUNCATE ... CASCADE also handles ordering, this is just
// documentation of the graph). Master data (materials, skus, sku_materials,
// sellers, stores, users, roles, master_import_jobs) and audit_logs are
// intentionally preserved.
var transactionalTables = []string{
	"import_errors",
	"import_jobs",
	"item_assets",
	"order_items",
	"orders",
	"batch_items",
	"batches",
	"package_items",
	"packages",
	"handoffs",
	"qc_records",
	"status_histories",
}

// ResetTransactional hard-truncates all order/production tables and restarts
// their identity sequences. TRUNCATE (not GORM soft delete) is required because
// the unique indexes on these tables don't include deleted_at, so soft-deleted
// rows would collide with a subsequent re-import of the same codes. CASCADE
// satisfies the RESTRICT foreign keys in a single atomic statement. Returns the
// list of tables cleared.
func (r *AdminRepository) ResetTransactional() ([]string, error) {
	stmt := "TRUNCATE " + strings.Join(transactionalTables, ", ") + " RESTART IDENTITY CASCADE"
	if err := r.db.Exec(stmt).Error; err != nil {
		return nil, err
	}
	return transactionalTables, nil
}
