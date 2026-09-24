package services

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"the-fulfillment/backend/internal/models"
)

// Định mức khai tay theo cặp (SKU, NVL) — khách chốt 24/09/2026: số từ file
// layout thật của xưởng, thắng ước tính theo kích thước. Cột "Định mức" của
// file SKU đặt số đó cho cặp trên cùng dòng; ô trống giữ số đang có.

func TestParseLegacyFile_QuotaColumn(t *testing.T) {
	csv := "SKU,Loại VL,D (mm),R (mm),Định mức (sp/tấm),Mô tả\nA,Mica,10,10,40,\nB,Mica,,,,\n"
	rows, err := ParseLegacyFile("CSV", strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Quota != "40" || rows[1].Quota != "" {
		t.Fatalf("quota column not picked up: %+v", rows)
	}
	for _, h := range []string{"Định mức", "SP/tấm", "định mức sản xuất", "Sản phẩm / tấm", "quota"} {
		if !skuQuotaHeaders[quotaHeaderKey(h)] {
			t.Errorf("header %q phải được nhận là cột định mức (key %q)", h, quotaHeaderKey(h))
		}
	}
	for raw, want := range map[string]int{"40": 40, "40 sp": 40, "40/tấm": 40, "": 0} {
		if got, msg := parseQuota(raw); got != want || msg != "" {
			t.Errorf("parseQuota(%q) = %d, %q; want %d", raw, got, msg, want)
		}
	}
	for _, raw := range []string{"0", "-3", "40,5", "nhiều", "4in"} {
		if _, msg := parseQuota(raw); msg == "" {
			t.Errorf("parseQuota(%q) phải báo lỗi", raw)
		}
	}
}

// TestMasterImport_DeclaredQuota: the file's Định mức lands on the pair (new or
// existing), a blank cell keeps the stored number, a bad cell or two rows that
// disagree drop that SKU with a reason, and the preview says which quotas are
// declared and which are still estimates.
func TestMasterImport_DeclaredQuota(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	actor := Actor{ID: 1, Role: models.RoleOwner}
	mica := &models.Material{Code: "MICA", Name: "Mica", LengthMM: ptrFloat(100), WidthMM: ptrFloat(100)}
	if err := db.Create(mica).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	ten := 10.0
	existing := &models.SKU{Code: "CU", Name: "CU", LengthMM: &ten, WidthMM: &ten}
	if err := db.Create(existing).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}
	seven := 7
	if err := db.Create(&models.SKUMaterial{SKUID: existing.ID, MaterialID: mica.ID, QuantityPerUnit: 1, ProductsPerUnit: &seven}).Error; err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	rows := []LegacyRow{
		{RowNumber: 1, SKU: "MOI", Material: "Mica", Length: "10", Width: "10", Quota: "40"}, // new pair, declared 40 (estimate would be 100)
		{RowNumber: 2, SKU: "CU", Material: "Mica"},                                          // blank → keeps 7
		{RowNumber: 3, SKU: "UOC", Material: "Mica", Length: "20", Width: "20"},              // nothing declared → estimate 25
		{RowNumber: 4, SKU: "SAI", Material: "Mica", Quota: "nhiều"},                         // bad cell
		{RowNumber: 5, SKU: "CAI", Material: "Mica", Quota: "5"},
		{RowNumber: 6, SKU: "CAI", Material: "Mica", Quota: "6"}, // rows disagree
		{RowNumber: 7, SKU: "KHONG-VL", Quota: "9"},              // quota without a material
		{RowNumber: 8, SKU: "DOI", Material: "Mica", Quota: "3"}, // second run below re-declares
	}
	pv, err := svc.Preview(actor, "TEST", "quota.csv", rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	codes := errorCodes(pv)
	if codes["SAI"] != errQuotaInvalid || codes["CAI"] != errQuotaConflict || codes["KHONG-VL"] != errQuotaInvalid {
		t.Fatalf("error codes: %+v", pv.Errors)
	}
	plans := map[string]SKUPlan{}
	for _, sp := range pv.SKUs {
		plans[sp.Code] = sp
	}
	if q := plans["MOI"].QuotaByMaterial["Mica"]; q != 40 || plans["MOI"].QuotaSources["Mica"] != "declared" {
		t.Fatalf("MOI: want 40 declared, got %d %q", q, plans["MOI"].QuotaSources["Mica"])
	}
	if q := plans["CU"].QuotaByMaterial["Mica"]; q != 7 || plans["CU"].QuotaSources["Mica"] != "declared" {
		t.Fatalf("CU: blank cell keeps the stored 7, got %d %q", q, plans["CU"].QuotaSources["Mica"])
	}
	if q := plans["UOC"].QuotaByMaterial["Mica"]; q != 25 || plans["UOC"].QuotaSources["Mica"] != "estimated" {
		t.Fatalf("UOC: want estimate 25 (100×100 / 20×20), got %d %q", q, plans["UOC"].QuotaSources["Mica"])
	}
	if pv.Summary.QuotasSet != 2 {
		t.Fatalf("summary quotas_set = %d, want 2 (MOI, DOI)", pv.Summary.QuotasSet)
	}
	res, err := svc.Commit(actor, pv.ImportJobID)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Applied == nil || res.Applied.QuotasSet != 2 {
		t.Fatalf("applied quotas_set: %+v", res.Applied)
	}
	quotaOf := func(code string) int {
		var sku models.SKU
		if err := db.Where("code = ?", code).First(&sku).Error; err != nil {
			t.Fatalf("find %s: %v", code, err)
		}
		var sm models.SKUMaterial
		if err := db.Where("sku_id = ? AND material_id = ?", sku.ID, mica.ID).First(&sm).Error; err != nil {
			t.Fatalf("mapping %s: %v", code, err)
		}
		if sm.ProductsPerUnit == nil {
			return 0
		}
		return *sm.ProductsPerUnit
	}
	if quotaOf("MOI") != 40 || quotaOf("CU") != 7 || quotaOf("UOC") != 0 || quotaOf("DOI") != 3 {
		t.Fatalf("stored quotas: MOI=%d CU=%d UOC=%d DOI=%d", quotaOf("MOI"), quotaOf("CU"), quotaOf("UOC"), quotaOf("DOI"))
	}

	// Second file re-declares an existing pair: the stored number changes, in one
	// UPDATE for the whole file (SetPairQuotas), and shows as declared.
	pv2, err := svc.Preview(actor, "TEST", "quota2.csv", []LegacyRow{{RowNumber: 1, SKU: "DOI", Material: "Mica", Quota: "30"}})
	if err != nil {
		t.Fatalf("preview 2: %v", err)
	}
	if _, err := svc.Commit(actor, pv2.ImportJobID); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	if quotaOf("DOI") != 30 {
		t.Fatalf("re-declared quota: got %d, want 30", quotaOf("DOI"))
	}

	// Export: the catalog in the import layout, declared quota in "Định mức",
	// the size estimate in its own read-only column.
	data, name, err := svc.MasterExportXLSX()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if name != "sku-hien-co.xlsx" {
		t.Fatalf("export filename %q", name)
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open export: %v", err)
	}
	defer f.Close()
	grid, err := f.GetRows(f.GetSheetList()[0])
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if strings.Join(grid[0][:8], "|") != strings.Join(masterTemplateHeaders, "|") || grid[0][8] != masterExportEstimateHeader {
		t.Fatalf("export header: %v", grid[0])
	}
	byCode := map[string][]string{}
	for _, r := range grid[1:] {
		if len(r) > 1 {
			byCode[r[1]] = r
		}
	}
	cell := func(r []string, i int) string {
		if i < len(r) {
			return r[i]
		}
		return ""
	}
	if r := byCode["MOI"]; cell(r, 3) != "Mica" || cell(r, 6) != "40" || cell(r, 8) != "100" {
		t.Fatalf("MOI export row: %v (want Mica, declared 40, estimate 100)", r)
	}
	if r := byCode["UOC"]; cell(r, 6) != "" || cell(r, 8) != "25" {
		t.Fatalf("UOC export row: %v (want blank declared, estimate 25)", r)
	}
	if r := byCode["DOI"]; cell(r, 6) != "30" || cell(r, 8) != "" {
		t.Fatalf("DOI export row: %v (declared 30, no size → no estimate)", r)
	}
}
