package database

import (
	"fmt"
	"log"
	"os"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// AutoMigrate creates/updates every table for the MVP. Order matters only for
// readability; GORM resolves FK dependencies. For the MVP we rely on GORM
// AutoMigrate; a dedicated migration tool can be introduced later without
// changing the model definitions.
func AutoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		// master data
		&models.RoleRecord{},
		&models.Seller{},
		&models.Store{},
		&models.User{},
		&models.Material{},
		&models.SKU{},
		&models.SKUMaterial{},
		// import
		&models.ImportJob{},
		&models.ImportError{},
		&models.MasterImportJob{},
		// orders
		&models.Order{},
		&models.OrderItem{},
		&models.ItemAsset{},
		&models.DailyCounter{},
		// production
		&models.Batch{},
		&models.BatchItem{},
		&models.BatchLink{},
		&models.StatusHistory{},
		&models.QCRecord{},
		// fulfillment
		&models.Package{},
		&models.PackageItem{},
		&models.Handoff{},
		&models.Note{},
		&models.AuditLog{},
	); err != nil {
		return fmt.Errorf("database: auto-migrate: %w", err)
	}
	if err := ensureLiveCodeUniqueIndexes(db); err != nil {
		return err
	}
	if err := dropLegacySellerStoreOrderIndex(db); err != nil {
		return err
	}
	if err := ensurePerformanceIndexes(db); err != nil {
		return err
	}
	if err := backfillOrderDailySeq(db); err != nil {
		return err
	}
	return nil
}

// backfillOrderDailySeq assigns a stable per-day sequence ("STT trong ngày") to
// every legacy order that predates the daily_seq feature, then seeds the
// daily_counters allocator so newly created orders continue after the max. It is
// idempotent: it only touches orders whose order_date is still unset and is a
// no-op once every order has been numbered. Postgres-only (the window function +
// AT TIME ZONE conversion); the sqlite test harness starts empty so never needs it.
func backfillOrderDailySeq(db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	var pending int64
	if err := db.Model(&models.Order{}).
		Where("order_date IS NULL OR order_date = ''").Count(&pending).Error; err != nil {
		return fmt.Errorf("database: count orders needing daily_seq: %w", err)
	}
	if pending == 0 {
		return nil
	}
	tz := os.Getenv("DB_TIMEZONE")
	if tz == "" {
		tz = "Asia/Ho_Chi_Minh"
	}
	// Number each order within its business-timezone calendar day, ordered by
	// creation time then id (so the number is deterministic and ties break stably).
	backfill := `
		WITH numbered AS (
		  SELECT id,
		         to_char(created_at AT TIME ZONE ?, 'YYYY-MM-DD') AS d,
		         ROW_NUMBER() OVER (
		           PARTITION BY (created_at AT TIME ZONE ?)::date
		           ORDER BY created_at, id
		         ) AS rn
		    FROM orders
		   WHERE deleted_at IS NULL
		)
		UPDATE orders o
		   SET order_date = n.d, daily_seq = n.rn
		  FROM numbered n
		 WHERE o.id = n.id AND (o.order_date IS NULL OR o.order_date = '')`
	if err := db.Exec(backfill, tz, tz).Error; err != nil {
		return fmt.Errorf("database: backfill order daily_seq: %w", err)
	}
	// Seed the counter so the next order created each day continues after the max
	// already assigned. GREATEST keeps any higher value a live allocation may hold.
	seed := `
		INSERT INTO daily_counters (scope_date, seq, updated_at)
		SELECT order_date, MAX(daily_seq), now()
		  FROM orders
		 WHERE order_date IS NOT NULL AND order_date <> ''
		 GROUP BY order_date
		ON CONFLICT (scope_date) DO UPDATE SET seq = GREATEST(daily_counters.seq, EXCLUDED.seq)`
	if err := db.Exec(seed).Error; err != nil {
		return fmt.Errorf("database: seed daily_counters: %w", err)
	}
	return nil
}

