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

// newMasterDB spins up an in-memory sqlite with just the tables the master-data
// import touches.
func newMasterDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.MasterImportJob{}, &models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func masterSvc(db *gorm.DB) *MasterImportService {
	repo := repositories.New(db)
	return &MasterImportService{repo: repo, audit: &AuditService{repo: repo}}
}

func legacyRows(pairs ...[2]string) []LegacyRow {
	rows := make([]LegacyRow, len(pairs))
	for i, p := range pairs {
		rows[i] = LegacyRow{RowNumber: i + 1, SKU: p[0], Material: p[1]}
	}
	return rows
}

func skuPlanByCode(t *testing.T, pv *MasterImportPreview, code string) SKUPlan {
	t.Helper()
	for _, sp := range pv.SKUs {
		if sp.Code == code {
			return sp
		}
	}
	t.Fatalf("no SKU plan for code %q (have %+v)", code, pv.SKUs)
	return SKUPlan{}
}

// ---- splitMaterials / rowSignature units --------------------------------------

func TestSplitMaterials(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Mica trong 3 ly", []string{"Mica trong 3 ly"}},
		{"Mica trong 3 ly + Basswood 5mm + Mica Hologram",
			[]string{"Mica trong 3 ly", "Basswood 5mm", "Mica Hologram"}},
		{"Gỗ 5 ly\nAcrylic", []string{"Gỗ 5 ly", "Acrylic"}}, // embedded newline
		{"Mica + + Hologram", []string{"Mica", "Hologram"}},  // empty middle dropped
		{"  Mica  +  mica  ", []string{"Mica"}},              // case-insensitive dedupe within cell
		{"   ", nil},                                         // blank
		{"", nil},
	}
	for _, c := range cases {
		got := splitMaterials(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitMaterials(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitMaterials(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}

	// Signature is order- and case-insensitive.
	if rowSignature([]string{"A", "B"}) != rowSignature([]string{"b", "a"}) {
		t.Fatalf("rowSignature should be order/case-insensitive")
	}
}

// ---- combo preview + commit ---------------------------------------------------

// TestMasterImport_Combo covers the whole point of the feature: a "+"-joined cell
// becomes a combo SKU mapped to all its materials, single cells stay single, an
// empty cell is "missing", and a re-import is additive.
func TestMasterImport_Combo(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	actor := Actor{ID: 1}

	rows := legacyRows(
		[2]string{"BR A 1.6 Gai", "Mica trong 3 ly"},                 // single, repeated
		[2]string{"BR A 1.6 Gai", "Mica trong 3 ly"},                 // dup → still single
		[2]string{"BR A 2 Gai", "Mica trong 3 ly + Mica Hologram"},   // combo via +
		[2]string{"BR SH 2", "Mica start Hologram\nGỗ 5 ly 3 layer"}, // combo via newline
		[2]string{"NO MAT", ""},                                      // missing material
	)

	pv, err := svc.Preview(actor, "CSV", "legacy.csv", rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	// Distinct materials: Mica trong 3 ly, Mica Hologram, Mica start Hologram, Gỗ 5 ly 3 layer = 4.
	if pv.Summary.NewMaterials != 4 {
		t.Fatalf("new materials = %d, want 4", pv.Summary.NewMaterials)
	}
	if pv.Summary.NewSKUs != 4 {
		t.Fatalf("new SKUs = %d, want 4", pv.Summary.NewSKUs)
	}
	// Mappings: single(1) + combo(2) + combo(2) = 5. Missing SKU maps nothing.
	if pv.Summary.NewMappings != 5 {
		t.Fatalf("new mappings = %d, want 5", pv.Summary.NewMappings)
	}
	if pv.Summary.MissingCount != 1 {
		t.Fatalf("missing = %d, want 1", pv.Summary.MissingCount)
	}
	if pv.Summary.ErrorRows != 0 {
		t.Fatalf("errors = %+v, want none (every SKU's rows agree)", pv.Errors)
	}

	single := skuPlanByCode(t, pv, normalizeSKUCode("BR A 1.6 Gai"))
	if single.IsCombo || single.Status != skuStatusOK || len(single.MaterialNames) != 1 {
		t.Fatalf("single SKU wrong: %+v", single)
	}
	if single.RowCount != 2 {
		t.Fatalf("single SKU row_count = %d, want 2", single.RowCount)
	}

	combo := skuPlanByCode(t, pv, normalizeSKUCode("BR A 2 Gai"))
	if !combo.IsCombo || combo.Status != skuStatusOK || len(combo.MaterialNames) != 2 {
		t.Fatalf("combo SKU wrong: %+v", combo)
	}

	missing := skuPlanByCode(t, pv, normalizeSKUCode("NO MAT"))
	if missing.Status != skuStatusMissing {
		t.Fatalf("missing SKU status = %q, want %q", missing.Status, skuStatusMissing)
	}

	// ---- Commit ----
	res, err := svc.Commit(actor, pv.ImportJobID)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Applied == nil || res.Applied.MaterialsCreated != 4 || res.Applied.MappingsCreated != 5 {
		t.Fatalf("applied = %+v, want 4 materials / 5 mappings", res.Applied)
	}

	repo := repositories.New(db)
	comboRec, err := repo.SKU.FindByCode(normalizeSKUCode("BR A 2 Gai"))
	if err != nil {
		t.Fatalf("find combo sku: %v", err)
	}
	if !comboRec.IsCombo {
		t.Fatalf("combo SKU should have IsCombo=true after commit")
	}
	if len(comboRec.Materials) != 2 {
		t.Fatalf("combo SKU should map 2 materials, got %d", len(comboRec.Materials))
	}

	singleRec, _ := repo.SKU.FindByCode(normalizeSKUCode("BR A 1.6 Gai"))
	if singleRec.IsCombo {
		t.Fatalf("single-material SKU must not be flagged combo")
	}

	// ---- Re-import is additive: adding a material turns a single SKU into a combo,
	//      and the already-present mapping is not duplicated. ----
	pv2, err := svc.Preview(actor, "CSV", "legacy2.csv",
		legacyRows([2]string{"BR A 1.6 Gai", "Mica trong 3 ly + Kim loại"}))
	if err != nil {
		t.Fatalf("preview 2: %v", err)
	}
	// Only "Kim loại" is new; the SKU and the Mica mapping already exist.
	if pv2.Summary.NewMaterials != 1 {
		t.Fatalf("re-import new materials = %d, want 1", pv2.Summary.NewMaterials)
	}
	if pv2.Summary.NewMappings != 1 {
		t.Fatalf("re-import new mappings = %d, want 1 (Mica already mapped)", pv2.Summary.NewMappings)
	}
	if _, err := svc.Commit(actor, pv2.ImportJobID); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	singleRec, _ = repo.SKU.FindByCode(normalizeSKUCode("BR A 1.6 Gai"))
	if !singleRec.IsCombo {
		t.Fatalf("additive import should have upgraded SKU to combo")
	}
	if len(singleRec.Materials) != 2 {
		t.Fatalf("SKU should now map 2 materials, got %d", len(singleRec.Materials))
	}
}

// TestMasterImport_ProductName covers the "Tên sản phẩm" column: the readable
// product name is captured per SKU (distinct list, first is representative), stored
// on the SKU at commit (not the code), refreshed on re-import, and falls back to the
// display name when the file has no product-name column.
func TestMasterImport_ProductName(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	actor := Actor{ID: 1}

	rows := []LegacyRow{
		{RowNumber: 1, SKU: "BR A 1.6 Gai", Material: "Mica trong 3 ly", ProductName: "Kệ Gỗ Treo Tường"},
		{RowNumber: 2, SKU: "BR A 1.6 Gai", Material: "Mica trong 3 ly", ProductName: "Giá Sách Gỗ Mini"}, // same SKU, another product
		{RowNumber: 3, SKU: "BR A 1.6 Gai", Material: "Mica trong 3 ly", ProductName: "Kệ Gỗ Treo Tường"}, // dup name → collapsed
		{RowNumber: 4, SKU: "LWD 12in", Material: "Gỗ 5 ly", ProductName: "Thớt Gỗ Khắc Tên"},
		{RowNumber: 5, SKU: "NO PN", Material: "Mica trong 3 ly"}, // no product name → fallback to display name
	}

	pv, err := svc.Preview(actor, "CSV", "pn.csv", rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	multi := skuPlanByCode(t, pv, normalizeSKUCode("BR A 1.6 Gai"))
	if multi.ProductName != "Kệ Gỗ Treo Tường" {
		t.Fatalf("representative product name = %q, want first seen", multi.ProductName)
	}
	if len(multi.ProductNames) != 2 {
		t.Fatalf("distinct product names = %v, want 2 (dup collapsed)", multi.ProductNames)
	}

	if _, err := svc.Commit(actor, pv.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	repo := repositories.New(db)

	rec, _ := repo.SKU.FindByCode(normalizeSKUCode("BR A 1.6 Gai"))
	if rec.ProductName != "Kệ Gỗ Treo Tường" {
		t.Fatalf("stored product name = %q, want the real name (not the code)", rec.ProductName)
	}
	if rec.ProductName == rec.Name && rec.Name == rec.Code {
		t.Fatalf("product name must not duplicate the SKU code/name")
	}

	// No product-name column for this SKU → fall back to the display name.
	noPN, _ := repo.SKU.FindByCode(normalizeSKUCode("NO PN"))
	if noPN.ProductName != noPN.Name {
		t.Fatalf("SKU without product name should fall back to name, got %q", noPN.ProductName)
	}

	// Re-import with a different product name refreshes the existing SKU.
	pv2, err := svc.Preview(actor, "CSV", "pn2.csv", []LegacyRow{
		{RowNumber: 1, SKU: "BR A 1.6 Gai", Material: "Mica trong 3 ly", ProductName: "Đồng Hồ Gỗ"},
	})
	if err != nil {
		t.Fatalf("preview 2: %v", err)
	}
	if _, err := svc.Commit(actor, pv2.ImportJobID); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	rec, _ = repo.SKU.FindByCode(normalizeSKUCode("BR A 1.6 Gai"))
	if rec.ProductName != "Đồng Hồ Gỗ" {
		t.Fatalf("re-import should refresh product name, got %q", rec.ProductName)
	}
}

// TestMasterImport_InconsistentRowsAreRefused: a SKU whose rows disagree on its
// material set is dropped with MATERIAL_CONFLICT — not mapped to the union. A
// product mapped to a material it is not made of would land in that material's
// batch and be cut from the wrong sheet, so the file has to be fixed first. The
// rest of the file still imports, and a material only the refused SKU used is
// not created.
func TestMasterImport_InconsistentRowsAreRefused(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	actor := Actor{ID: 1}

	rows := legacyRows(
		[2]string{"BR A 1.6 kep", "Mica trong 3 ly + Basswood 5mm + Mica Hologram"},
		[2]string{"BR A 1.6 kep", "Mica trong 3 ly"}, // partial → disagrees
		[2]string{"BR A 2 kep", "Mica trong 3 ly"},
	)

	pv, err := svc.Preview(actor, "CSV", "inc.csv", rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(pv.Errors) != 1 || pv.Errors[0].ErrorCode != errMaterialConflict || pv.Errors[0].RowNumber != 1 {
		t.Fatalf("errors = %+v, want one MATERIAL_CONFLICT against row 1", pv.Errors)
	}
	if !strings.Contains(pv.Errors[0].Message, "Mica trong 3 ly + Basswood 5mm + Mica Hologram") || !strings.Contains(pv.Errors[0].Message, " / ") {
		t.Fatalf("the message must show the sets that disagree, got %q", pv.Errors[0].Message)
	}
	if len(pv.SKUs) != 1 || pv.SKUs[0].Code != normalizeSKUCode("BR A 2 kep") {
		t.Fatalf("plan = %+v, want only the consistent SKU", pv.SKUs)
	}
	for _, m := range pv.Materials {
		if m.Name != "Mica trong 3 ly" {
			t.Fatalf("a material only the refused SKU uses must not be planned: %+v", pv.Materials)
		}
	}
	if pv.Summary.NewSKUs != 1 || pv.Summary.NewMappings != 1 || pv.Summary.ErrorRows != 1 {
		t.Fatalf("summary = %+v", pv.Summary)
	}

	if _, err := svc.Commit(actor, pv.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	repo := repositories.New(db)
	if _, err := repo.SKU.FindByCode(normalizeSKUCode("BR A 1.6 kep")); err == nil {
		t.Fatalf("the refused SKU must not be created")
	}
	if mats, _ := repo.Material.ListByNameInsensitive("Basswood 5mm"); len(mats) != 0 {
		t.Fatalf("Basswood 5mm belonged only to the refused SKU, got %d", len(mats))
	}
}

// TestMasterImport_StaysOffTheRowByRowPath is the N+1 guard for the legacy
// master-data import. The plan used to look every SKU up one at a time — and
// FindByCode preloads materials, so that was three statements per SKU — while the
// commit added a lookup + insert per material, per SKU and per mapping. A 200-row
// file meant ~600 statements on preview and well over a thousand on commit, which
// against a hosted database is the minute of waiting this test exists to prevent.
func TestMasterImport_StaysOffTheRowByRowPath(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	actor := Actor{ID: 1, Role: models.RoleOwner}

	const skus = 200
	rows := make([]LegacyRow, 0, skus)
	for i := 0; i < skus; i++ {
		rows = append(rows, LegacyRow{
			RowNumber: i + 1,
			SKU:       "SKU-" + strconv.Itoa(i),
			// A handful of shared materials, like a real file.
			Material:    "Mica " + strconv.Itoa(i%5) + " ly",
			ProductName: "Sản phẩm " + strconv.Itoa(i%3),
		})
	}

	stmts := 0
	count := func(*gorm.DB) { stmts++ }
	for _, reg := range []func(string, func(*gorm.DB)) error{
		db.Callback().Query().After("gorm:query").Register,
		db.Callback().Create().After("gorm:create").Register,
		db.Callback().Update().After("gorm:update").Register,
		db.Callback().Delete().After("gorm:delete").Register,
		db.Callback().Row().After("gorm:row").Register,
		db.Callback().Raw().After("gorm:raw").Register,
	} {
		if err := reg("test:count", count); err != nil {
			t.Fatalf("register callback: %v", err)
		}
	}

	pv, err := svc.Preview(actor, "XLSX", "legacy.xlsx", rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if pv.Summary.NewSKUs != skus || pv.Summary.NewMaterials != 5 {
		t.Fatalf("plan = %d SKUs / %d materials, want %d/5", pv.Summary.NewSKUs, pv.Summary.NewMaterials, skus)
	}
	// 3 catalog reads + the job insert + the audit insert.
	if stmts > 6 {
		t.Fatalf("preview of %d rows issued %d statements, want a fixed handful", len(rows), stmts)
	}

	stmts = 0
	res, err := svc.Commit(actor, pv.ImportJobID)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Applied == nil || res.Applied.SKUsCreated != skus || res.Applied.MaterialsCreated != 5 {
		t.Fatalf("applied = %+v, want %d SKUs / 5 materials", res.Applied, skus)
	}
	if res.Applied.MappingsCreated != skus {
		t.Fatalf("mappings created = %d, want %d (one per SKU)", res.Applied.MappingsCreated, skus)
	}
	// Reads + batched inserts (200 per batch) + job/audit writes — an order of
	// magnitude below one statement per row, let alone the seven it used to take.
	if stmts > 15 {
		t.Fatalf("commit of %d SKUs issued %d statements, want batched writes", skus, stmts)
	}

	// Re-importing the same file must be a no-op, not a second catalog.
	pv2, err := svc.Preview(actor, "XLSX", "legacy.xlsx", rows)
	if err != nil {
		t.Fatalf("re-preview: %v", err)
	}
	if pv2.Summary.NewSKUs != 0 || pv2.Summary.NewMaterials != 0 || pv2.Summary.NewMappings != 0 {
		t.Fatalf("re-preview should find nothing new, got %+v", pv2.Summary)
	}
	res2, err := svc.Commit(actor, pv2.ImportJobID)
	if err != nil {
		t.Fatalf("re-commit: %v", err)
	}
	if res2.Applied.SKUsCreated != 0 || res2.Applied.MaterialsCreated != 0 || res2.Applied.MappingsCreated != 0 {
		t.Fatalf("re-commit must create nothing, got %+v", res2.Applied)
	}
	var skuCount, matCount, mapCount int64
	db.Model(&models.SKU{}).Count(&skuCount)
	db.Model(&models.Material{}).Count(&matCount)
	db.Model(&models.SKUMaterial{}).Count(&mapCount)
	if skuCount != skus || matCount != 5 || mapCount != skus {
		t.Fatalf("catalog after re-import = %d SKUs / %d materials / %d mappings, want %d/5/%d",
			skuCount, matCount, mapCount, skus, skus)
	}
}
