package repositories

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Material{}, &models.SKU{}, &models.SKUMaterial{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestReplaceMaterials_RepeatedSaveSameMaterial locks in the fix for the
// soft-delete + unique-index collision. sku_materials has a unique index on
// (sku_id, material_id) and SKUMaterial soft-deletes via Base.DeletedAt, so a
// plain Delete leaves the old row physically present. Re-saving the SAME mapping
// (the exact "Lưu mapping" second time) then collided on the unique index.
// ReplaceMaterials must hard-delete (Unscoped) so the re-save succeeds and no
// soft-deleted junk accumulates.
func TestReplaceMaterials_RepeatedSaveSameMaterial(t *testing.T) {
	db := newTestDB(t)
	repo := New(db)

	mat := &models.Material{Code: "SILICON", Name: "Silicon dẻo"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	sku := &models.SKU{Code: "CAGIU1", Name: "Ca giữ 1"}
	if err := db.Create(sku).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}

	// First save — this always worked, even before the fix.
	if err := repo.SKU.ReplaceMaterials(sku.ID, []models.SKUMaterial{
		{MaterialID: mat.ID, QuantityPerUnit: 1},
	}); err != nil {
		t.Fatalf("first ReplaceMaterials: %v", err)
	}

	// Second save with the SAME material but a new quantity — the regression.
	if err := repo.SKU.ReplaceMaterials(sku.ID, []models.SKUMaterial{
		{MaterialID: mat.ID, QuantityPerUnit: 2},
	}); err != nil {
		t.Fatalf("second ReplaceMaterials (regression — unique-index collision): %v", err)
	}

	// Exactly one live mapping row remains, carrying the updated quantity.
	var live []models.SKUMaterial
	if err := db.Where("sku_id = ?", sku.ID).Find(&live).Error; err != nil {
		t.Fatalf("query live rows: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("want 1 live mapping, got %d", len(live))
	}
	if live[0].QuantityPerUnit != 2 {
		t.Fatalf("want quantity 2 after re-save, got %d", live[0].QuantityPerUnit)
	}

	// And no soft-deleted junk left behind: the hard delete physically removed the
	// old row. (With a plain soft delete this count would be 2.)
	var physical int64
	if err := db.Unscoped().Model(&models.SKUMaterial{}).Where("sku_id = ?", sku.ID).Count(&physical).Error; err != nil {
		t.Fatalf("count physical rows: %v", err)
	}
	if physical != 1 {
		t.Fatalf("want 1 physical row (no soft-deleted junk), got %d", physical)
	}
}

// TestReplaceMaterials_SwapMaterialSet verifies a genuine replace (different
// material set) works and leaves only the new set live.
func TestReplaceMaterials_SwapMaterialSet(t *testing.T) {
	db := newTestDB(t)
	repo := New(db)

	a := &models.Material{Code: "A", Name: "Mat A"}
	b := &models.Material{Code: "B", Name: "Mat B"}
	if err := db.Create(a).Error; err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := db.Create(b).Error; err != nil {
		t.Fatalf("seed B: %v", err)
	}
	sku := &models.SKU{Code: "SKU1", Name: "SKU 1"}
	if err := db.Create(sku).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}

	if err := repo.SKU.ReplaceMaterials(sku.ID, []models.SKUMaterial{{MaterialID: a.ID}}); err != nil {
		t.Fatalf("map A: %v", err)
	}
	if err := repo.SKU.ReplaceMaterials(sku.ID, []models.SKUMaterial{{MaterialID: b.ID}}); err != nil {
		t.Fatalf("swap to B: %v", err)
	}

	var live []models.SKUMaterial
	if err := db.Where("sku_id = ?", sku.ID).Find(&live).Error; err != nil {
		t.Fatalf("query live: %v", err)
	}
	if len(live) != 1 || live[0].MaterialID != b.ID {
		t.Fatalf("want only material B live, got %+v", live)
	}
}
