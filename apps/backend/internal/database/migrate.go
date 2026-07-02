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
	return nil
}
