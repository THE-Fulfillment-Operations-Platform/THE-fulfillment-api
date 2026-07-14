package services

import (
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newCatalogDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Material{}, &models.SKU{}, &models.SKUMaterial{}, &models.AuditLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func catalogSvc(db *gorm.DB) *CatalogService {
	repo := repositories.New(db)
	return &CatalogService{repo: repo, audit: &AuditService{repo: repo}}
}

func ptrInt(n int) *int { return &n }

func TestParseQuotaCell(t *testing.T) {
	cases := []struct {
		in   string
		want *int
		ok   bool
	}{
		{"", nil, true}, // blank = unlimited
		{"20", ptrInt(20), true},
		{"20 sp/tấm", ptrInt(20), true}, // lenient: pull the number out
		{"0", nil, true},                // ≤0 = unlimited
		{"-5", nil, true},
		{"abc", nil, false}, // no number → flagged
	}
	for _, c := range cases {
		got, ok := parseQuotaCell(c.in)
		if ok != c.ok || !quotaEqual(got, c.want) {
			t.Fatalf("parseQuotaCell(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseMaterialQuotaFile(t *testing.T) {
	csv := strings.Join([]string{
		"Loại VL,Định mức,Mô tả",
		"Mica trong 3 ly,20,Mica 3mm",
		"Gỗ 5 ly,,",  // blank quota + blank desc
		"Hỏng,abc,x", // invalid quota → parse error
	}, "\n")
	rows, perrs, err := ParseMaterialQuotaFile("CSV", strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 valid rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].Material != "Mica trong 3 ly" || rows[0].Quota == nil || *rows[0].Quota != 20 {
		t.Fatalf("row0 wrong: %+v", rows[0])
	}
	if rows[0].Description != "Mica 3mm" {
		t.Fatalf("row0 description not parsed: %+v", rows[0])
	}
	if rows[1].Quota != nil {
		t.Fatalf("blank quota should parse to nil, got %+v", rows[1])
	}
	if len(perrs) != 1 || perrs[0].ErrorCode != errQuotaInvalid {
		t.Fatalf("want 1 QUOTA_INVALID error, got %+v", perrs)
	}
}

// TestMaterialImport_PreviewCommit covers create (with quota + description),
// create-blank (unlimited), quota update, description-only update, blank cells
// never clearing an existing value, and quota conflict.
func TestMaterialImport_PreviewCommit(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	// Existing materials.
	if err := db.Create(&models.Material{Code: "MICA-TRONG-3-LY", Name: "Mica trong 3 ly", ProductsPerUnit: ptrInt(15)}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Create(&models.Material{Code: "KEEP", Name: "Giữ nguyên", ProductsPerUnit: ptrInt(30)}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Create(&models.Material{Code: "DESC-ONLY", Name: "Mô tả mới", ProductsPerUnit: ptrInt(8)}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	rows := []MaterialQuotaRow{
		{RowNumber: 1, Material: "Mica trong 3 ly", Quota: ptrInt(20)},                // update 15→20
		{RowNumber: 2, Material: "Gỗ 5 ly", Quota: ptrInt(12), Description: "Gỗ tốt"}, // create w/ quota + desc
		{RowNumber: 3, Material: "Acrylic", Quota: nil},                               // create, unlimited, no desc
		{RowNumber: 4, Material: "Giữ nguyên", Quota: nil},                            // blank → no change (keep 30)
		{RowNumber: 5, Material: "Mô tả mới", Quota: nil, Description: "Ghi chú"},     // desc-only update, quota untouched
		{RowNumber: 6, Material: "Xung đột", Quota: ptrInt(5)},                        // conflict pair ↓
		{RowNumber: 7, Material: "Xung đột", Quota: ptrInt(9)},
	}

	pv := svc.PreviewMaterialImport("quota.csv", rows, nil)
	if pv.Summary.NewMaterials != 2 {
		t.Fatalf("new = %d, want 2", pv.Summary.NewMaterials)
	}
	if pv.Summary.Updates != 2 {
		t.Fatalf("updates = %d, want 2 (Mica quota + Mô tả mới desc)", pv.Summary.Updates)
	}
	if pv.Summary.Unchanged != 1 {
		t.Fatalf("unchanged = %d, want 1", pv.Summary.Unchanged)
	}
	if pv.Summary.ErrorRows != 1 || pv.Errors[0].ErrorCode != errQuotaConflict {
		t.Fatalf("want 1 QUOTA_CONFLICT, got %+v", pv.Errors)
	}

	if _, err := svc.CommitMaterialImport(owner, rows); err != nil {
		t.Fatalf("commit: %v", err)
	}

	repo := repositories.New(db)
	mica, _ := repo.Material.FindByNameInsensitive("Mica trong 3 ly")
	if mica.ProductsPerUnit == nil || *mica.ProductsPerUnit != 20 {
		t.Fatalf("Mica quota should be 20, got %v", mica.ProductsPerUnit)
	}
	go5, _ := repo.Material.FindByNameInsensitive("Gỗ 5 ly")
	if go5 == nil || go5.ProductsPerUnit == nil || *go5.ProductsPerUnit != 12 || go5.Description != "Gỗ tốt" {
		t.Fatalf("Gỗ 5 ly should be created with quota 12 + desc, got %+v", go5)
	}
	acr, _ := repo.Material.FindByNameInsensitive("Acrylic")
	if acr == nil || acr.ProductsPerUnit != nil || acr.Description != "" {
		t.Fatalf("Acrylic should be created unlimited with no desc, got %+v", acr)
	}
	keep, _ := repo.Material.FindByNameInsensitive("Giữ nguyên")
	if keep.ProductsPerUnit == nil || *keep.ProductsPerUnit != 30 {
		t.Fatalf("blank quota must NOT clear existing 30, got %v", keep.ProductsPerUnit)
	}
	// Description-only update must set the description and leave the quota alone.
	descOnly, _ := repo.Material.FindByNameInsensitive("Mô tả mới")
	if descOnly.Description != "Ghi chú" {
		t.Fatalf("desc should be set to 'Ghi chú', got %q", descOnly.Description)
	}
	if descOnly.ProductsPerUnit == nil || *descOnly.ProductsPerUnit != 8 {
		t.Fatalf("blank quota on a desc-only update must NOT clear existing 8, got %v", descOnly.ProductsPerUnit)
	}
	// The conflicting material was skipped entirely.
	if conf, _ := repo.Material.FindByNameInsensitive("Xung đột"); conf != nil {
		t.Fatalf("conflicting material must not be created, got %+v", conf)
	}
}

// Non-OWNER must be refused at the service layer too (defense in depth).
func TestMaterialImport_NonOwnerForbidden(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	admin := Actor{ID: 2, Role: models.RoleAdmin}
	_, err := svc.CommitMaterialImport(admin, []MaterialQuotaRow{{RowNumber: 1, Material: "X", Quota: ptrInt(5)}})
	if err == nil {
		t.Fatalf("expected forbidden error for non-owner")
	}
}
