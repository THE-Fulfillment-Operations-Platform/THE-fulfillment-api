package services

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

func ptrFloat(f float64) *float64           { return &f }
func ptrStr(s string) *string               { return &s }
func dimEq(got *float64, want float64) bool { return got != nil && *got == want }

func loadSKU(t *testing.T, db *gorm.DB, id uint) models.SKU {
	t.Helper()
	var s models.SKU
	if err := db.First(&s, id).Error; err != nil {
		t.Fatalf("load sku %d: %v", id, err)
	}
	return s
}

// TestUpdateSKU_PartialPayloadKeepsTheRest: the mapping screen submits only
// `materials` and the hide/show toggle only `is_active`. Both used to blank the
// product name and description, because UpdateSKU copied every field whether it
// was sent or not.
func TestUpdateSKU_PartialPayloadKeepsTheRest(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	mat := &models.Material{Code: "MICA", Name: "Mica"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	sku, err := svc.CreateSKU(owner, SKUInput{
		Code: "HOP-BE", Name: "HOP-BE", ProductName: "Hộp nhựa bé", Description: "Mica trong 3 ly",
		LengthMM: ptrFloat(80), WidthMM: ptrFloat(60),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	hidden := false
	if _, err := svc.UpdateSKU(owner, sku.ID, SKUUpdateInput{IsActive: &hidden}); err != nil {
		t.Fatalf("toggle: %v", err)
	}
	if _, err := svc.UpdateSKU(owner, sku.ID, SKUUpdateInput{
		Materials: []SKUMaterialInput{{MaterialID: mat.ID, QuantityPerUnit: 1}},
	}); err != nil {
		t.Fatalf("mapping save: %v", err)
	}

	got := loadSKU(t, db, sku.ID)
	if got.IsActive {
		t.Fatalf("toggle did not apply")
	}
	if got.ProductName != "Hộp nhựa bé" || got.Description != "Mica trong 3 ly" {
		t.Fatalf("partial updates wiped the SKU: product_name=%q description=%q", got.ProductName, got.Description)
	}
	if !dimEq(got.LengthMM, 80) || !dimEq(got.WidthMM, 60) {
		t.Fatalf("partial updates touched D x R: %v x %v", got.LengthMM, got.WidthMM)
	}

	// Sending the field — even empty — still sets it: the edit form clears a
	// description this way.
	if _, err := svc.UpdateSKU(owner, sku.ID, SKUUpdateInput{Description: ptrStr("")}); err != nil {
		t.Fatalf("clear description: %v", err)
	}
	if got := loadSKU(t, db, sku.ID); got.Description != "" || got.ProductName != "Hộp nhựa bé" {
		t.Fatalf("explicit clear: product_name=%q description=%q", got.ProductName, got.Description)
	}
}

// TestSKUParent_TwoLevelTree: "Hộp nhựa" holds "Hộp nhựa bé/lớn"; a child can't
// hold children of its own and a parent can't be filed under someone else.
func TestSKUParent_TwoLevelTree(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	parent, err := svc.CreateSKU(owner, SKUInput{Code: "HOP-NHUA", Name: "HOP-NHUA", ProductName: "Hộp nhựa"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	small, err := svc.CreateSKU(owner, SKUInput{
		Code: "HOP-NHUA-BE", Name: "HOP-NHUA-BE", ParentID: &parent.ID,
		LengthMM: ptrFloat(80.456), WidthMM: ptrFloat(60),
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if small.ParentID == nil || *small.ParentID != parent.ID {
		t.Fatalf("child parent_id = %v, want %d", small.ParentID, parent.ID)
	}
	if !dimEq(small.LengthMM, 80.46) || !dimEq(small.WidthMM, 60) {
		t.Fatalf("D x R = %v x %v, want 80.46 x 60 (rounded to 0.01 mm)", small.LengthMM, small.WidthMM)
	}

	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"grandchild", func() error {
			_, err := svc.CreateSKU(owner, SKUInput{Code: "CHAU", Name: "CHAU", ParentID: &small.ID})
			return err
		}, "đang là SKU con"},
		{"missing parent", func() error {
			_, err := svc.CreateSKU(owner, SKUInput{Code: "MO-COI", Name: "MO-COI", ParentID: ptrUint(9999)})
			return err
		}, "không tồn tại"},
		{"parent with children filed under another", func() error {
			other, err := svc.CreateSKU(owner, SKUInput{Code: "KHAC", Name: "KHAC"})
			if err != nil {
				return err
			}
			_, err = svc.UpdateSKU(owner, parent.ID, SKUUpdateInput{ParentID: &other.ID})
			return err
		}, "đang là SKU cha của 1 SKU con"},
		{"own parent", func() error {
			_, err := svc.UpdateSKU(owner, parent.ID, SKUUpdateInput{ParentID: &parent.ID})
			return err
		}, "chính nó"},
		{"absurd size", func() error {
			_, err := svc.UpdateSKU(owner, small.ID, SKUUpdateInput{LengthMM: ptrFloat(1e9)})
			return err
		}, "Kích thước"},
	}
	for _, c := range cases {
		err := c.run()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.want)
		}
	}

	// Half a size is refused: D and R go together (an area needs both).
	if _, err := svc.UpdateSKU(owner, small.ID, SKUUpdateInput{WidthMM: ptrFloat(0)}); err == nil || !strings.Contains(err.Error(), "cả D lẫn R") {
		t.Fatalf("clearing one side only: err = %v, want a refusal", err)
	}
	if _, err := svc.CreateSKU(owner, SKUInput{Code: "NUA", Name: "NUA", LengthMM: ptrFloat(10)}); err == nil {
		t.Fatalf("creating with one side only must be refused")
	}

	// Detach with 0, clear the size with 0 on both sides.
	if _, err := svc.UpdateSKU(owner, small.ID, SKUUpdateInput{ParentID: ptrUint(0), LengthMM: ptrFloat(0), WidthMM: ptrFloat(0)}); err != nil {
		t.Fatalf("detach: %v", err)
	}
	got := loadSKU(t, db, small.ID)
	if got.ParentID != nil || got.WidthMM != nil || got.LengthMM != nil {
		t.Fatalf("after detach: parent=%v D=%v R=%v", got.ParentID, got.LengthMM, got.WidthMM)
	}
}

// TestDeleteSKUs_ParentGoesWithItsChildren: deleting a parent on its own would
// strand its children under an invisible parent, so it is skipped with a reason
// — unless every child goes in the same request.
func TestDeleteSKUs_ParentGoesWithItsChildren(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	parent := &models.SKU{Code: "HOP-NHUA", Name: "Hộp nhựa"}
	if err := db.Create(parent).Error; err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	a := &models.SKU{Code: "HOP-BE", Name: "bé", ParentID: &parent.ID}
	b := &models.SKU{Code: "HOP-LON", Name: "lớn", ParentID: &parent.ID}
	for _, s := range []*models.SKU{a, b} {
		if err := db.Create(s).Error; err != nil {
			t.Fatalf("seed child: %v", err)
		}
	}

	res, err := svc.DeleteSKUs(owner, []uint{parent.ID, a.ID})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(res.DeletedIDs) != 1 || res.DeletedIDs[0] != a.ID {
		t.Fatalf("deleted = %v, want only the child", res.DeletedIDs)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].ID != parent.ID || !strings.Contains(res.Skipped[0].Reason, "1 SKU con") {
		t.Fatalf("skipped = %+v, want the parent (1 child left)", res.Skipped)
	}

	res, err = svc.DeleteSKUs(owner, []uint{parent.ID, b.ID})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(res.DeletedIDs) != 2 || len(res.Skipped) != 0 {
		t.Fatalf("parent + last child: deleted=%v skipped=%+v, want both deleted", res.DeletedIDs, res.Skipped)
	}
}
