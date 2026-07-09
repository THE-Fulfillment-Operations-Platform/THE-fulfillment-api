package database

import (
	"fmt"

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
		// production
		&models.Batch{},
		&models.BatchItem{},
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
		stmt := fmt.Sprintf(
			`DROP INDEX IF EXISTS %[1]s; CREATE UNIQUE INDEX %[1]s ON %[2]s (code) WHERE deleted_at IS NULL;`,
			index, table,
		)
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("database: partial unique index %s: %w", index, err)
		}
	}
	return nil
}