// ensurePerformanceIndexes creates the composite/partial indexes the hot list
// and lookup queries need at 100k–1M rows. GORM struct tags can only express
// single-column and full-table indexes, so these live here as idempotent raw
// SQL (Postgres-only; sqlite tests don't need them — the datasets are tiny).
//
// Every operational query in this codebase filters soft-deletes
// (deleted_at IS NULL), so the partial variants keep the indexes small and are
// always applicable.
func ensurePerformanceIndexes(db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	stmts := []string{
		// Seller-scoped order lists (seller portal + ops filters) paginate with
		// ORDER BY id DESC — this walks the index directly instead of sorting.
		`CREATE INDEX IF NOT EXISTS idx_orders_seller_page ON orders (seller_id, id DESC) WHERE deleted_at IS NULL`,
		// Duplicate StoreOrderID detection + FindBySellerAndStoreOrder both hit
		// (seller_id, store_order_id); the GROUP BY becomes an index-only scan.
		`CREATE INDEX IF NOT EXISTS idx_orders_seller_store_order ON orders (seller_id, store_order_id) WHERE deleted_at IS NULL`,
		// Review queue: review_status IN (...) ORDER BY id DESC LIMIT n.
		`CREATE INDEX IF NOT EXISTS idx_orders_review_page ON orders (review_status, id DESC) WHERE deleted_at IS NULL`,
		// Ops order list filtered by seller_status, newest first.
		`CREATE INDEX IF NOT EXISTS idx_orders_seller_status_page ON orders (seller_status, id DESC) WHERE deleted_at IS NULL`,
		// "STT trong ngày" lookups + stable ordering by the per-day sequence.
		`CREATE INDEX IF NOT EXISTS idx_orders_daily_seq ON orders (order_date, daily_seq) WHERE deleted_at IS NULL`,
		// Tracking-number lookups (order list badge + future provider sync).
		`CREATE INDEX IF NOT EXISTS idx_orders_tracking_number ON orders (tracking_number) WHERE deleted_at IS NULL AND tracking_number <> ''`,
		// Item list filtered by internal_status (design/QC/packing views).
		`CREATE INDEX IF NOT EXISTS idx_order_items_status_page ON order_items (internal_status, id DESC) WHERE deleted_at IS NULL`,
		// Item cancellation-request queue: status filter + ORDER BY requested_at.
		`CREATE INDEX IF NOT EXISTS idx_order_items_cancel_queue ON order_items (cancellation_requested_at) WHERE cancellation_status = 'REQUESTED' AND deleted_at IS NULL`,
		// The single-column seller_id index is fully covered by the two composite
		// indexes above; drop it to save write amplification. (The struct tag that
		// created it is removed alongside this migration.)
		`DROP INDEX IF EXISTS idx_orders_seller_id`,
		// Concurrency guard: at most ONE open package per order, so two packing
		// stations scanning the same order's first item can't each create a package.
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_packages_open_order ON packages (order_id) WHERE status = 'OPEN' AND deleted_at IS NULL`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("database: performance index (%s): %w", stmt, err)
		}
	}

	// Trigram index for the orders search box (store_order_id ILIKE '%kw%').
	// Without it every search is a sequential scan. pg_trgm ships with Postgres
	// but CREATE EXTENSION needs sufficient privileges — degrade gracefully so a
	// managed database without it still boots (search just stays unindexed).
	if err := db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_trgm`).Error; err != nil {
		log.Printf("database: pg_trgm unavailable (%v) — store_order_id search stays unindexed", err)
		return nil
	}
	if err := db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_orders_store_order_trgm ON orders USING gin (store_order_id gin_trgm_ops)`,
	).Error; err != nil {
		return fmt.Errorf("database: trigram index: %w", err)
	}
	return nil
}

// dropLegacySellerStoreOrderIndex removes the old unique index on
// (seller_id, store_order_ref). A StoreOrderID is now a repeatable reference
// label — the same store order can span many items and re-imports, each becoming
// its own order with its own unique InternalCode — so uniqueness lives solely on
// order_items.internal_code. GORM's AutoMigrate never drops an index on its own
// when the struct tag is removed, so we drop it explicitly. Idempotent: guarded by
// HasIndex so it is a no-op once gone (and on fresh databases that never had it).
func dropLegacySellerStoreOrderIndex(db *gorm.DB) error {
	if db.Migrator().HasIndex(&models.Order{}, "idx_seller_store_order") {
		if err := db.Migrator().DropIndex(&models.Order{}, "idx_seller_store_order"); err != nil {
			return fmt.Errorf("database: drop idx_seller_store_order: %w", err)
		}
	}
	return nil
}

// ensureLiveCodeUniqueIndexes rewrites the unique index on `code` for the master
// tables into a PARTIAL unique index that ignores soft-deleted rows
// (`WHERE deleted_at IS NULL`).
//
// GORM's `uniqueIndex` tag creates a plain unique index that spans *all* rows,
// including soft-deleted ones. Because lookups run with the default scope
// (`deleted_at IS NULL`), a soft-deleted material/SKU/seller is invisible to the
// code yet still reserves its `code` in the index — so re-creating or importing
// the same code blows up with a duplicate-key error (SQLSTATE 23505). Making the
// index partial lets a deleted code be reused while still keeping codes unique
// among live rows.
func ensureLiveCodeUniqueIndexes(db *gorm.DB) error {
	// Partial indexes with a WHERE clause are Postgres-specific; the raw SQL below
	// (and pg_indexes) only makes sense there. Tests run on sqlite and never hit
	// this path, but guard anyway so the wrapper stays portable.
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	for _, table := range []string{"materials", "skus", "sellers"} {
		index := "idx_" + table + "_code"
		// Skip when it is already partial so we don't drop/recreate (and briefly
		// lose the uniqueness guarantee) on every boot.
		var partial bool
		if err := db.Raw(
			`SELECT COALESCE(bool_or(indexdef ILIKE '%WHERE (deleted_at IS NULL)%'), false)
			   FROM pg_indexes WHERE indexname = ?`, index,
		).Scan(&partial).Error; err != nil {
			return fmt.Errorf("database: inspect index %s: %w", index, err)
		}
		if partial {
			continue
		}
		// Run DROP and CREATE as SEPARATE Exec calls. The GORM connection uses
		// PrepareStmt (extended protocol), which rejects multiple commands in one
		// prepared statement (SQLSTATE 42601). Two single-statement Execs reach the
		// same end state and work on a fresh database (e.g. first boot on Supabase).
		if err := db.Exec(fmt.Sprintf(`DROP INDEX IF EXISTS %s;`, index)).Error; err != nil {
			return fmt.Errorf("database: drop index %s: %w", index, err)
		}
		if err := db.Exec(fmt.Sprintf(
			`CREATE UNIQUE INDEX %s ON %s (code) WHERE deleted_at IS NULL;`, index, table,
		)).Error; err != nil {
			return fmt.Errorf("database: partial unique index %s: %w", index, err)
		}
	}
	return nil
}
