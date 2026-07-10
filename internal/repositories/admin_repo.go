package repositories

import (
	"fmt"
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

// purgePasses is how many times PurgeSoftDeleted sweeps the table set, so a
// soft-deleted parent gets purged in a later pass once its soft-deleted children
// (RESTRICT FKs) are gone. The FK graph here is shallow (parent→child depth ≤ 3);
// 5 leaves margin.
const purgePasses = 5

// PurgeSoftDeleted hard-deletes rows that were soft-deleted (GORM's deleted_at)
// more than retentionDays ago, across every base table that carries a deleted_at
// column. Soft delete is kept for its recovery/audit value, but without a purge
// those rows accumulate forever — this is the periodic garbage collection. It is
// table-agnostic (discovers deleted_at columns at runtime) so new soft-deletable
// tables are covered automatically.
//
// Hardened after review:
//   - retentionDays MUST be > 0. A value <= 0 collapses the cutoff to now() and
//     would hard-delete EVERY soft-deleted row immediately, obliterating the
//     recovery window — so it is refused, not clamped.
//   - Each table is purged in its OWN transaction (a separate Exec) so progress
//     is durable and locks stay short, instead of one giant all-or-nothing tx.
//   - Within a table, rows are deleted ONE AT A TIME (by ctid), each in its own
//     savepoint, so a single row still referenced by a LIVE child is skipped —
//     not rolled back together with every other aged row in that table.
func (r *AdminRepository) PurgeSoftDeleted(retentionDays int) error {
	if retentionDays <= 0 {
		return fmt.Errorf("purge refused: retentionDays must be > 0, got %d", retentionDays)
	}

	var tables []string
	if err := r.db.Raw(`
		SELECT c.table_name
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = 'public'
		  AND c.column_name = 'deleted_at'
		  AND t.table_type = 'BASE TABLE'
		ORDER BY c.table_name`).Scan(&tables).Error; err != nil {
		return err
	}

	var lastErr error
	for pass := 0; pass < purgePasses; pass++ {
		for _, tbl := range tables {
			if err := r.purgeTable(tbl, retentionDays); err != nil {
				lastErr = err // record but keep going: one bad table mustn't block the rest
			}
		}
	}
	return lastErr
}

// purgeTable hard-deletes one table's rows soft-deleted before the cutoff, one
// row at a time (by ctid) so a single FK-blocked (live-referenced) row is skipped
// instead of aborting the whole delete. Runs as its own transaction.
func (r *AdminRepository) purgeTable(table string, retentionDays int) error {
	// table comes from the catalog; still quote it as an identifier defensively.
	ident := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
	stmt := fmt.Sprintf(`
DO $$
DECLARE c tid;
BEGIN
  FOR c IN
    SELECT ctid FROM public.%[1]s
    WHERE deleted_at IS NOT NULL AND deleted_at < now() - make_interval(days => %[2]d)
  LOOP
    BEGIN
      DELETE FROM public.%[1]s WHERE ctid = c;
    EXCEPTION WHEN foreign_key_violation THEN
      NULL; -- still referenced by a live row; retry on a future run
    END;
  END LOOP;
END $$;`, ident, retentionDays)
	return r.db.Exec(stmt).Error
}
