package services

import (
	"strconv"
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
	// Batches/batch items too: deleting a material checks whether production still
	// references it.
	if err := db.AutoMigrate(&models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Batch{}, &models.BatchItem{},
		// Orders too: deleting a SKU checks whether an order line still points at it.
		&models.Order{}, &models.OrderItem{}, &models.AuditLog{}); err != nil {
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
// never clearing an existing value, and two lines sharing a name but differing in
// quota (separate materials, not a conflict).
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
		{RowNumber: 6, Material: "Hai loại", Quota: ptrInt(5)},                        // same name, different quota ↓
		{RowNumber: 7, Material: "Hai loại", Quota: ptrInt(9)},
	}

	pv, err := svc.PreviewMaterialImport("quota.csv", rows, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if pv.Summary.NewMaterials != 4 {
		t.Fatalf("new = %d, want 4 (Gỗ, Acrylic + both 'Hai loại' variants)", pv.Summary.NewMaterials)
	}
	if pv.Summary.Updates != 2 {
		t.Fatalf("updates = %d, want 2 (Mica quota + Mô tả mới desc)", pv.Summary.Updates)
	}
	if pv.Summary.Unchanged != 1 {
		t.Fatalf("unchanged = %d, want 1", pv.Summary.Unchanged)
	}
	// Same Loại VL with a different Định mức is a different material, not an error.
	if pv.Summary.ErrorRows != 0 {
		t.Fatalf("want no errors, got %+v", pv.Errors)
	}
	if pv.Summary.NameVariants != 2 {
		t.Fatalf("name variants = %d, want 2 rows flagged as same-name-different-data", pv.Summary.NameVariants)
	}
	for _, it := range pv.Items {
		if it.Name == "Hai loại" && !it.NameVariant {
			t.Fatalf("'Hai loại' must be flagged as a name variant: %+v", it)
		}
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
	// Two lines sharing a name but not a quota become two materials, each with its
	// own code — the older behaviour dropped both on the floor as a conflict.
	pair, _ := repo.Material.ListByNameInsensitive("Hai loại")
	if len(pair) != 2 {
		t.Fatalf("same name + different quota must create 2 materials, got %d", len(pair))
	}
	if *pair[0].ProductsPerUnit != 5 || *pair[1].ProductsPerUnit != 9 {
		t.Fatalf("quotas = %v/%v, want 5 and 9", pair[0].ProductsPerUnit, pair[1].ProductsPerUnit)
	}
	if pair[0].Code == pair[1].Code {
		t.Fatalf("materials sharing a name must still get distinct codes, both %q", pair[0].Code)
	}
}

// TestMaterialImport_FoldsExactDuplicateRows locks in what "dòng trùng nhau" means
// here: only rows equal in ALL THREE columns are the same material and collapse to
// one; differ in any single column and they stay separate. Real files repeat the
// same material many times, and re-importing must not multiply the catalog.
func TestMaterialImport_FoldsExactDuplicateRows(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	rows := []MaterialQuotaRow{
		{RowNumber: 1, Material: "Mica trong 3 ly", Quota: ptrInt(20), Description: "Mica 3mm"},
		{RowNumber: 2, Material: "Mica trong 3 ly", Quota: ptrInt(20), Description: "Mica 3mm"},        // exact dup
		{RowNumber: 3, Material: "  mica TRONG 3 ly ", Quota: ptrInt(20), Description: "mica 3MM"},     // dup modulo case/spaces
		{RowNumber: 4, Material: "Mica trong 3 ly", Quota: ptrInt(20), Description: "Mica 3mm loại B"}, // desc differs → own material
		{RowNumber: 5, Material: "Mica trong 3 ly", Quota: ptrInt(30), Description: "Mica 3mm"},        // quota differs → own material
	}

	pv, err := svc.PreviewMaterialImport("dup.csv", rows, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(pv.Items) != 3 {
		t.Fatalf("items = %d, want 3 (one per distinct triple)", len(pv.Items))
	}
	if pv.Summary.DuplicateRows != 2 {
		t.Fatalf("duplicate rows = %d, want 2 folded away", pv.Summary.DuplicateRows)
	}
	// The folded rows keep their line numbers so the preview can point at them.
	if got := pv.Items[0].RowNumbers; len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("first item rows = %v, want rows 1,2,3 folded together", got)
	}
	if pv.Summary.ErrorRows != 0 {
		t.Fatalf("duplicates are not errors, got %+v", pv.Errors)
	}

	if _, err := svc.CommitMaterialImport(owner, rows); err != nil {
		t.Fatalf("commit: %v", err)
	}
	repo := repositories.New(db)
	mats, _ := repo.Material.ListByNameInsensitive("Mica trong 3 ly")
	if len(mats) != 3 {
		t.Fatalf("materials created = %d, want 3", len(mats))
	}

	// Re-importing the same file must be a no-op, not a second set of materials.
	pv2, err := svc.PreviewMaterialImport("dup.csv", rows, nil)
	if err != nil {
		t.Fatalf("re-preview: %v", err)
	}
	if pv2.Summary.NewMaterials != 0 || pv2.Summary.Updates != 0 || pv2.Summary.Unchanged != 3 {
		t.Fatalf("re-import should be all NOCHANGE, got %+v", pv2.Summary)
	}
	if _, err := svc.CommitMaterialImport(owner, rows); err != nil {
		t.Fatalf("re-commit: %v", err)
	}
	if again, _ := repo.Material.ListByNameInsensitive("Mica trong 3 ly"); len(again) != 3 {
		t.Fatalf("re-import must not duplicate the catalog, got %d materials", len(again))
	}
}

// TestMaterialImport_StaysOffTheRowByRowPath is the N+1 guard. The catalog lookup
// used to run once per line and the commit probed for a free code per new
// material: a 350-row file meant ~700 round-trips, which on a hosted database is
// minutes of waiting. Preview must read the catalog in one query and commit must
// insert in batches, no matter how many rows the file has.
func TestMaterialImport_StaysOffTheRowByRowPath(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	const materials = 300
	rows := make([]MaterialQuotaRow, 0, materials*2)
	for i := 0; i < materials; i++ {
		name := "NVL " + strconv.Itoa(i)
		rows = append(rows,
			MaterialQuotaRow{RowNumber: len(rows) + 1, Material: name, Quota: ptrInt(10 + i%5)},
			// Every material repeated once — the fold must not cost extra queries.
			MaterialQuotaRow{RowNumber: len(rows) + 2, Material: name, Quota: ptrInt(10 + i%5)},
		)
	}

	stmts := 0
	count := func(*gorm.DB) { stmts++ }
	for _, reg := range []func(string, func(*gorm.DB)) error{
		db.Callback().Query().After("gorm:query").Register,
		db.Callback().Create().After("gorm:create").Register,
		db.Callback().Update().After("gorm:update").Register,
		db.Callback().Row().After("gorm:row").Register,
		db.Callback().Raw().After("gorm:raw").Register,
	} {
		if err := reg("test:count", count); err != nil {
			t.Fatalf("register callback: %v", err)
		}
	}

	if _, err := svc.PreviewMaterialImport("big.csv", rows, nil); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if stmts > 2 {
		t.Fatalf("preview of %d rows issued %d statements, want ≤2 (one catalog read)", len(rows), stmts)
	}

	stmts = 0
	if _, err := svc.CommitMaterialImport(owner, rows); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// 1 catalog read + 1 code read + ⌈300/200⌉ inserts + the audit log.
	if stmts > 8 {
		t.Fatalf("commit of %d new materials issued %d statements, want a handful of batches", materials, stmts)
	}

	repo := repositories.New(db)
	if got, _ := repo.Material.AllCodes(); len(got) != materials {
		t.Fatalf("created %d materials, want %d", len(got), materials)
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
