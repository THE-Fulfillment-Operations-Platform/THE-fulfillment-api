package services

import (
	"bytes"
	"strings"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// pairQuotaFixture: two materials (mica with a default of 20 per sheet, wood with
// none) and SKUs mapped to them — two trays on mica, a combo on both, a board on
// wood only.
func pairQuotaFixture(t *testing.T) (*gorm.DB, *CatalogService, map[string]uint, map[string]uint) {
	t.Helper()
	db, svc, mica := newCatalogQuotaFixture(t) // material "MICA" / "Mica"
	twenty := 20
	if err := db.Model(mica).Updates(map[string]any{"name": "Mica trong 3 ly", "products_per_unit": twenty}).Error; err != nil {
		t.Fatalf("seed mica: %v", err)
	}
	wood := &models.Material{Code: "GO-5-LY", Name: "Gỗ 5 ly"}
	if err := db.Create(wood).Error; err != nil {
		t.Fatalf("seed wood: %v", err)
	}
	mats := map[string]uint{"mica": mica.ID, "wood": wood.ID}
	skus := map[string]uint{}
	for code, matIDs := range map[string][]uint{
		"HOP-BE": {mica.ID}, "HOP-LON": {mica.ID}, "COMBO-A": {mica.ID, wood.ID}, "BANG-GO": {wood.ID},
	} {
		sku := &models.SKU{Code: code, Name: code, ProductName: code, IsActive: true}
		if err := db.Create(sku).Error; err != nil {
			t.Fatalf("seed sku %s: %v", code, err)
		}
		for _, m := range matIDs {
			if err := db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: m, QuantityPerUnit: 1}).Error; err != nil {
				t.Fatalf("seed mapping: %v", err)
			}
		}
		skus[code] = sku.ID
	}
	return db, svc, skus, mats
}

func quotaPtr(n int) *int { return &n }

