package services

import (
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
		{"Mica + + Hologram", []string{"Mica", "Hologram"}},   // empty middle dropped
		{"  Mica  +  mica  ", []string{"Mica"}},               // case-insensitive dedupe within cell
		{"   ", nil},                                          // blank
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
		[2]string{"BR A 1.6 Gai", "Mica trong 3 ly"},        // single, repeated
		[2]string{"BR A 1.6 Gai", "Mica trong 3 ly"},        // dup → still single
		[2]string{"BR A 2 Gai", "Mica trong 3 ly + Mica Hologram"}, // combo via +
		[2]string{"BR SH 2", "Mica start Hologram\nGỗ 5 ly 3 layer"}, // combo via newline
		[2]string{"NO MAT", ""},                             // missing material
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
	if pv.Summary.ReviewCount != 0 {
		t.Fatalf("review = %d, want 0 (no inconsistent rows)", pv.Summary.ReviewCount)
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

// TestMasterImport_InconsistentRowsFlaggedButMapped: when the same SKU's rows give
// different material sets, the plan flags NEEDS_REVIEW yet still maps the union so
// nothing is silently dropped.
func TestMasterImport_InconsistentRowsFlaggedButMapped(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	actor := Actor{ID: 1}

	rows := legacyRows(
		[2]string{"BR A 1.6 kep", "Mica trong 3 ly + Basswood 5mm + Mica Hologram"},
		[2]string{"BR A 1.6 kep", "Mica trong 3 ly"}, // partial → disagrees
	)

	pv, err := svc.Preview(actor, "CSV", "inc.csv", rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	sp := skuPlanByCode(t, pv, normalizeSKUCode("BR A 1.6 kep"))
	if sp.Status != skuStatusReview {
		t.Fatalf("inconsistent SKU status = %q, want %q", sp.Status, skuStatusReview)
	}
	if len(sp.MaterialNames) != 3 {
		t.Fatalf("union should be 3 materials, got %v", sp.MaterialNames)
	}
	if pv.Summary.ReviewCount != 1 {
		t.Fatalf("review count = %d, want 1", pv.Summary.ReviewCount)
	}
	if pv.Summary.NewMappings != 3 {
		t.Fatalf("union still mapped: new mappings = %d, want 3", pv.Summary.NewMappings)
	}

	if _, err := svc.Commit(actor, pv.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	repo := repositories.New(db)
	rec, _ := repo.SKU.FindByCode(normalizeSKUCode("BR A 1.6 kep"))
	if len(rec.Materials) != 3 || !rec.IsCombo {
		t.Fatalf("inconsistent SKU should still map all 3 as combo, got %d combo=%v", len(rec.Materials), rec.IsCombo)
	}
}
