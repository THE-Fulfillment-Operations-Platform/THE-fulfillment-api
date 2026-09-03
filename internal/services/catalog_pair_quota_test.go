package services

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newCatalogQuotaFixture(t *testing.T) (*gorm.DB, *CatalogService, *models.Material) {
	t.Helper()
	db := newSplitDB(t)
	repo := repositories.New(db)
	svc := &CatalogService{repo: repo, audit: &AuditService{repo: repo}}
	mat := &models.Material{Code: "MICA", Name: "Mica"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	return db, svc, mat
}

func pairQuota(t *testing.T, db *gorm.DB, skuID, materialID uint) *int {
	t.Helper()
	var link models.SKUMaterial
	if err := db.Where("sku_id = ? AND material_id = ?", skuID, materialID).First(&link).Error; err != nil {
		t.Fatalf("load sku-material: %v", err)
	}
	return link.ProductsPerUnit
}

// TestSKUMaterialQuota_OwnerCanSet: the production quota of a (SKU, material)
// pair is stored alongside the mapping, and the bill-of-materials figure next to
// it (quantity_per_unit — how much material ONE product eats) is left untouched.
func TestSKUMaterialQuota_OwnerCanSet(t *testing.T) {
	db, svc, mat := newCatalogQuotaFixture(t)
	quota := 4

	sku, err := svc.CreateSKU(Actor{ID: 1, Role: models.RoleOwner}, SKUInput{
		Code: "MICA-01", Name: "Khay mica",
		Materials: []SKUMaterialInput{
			{MaterialID: mat.ID, QuantityPerUnit: 2, ProductsPerUnit: &quota},
		},
	})
	if err != nil {
		t.Fatalf("create sku: %v", err)
	}
	got := pairQuota(t, db, sku.ID, mat.ID)
	if got == nil || *got != 4 {
		t.Fatalf("pair quota: got %v, want 4", got)
	}
	var link models.SKUMaterial
	if err := db.Where("sku_id = ?", sku.ID).First(&link).Error; err != nil {
		t.Fatalf("load link: %v", err)
	}
	if link.QuantityPerUnit != 2 {
		t.Fatalf("quantity_per_unit must stay the bill-of-materials figure, got %d", link.QuantityPerUnit)
	}
}

// TestSKUMaterialQuota_NonOwnerIgnored: the quota is an OWNER-only lever, same
// as the material-level one — another role sending the field cannot set it.
func TestSKUMaterialQuota_NonOwnerIgnored(t *testing.T) {
	db, svc, mat := newCatalogQuotaFixture(t)
	quota := 4

	sku, err := svc.CreateSKU(Actor{ID: 2, Role: models.RoleAdmin}, SKUInput{
		Code: "MICA-02", Name: "Khay mica",
		Materials: []SKUMaterialInput{{MaterialID: mat.ID, ProductsPerUnit: &quota}},
	})
	if err != nil {
		t.Fatalf("create sku: %v", err)
	}
	if got := pairQuota(t, db, sku.ID, mat.ID); got != nil {
		t.Fatalf("non-owner must not set the pair quota, got %v", *got)
	}

	// An OWNER sets it; a later ADMIN edit must neither change nor clear it.
	if _, err := svc.UpdateSKU(Actor{ID: 1, Role: models.RoleOwner}, sku.ID, SKUUpdateInput{
		Materials: []SKUMaterialInput{{MaterialID: mat.ID, ProductsPerUnit: &quota}},
	}); err != nil {
		t.Fatalf("owner update: %v", err)
	}
	if got := pairQuota(t, db, sku.ID, mat.ID); got == nil || *got != 4 {
		t.Fatalf("owner should have set the quota, got %v", got)
	}
	if _, err := svc.UpdateSKU(Actor{ID: 2, Role: models.RoleAdmin}, sku.ID, SKUUpdateInput{
		Materials: []SKUMaterialInput{{MaterialID: mat.ID}}, // no quota in the payload
	}); err != nil {
		t.Fatalf("admin update: %v", err)
	}
	if got := pairQuota(t, db, sku.ID, mat.ID); got == nil || *got != 4 {
		t.Fatalf("a non-owner edit must preserve the existing quota, got %v", got)
	}
}

// TestSKUMaterialQuota_OwnerCanClear: an OWNER clearing the pair quota drops the
// pair back to the material's default, it does not pin it to zero.
func TestSKUMaterialQuota_OwnerCanClear(t *testing.T) {
	db, svc, mat := newCatalogQuotaFixture(t)
	quota := 4
	owner := Actor{ID: 1, Role: models.RoleOwner}

	sku, err := svc.CreateSKU(owner, SKUInput{
		Code: "MICA-03", Name: "Khay mica",
		Materials: []SKUMaterialInput{{MaterialID: mat.ID, ProductsPerUnit: &quota}},
	})
	if err != nil {
		t.Fatalf("create sku: %v", err)
	}
	zero := 0
	if _, err := svc.UpdateSKU(owner, sku.ID, SKUUpdateInput{
		Materials: []SKUMaterialInput{{MaterialID: mat.ID, ProductsPerUnit: &zero}},
	}); err != nil {
		t.Fatalf("clear quota: %v", err)
	}
	if got := pairQuota(t, db, sku.ID, mat.ID); got != nil {
		t.Fatalf("quota ≤ 0 must store as nil (fall back to material), got %v", *got)
	}
}