// The file is matched pair by pair: SKU by code (normalised), material among that
// SKU's OWN materials by name or code. Every way a row can be wrong is reported
// on that row instead of guessed, and a blank quota just skips the row.
func TestPairQuotaImport_Preview(t *testing.T) {
	_, svc, _, _ := pairQuotaFixture(t)
	rows := []PairQuotaFileRow{
		{RowNumber: 1, SKU: "HOP-BE", Material: "Mica trong 3 ly", Quota: quotaPtr(12)},
		{RowNumber: 2, SKU: "hop lon", Material: "MICA TRONG 3 LY", Quota: quotaPtr(4)}, // normalised both sides
		{RowNumber: 3, SKU: "COMBO-A", Material: "Gỗ 5 ly", Quota: nil},                 // blank → skipped
		{RowNumber: 4, SKU: "BANG-GO", Material: "Mica trong 3 ly", Quota: quotaPtr(5)}, // wood-only SKU
		{RowNumber: 5, SKU: "NOPE", Material: "Mica trong 3 ly", Quota: quotaPtr(3)},
		{RowNumber: 6, SKU: "HOP-BE", Material: "Mica trong 3 ly", Quota: quotaPtr(12)}, // exact repeat
		{RowNumber: 7, SKU: "COMBO-A", Material: "MICA", Quota: quotaPtr(8)},            // by material code
		{RowNumber: 8, SKU: "COMBO-A", Material: "Mica trong 3 ly", Quota: quotaPtr(9)}, // same pair, other number
		{RowNumber: 9, SKU: "BANG-GO", Material: "Gỗ 5 ly", Quota: quotaPtr(6)},
	}
	pv, err := svc.PreviewPairQuotaImport("f.xlsx", rows, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	got := map[string]PairQuotaItem{}
	for _, it := range pv.Items {
		got[it.SKUCode+"|"+it.MaterialCode] = it
	}
	if len(got) != 3 {
		t.Fatalf("items = %+v, want HOP-BE, HOP-LON, BANG-GO", pv.Items)
	}
	if it := got["HOP-BE|MICA"]; it.Quota != 12 || it.Action != matActionUpdate || len(it.RowNumbers) != 2 {
		t.Errorf("HOP-BE: %+v, want update to 12 from rows 1 and 6", it)
	}
	if it := got["HOP-BE|MICA"]; it.MaterialQuota == nil || *it.MaterialQuota != 20 || it.CurrentQuota != nil {
		t.Errorf("HOP-BE must show the material default (20) and no pair quota yet: %+v", it)
	}
	if it := got["HOP-LON|MICA"]; it.Quota != 4 {
		t.Errorf("HOP-LON: %+v, want 4", it)
	}
	if it := got["BANG-GO|GO-5-LY"]; it.Quota != 6 {
		t.Errorf("BANG-GO: %+v, want 6", it)
	}

	codes := map[int]string{}
	for _, e := range pv.Errors {
		codes[e.RowNumber] = e.ErrorCode
	}
	want := map[int]string{4: errPairNotMapped, 5: errPairSKUNotFound, 7: errPairConflict}
	for row, code := range want {
		if codes[row] != code {
			t.Errorf("row %d: got %q, want %q (errors %+v)", row, codes[row], code, pv.Errors)
		}
	}
	for _, e := range pv.Errors {
		if e.ErrorCode == errPairConflict && (len(e.RowNumbers) != 2 || !strings.Contains(e.Message, "8, 9")) {
			t.Errorf("conflict must name both rows and both numbers: %+v", e)
		}
	}
	s := pv.Summary
	if s.Updates != 3 || s.BlankRows != 1 || s.DuplicateRows != 1 || s.ErrorRows != 3 {
		t.Errorf("summary = %+v, want 3 updates, 1 blank, 1 duplicate, 3 errors", s)
	}
}

// Commit writes only the pair's own quota — the material default and the
// bill-of-materials figure stay — and the batch split then uses it.
func TestPairQuotaImport_CommitSetsPairQuota(t *testing.T) {
	db, svc, skus, mats := pairQuotaFixture(t)
	rows := []PairQuotaFileRow{
		{RowNumber: 1, SKU: "HOP-BE", Material: "Mica trong 3 ly", Quota: quotaPtr(12)},
		{RowNumber: 2, SKU: "HOP-LON", Material: "Mica trong 3 ly", Quota: quotaPtr(4)},
		{RowNumber: 3, SKU: "NOPE", Material: "Mica trong 3 ly", Quota: quotaPtr(3)},
	}

	_, err := svc.CommitPairQuotaImport(Actor{ID: 2, Role: models.RoleAdmin}, rows)
	if ae, ok := apperr.As(err); !ok || ae.Code != "FORBIDDEN" {
		t.Fatalf("only OWNER sets quotas, got %v", err)
	}

	pv, err := svc.CommitPairQuotaImport(Actor{ID: 1, Role: models.RoleOwner}, rows)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if pv.Applied == nil || pv.Applied.Updated != 2 {
		t.Fatalf("applied = %+v, want 2", pv.Applied)
	}
	if q := pairQuota(t, db, skus["HOP-BE"], mats["mica"]); q == nil || *q != 12 {
		t.Errorf("HOP-BE quota = %v, want 12", q)
	}
	if q := pairQuota(t, db, skus["HOP-LON"], mats["mica"]); q == nil || *q != 4 {
		t.Errorf("HOP-LON quota = %v, want 4", q)
	}
	var mica models.Material
	db.First(&mica, mats["mica"])
	if mica.ProductsPerUnit == nil || *mica.ProductsPerUnit != 20 {
		t.Errorf("material default must stay 20, got %v", mica.ProductsPerUnit)
	}

	// The split reads the pair quota: 4 large trays fill a sheet.
	var lon models.SKU
	if err := db.Preload("Materials").First(&lon, skus["HOP-LON"]).Error; err != nil {
		t.Fatalf("load sku: %v", err)
	}
	if got := resolveProductionQuota(&lon, &mica); got != 4 {
		t.Errorf("batch split quota for HOP-LON = %d, want 4", got)
	}

	// Committing the same file again changes nothing.
	again, err := svc.CommitPairQuotaImport(Actor{ID: 1, Role: models.RoleOwner}, rows)
	if err != nil || again.Applied.Updated != 0 || again.Summary.Unchanged != 2 {
		t.Fatalf("re-commit: applied %+v unchanged %d err %v, want 0 / 2", again.Applied, again.Summary.Unchanged, err)
	}
}

// Export → fill → import: the exported sheet lists every pair (blank where there
// is no quota), and reading it straight back sets nothing and flags nothing.
func TestPairQuotaExport_RoundTrips(t *testing.T) {
	db, svc, skus, mats := pairQuotaFixture(t)
	if err := db.Model(&models.SKUMaterial{}).Where("sku_id = ? AND material_id = ?", skus["HOP-BE"], mats["mica"]).
		Update("products_per_unit", 12).Error; err != nil {
		t.Fatalf("seed pair quota: %v", err)
	}

	data, _, err := svc.ExportPairQuotasXLSX()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	rows, perrs, err := ParsePairQuotaFile("XLSX", bytes.NewReader(data))
	if err != nil || len(perrs) != 0 {
		t.Fatalf("parse export: %v %+v", err, perrs)
	}
	if len(rows) != 5 { // HOP-BE, HOP-LON, COMBO-A ×2, BANG-GO
		t.Fatalf("export rows = %d, want 5 pairs", len(rows))
	}
	filled := 0
	for _, r := range rows {
		if r.Quota != nil {
			filled++
			if r.SKU != "HOP-BE" || *r.Quota != 12 {
				t.Errorf("only HOP-BE carries a pair quota (12), got %+v", r)
			}
		}
		if r.MaterialCode == "" {
			t.Errorf("export must carry the material code: %+v", r)
		}
	}
	if filled != 1 {
		t.Errorf("filled rows = %d, want 1 (the rest are blank to fill in)", filled)
	}

	pv, err := svc.PreviewPairQuotaImport("export.xlsx", rows, perrs)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if pv.Summary.Updates != 0 || len(pv.Errors) != 0 || pv.Summary.Unchanged != 1 || pv.Summary.BlankRows != 4 {
		t.Errorf("round-trip summary = %+v errors %+v, want 1 unchanged, 4 blank, nothing else", pv.Summary, pv.Errors)
	}
}

// A quota that is not a positive number is refused on its row — "0" must not
// quietly read as "leave it".
func TestParsePairQuotaFile_RejectsBadQuota(t *testing.T) {
	csv := "SKU,Loại VL,Định mức\nHOP-BE,Mica,abc\nHOP-LON,Mica,0\nCOMBO-A,Mica,\nBANG-GO,Gỗ,7\n"
	rows, errs, err := ParsePairQuotaFile("CSV", strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	codes := map[int]string{}
	for _, e := range errs {
		codes[e.RowNumber] = e.ErrorCode
	}
	if codes[1] != errQuotaInvalid || codes[2] != errPairQuotaNotPos {
		t.Errorf("errors = %+v, want row 1 invalid, row 2 not positive", errs)
	}
	if len(rows) != 2 || rows[0].Quota != nil || rows[1].Quota == nil || *rows[1].Quota != 7 {
		t.Errorf("rows = %+v, want the blank row (skipped later) and the 7", rows)
	}

	if _, _, err := ParsePairQuotaFile("CSV", strings.NewReader("SKU,Định mức\nA,1\n")); err == nil {
		t.Error("a file without Loại VL must be refused as a whole")
	}
}
