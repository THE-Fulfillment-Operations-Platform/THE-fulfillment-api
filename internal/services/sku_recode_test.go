package services

import (
	"strings"
	"testing"

	"the-fulfillment/backend/internal/models"
)

func recodeParentWithChildren(t *testing.T, svc *CatalogService, owner Actor) (*models.SKU, []*models.SKU) {
	t.Helper()
	parent, err := svc.CreateSKU(owner, SKUInput{Code: "CO", Name: "CO"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	var kids []*models.SKU
	for _, c := range []string{"CO1-C", "CO2-S"} {
		k, err := svc.CreateSKU(owner, SKUInput{Code: c, Name: c, ParentID: &parent.ID})
		if err != nil {
			t.Fatalf("create child %s: %v", c, err)
		}
		kids = append(kids, k)
	}
	return parent, kids
}

func TestUpdateSKU_RecodeOK(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	sku, _ := svc.CreateSKU(owner, SKUInput{Code: "AAA", Name: "A", ProductName: "Giữ nguyên"})

	got, err := svc.UpdateSKU(owner, sku.ID, SKUUpdateInput{Code: ptrStr("BBB")})
	if err != nil {
		t.Fatalf("recode: %v", err)
	}
	if got.Code != "BBB" || loadSKU(t, db, sku.ID).Code != "BBB" {
		t.Fatalf("code not changed: %q", got.Code)
	}
	if loadSKU(t, db, sku.ID).ProductName != "Giữ nguyên" {
		t.Fatalf("recode wiped other fields")
	}
	// The old code is free again and lookups follow the new one.
	if _, err := svc.repo.SKU.FindByCode("AAA"); err == nil {
		t.Fatalf("old code still resolves")
	}
	if f, err := svc.repo.SKU.FindByCode("BBB"); err != nil || f.ID != sku.ID {
		t.Fatalf("new code does not resolve to the SKU: %v", err)
	}
}

func TestUpdateSKU_RecodeKeepWhenOmittedOrSame(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	sku, _ := svc.CreateSKU(owner, SKUInput{Code: "AAA", Name: "A"})
	if _, err := svc.UpdateSKU(owner, sku.ID, SKUUpdateInput{Name: "A2"}); err != nil {
		t.Fatalf("omitted: %v", err)
	}
	// Same code, different spelling: normalises to the current code, not a clash with itself.
	if _, err := svc.UpdateSKU(owner, sku.ID, SKUUpdateInput{Code: ptrStr(" aaa ")}); err != nil {
		t.Fatalf("same code: %v", err)
	}
	if loadSKU(t, db, sku.ID).Code != "AAA" {
		t.Fatalf("code changed unexpectedly")
	}
}

func TestUpdateSKU_RecodeDuplicate(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	a, _ := svc.CreateSKU(owner, SKUInput{Code: "AAA", Name: "A"})
	_, _ = svc.CreateSKU(owner, SKUInput{Code: "BBB", Name: "B"})
	_, err := svc.UpdateSKU(owner, a.ID, SKUUpdateInput{Code: ptrStr("bbb")})
	if err == nil || !strings.Contains(err.Error(), "đã có SKU khác dùng") {
		t.Fatalf("want duplicate error in Vietnamese, got %v", err)
	}
	if loadSKU(t, db, a.ID).Code != "AAA" {
		t.Fatalf("failed rename changed the code")
	}
}

func TestUpdateSKU_RecodeEmpty(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	a, _ := svc.CreateSKU(owner, SKUInput{Code: "AAA", Name: "A"})
	for _, c := range []string{"", "   ", "@@@"} {
		_, err := svc.UpdateSKU(owner, a.ID, SKUUpdateInput{Code: ptrStr(c)})
		if err == nil || !strings.Contains(err.Error(), "không được để trống") {
			t.Fatalf("code %q: want empty error, got %v", c, err)
		}
	}
}

func TestUpdateSKU_RecodeNormalises(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	a, _ := svc.CreateSKU(owner, SKUInput{Code: "AAA", Name: "A"})
	got, err := svc.UpdateSKU(owner, a.ID, SKUUpdateInput{Code: ptrStr("  hộp đỏ-1 ")})
	if err != nil {
		t.Fatalf("recode: %v", err)
	}
	if got.Code != "HOP-DO-1" {
		t.Fatalf("want HOP-DO-1, got %q", got.Code)
	}
}

func TestUpdateSKU_RecodeFreesSoftDeletedCode(t *testing.T) {
	db := newCatalogDB(t)
	// Same shape as prod (ensureLiveCodeUniqueIndexes): unique among live rows only.
	db.Exec(`DROP INDEX IF EXISTS idx_skus_code`)
	if err := db.Exec(`CREATE UNIQUE INDEX idx_skus_code ON skus (code) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatalf("partial index: %v", err)
	}
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	a, _ := svc.CreateSKU(owner, SKUInput{Code: "AAA", Name: "A"})
	b, _ := svc.CreateSKU(owner, SKUInput{Code: "BBB", Name: "B"})
	if _, err := svc.DeleteSKUs(owner, []uint{b.ID}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Prod's unique index is partial (live rows only), so a soft-deleted code is reusable.
	if _, err := svc.UpdateSKU(owner, a.ID, SKUUpdateInput{Code: ptrStr("BBB")}); err != nil {
		t.Fatalf("reuse deleted code: %v", err)
	}
}

func TestUpdateSKU_RecodeParentKeepsChildren(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	parent, kids := recodeParentWithChildren(t, svc, owner)

	if _, err := svc.UpdateSKU(owner, parent.ID, SKUUpdateInput{Code: ptrStr("coh")}); err != nil {
		t.Fatalf("recode parent: %v", err)
	}
	if got := loadSKU(t, db, parent.ID); got.Code != "COH" {
		t.Fatalf("parent code %q", got.Code)
	}
	for _, k := range kids {
		got := loadSKU(t, db, k.ID)
		if got.ParentID == nil || *got.ParentID != parent.ID {
			t.Fatalf("child %s lost its parent", got.Code)
		}
		if got.Code != k.Code {
			t.Fatalf("child code changed: %s -> %s", k.Code, got.Code)
		}
	}
	full, err := svc.repo.SKU.FindByCode("COH")
	if err != nil {
		t.Fatalf("find COH: %v", err)
	}
	ch, err := svc.repo.SKU.ChildrenOf([]uint{full.ID})
	if err != nil || len(ch[full.ID]) != 2 {
		t.Fatalf("children of renamed parent: %v %v", len(ch[full.ID]), err)
	}
}

// A SKU renamed after orders were imported: the order line keeps its old
// sku_code, yet the parcel must still find the SKU's weight, box and HS code
// through sku_id (otherwise every old order reads "SKU không có trong Master Data").
func TestTHE_BuildParcelAfterSKURecode(t *testing.T) {
	e := newTHEEnv(t)
	sku := &models.SKU{Code: "OLD-1", Name: "Cũ", ShipWeightG: fptr(100), ShipLengthCM: fptr(10),
		ShipWidthCM: fptr(10), ShipHeightCM: fptr(1), DeclaredValue: fptr(5), HSCode: "44209000"}
	e.db.Create(sku)
	o := e.order(t, "100777")
	e.db.Model(&models.OrderItem{}).Where("order_id = ?", o.ID).
		Updates(map[string]any{"sku_code": "OLD-1", "sku_id": sku.ID})

	owner := Actor{ID: 1, Role: models.RoleOwner}
	if _, err := catalogSvc(e.db).UpdateSKU(owner, sku.ID, SKUUpdateInput{Code: ptrStr("NEW-1")}); err != nil {
		t.Fatalf("recode: %v", err)
	}
	var line models.OrderItem
	e.db.Where("order_id = ?", o.ID).First(&line)
	if line.SKUCode != "OLD-1" {
		t.Fatalf("order line sku_code must stay the import-time text, got %q", line.SKUCode)
	}
	full, _ := e.carrier.repo.Order.FindByID(o.ID)
	c, _, _ := e.carrier.activeTHE()
	p, problems := e.carrier.BuildParcel(c, full)
	if len(problems) > 0 || p == nil {
		t.Fatalf("renamed SKU broke the parcel: %v", problems)
	}
	if p.HSCode != "44209000" {
		t.Fatalf("HS code lost after rename: %q", p.HSCode)
	}
}
