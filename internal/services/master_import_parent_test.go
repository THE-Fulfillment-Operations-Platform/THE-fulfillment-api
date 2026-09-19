package services

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

func TestParseDimMM(t *testing.T) {
	cases := []struct {
		in   string
		want float64 // 0 = expect nil
		ok   bool
	}{
		{"", 0, true},
		{"80", 80, true},
		{"80,5", 80.5, true}, // Vietnamese decimal comma
		{"80.5", 80.5, true},
		{"88.88", 88.88, true},
		{"80mm", 80, true},
		{"80 MM", 80, true},
		{"4in", 0, false}, // another unit is refused, never read as 4 mm
		{"10 cm", 0, false},
		{"1,000", 0, false}, // thousands or decimal? refuse to guess
		{"1.000", 0, false},
		{"0", 0, false},
		{"-5", 0, false},
		{"abc", 0, false},
		{"200000", 0, false}, // past maxDimMM
	}
	for _, c := range cases {
		got, msg := parseDimMM(c.in, "D")
		if c.ok != (msg == "") {
			t.Errorf("parseDimMM(%q): msg=%q, want ok=%v", c.in, msg, c.ok)
			continue
		}
		switch {
		case c.want == 0 && got != nil:
			t.Errorf("parseDimMM(%q) = %v, want nil", c.in, *got)
		case c.want != 0 && (got == nil || *got != c.want):
			t.Errorf("parseDimMM(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	// A combined "D x R" cell fills whichever side has no column of its own.
	l, w, msg := parseRowDims(LegacyRow{Size: "100 x 50,5mm"})
	if msg != "" || !dimEq(l, 100) || !dimEq(w, 50.5) {
		t.Fatalf("combined cell: %v x %v (%s)", l, w, msg)
	}
	if _, _, msg := parseRowDims(LegacyRow{Size: "100"}); msg == "" {
		t.Fatalf("a combined cell without the x must be refused")
	}
}

// TestParseLegacyFile_ParentAndSizeColumns: the step-2 template's columns are
// found by header, and an order file's "Note" never becomes the SKU description.
func TestParseLegacyFile_ParentAndSizeColumns(t *testing.T) {
	csv := "SKU cha,SKU,Tên sản phẩm,Loại VL,D (mm),R (mm),Mô tả,Note\n" +
		"HOP-NHUA,HOP-BE,Hộp nhựa bé,Mica,80,60,nắp trượt,giao gấp\n"
	rows, err := ParseLegacyFile("CSV", strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	r := rows[0]
	if r.ParentSKU != "HOP-NHUA" || r.SKU != "HOP-BE" || r.Length != "80" || r.Width != "60" || r.Description != "nắp trượt" {
		t.Fatalf("columns not picked up: %+v", r)
	}

	rows, err = ParseLegacyFile("CSV", strings.NewReader("SKU,Loại VL,D x R (mm),Ghi chú\nHOP-BE,Mica,80 x 60,giao gấp\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Size != "80 x 60" || rows[0].Description != "" {
		t.Fatalf("combined size / note: %+v", rows[0])
	}
}

// seedParentCatalog: HOP-NHUA is a parent-to-be (top level, no children),
// HOP-CU a standalone SKU, and KHAC a parent that already holds KHAC-1. The
// material "Mica" exists with a 100 × 100 mm sheet, so children with a size get
// a derived quota (and a child bigger than the sheet is refused).
func seedParentCatalog(t *testing.T, db *gorm.DB) (hop, cu, khac, khac1 *models.SKU) {
	t.Helper()
	if err := db.Create(&models.Material{Code: "MICA", Name: "Mica", LengthMM: ptrFloat(100), WidthMM: ptrFloat(100)}).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	hop = &models.SKU{Code: "HOP-NHUA", Name: "HOP-NHUA", ProductName: "Hộp nhựa"}
	cu = &models.SKU{Code: "HOP-CU", Name: "HOP-CU", ProductName: "Hộp cũ"}
	khac = &models.SKU{Code: "KHAC", Name: "KHAC"}
	for _, s := range []*models.SKU{hop, cu, khac} {
		if err := db.Create(s).Error; err != nil {
			t.Fatalf("seed %s: %v", s.Code, err)
		}
	}
	khac1 = &models.SKU{Code: "KHAC-1", Name: "KHAC-1", ParentID: &khac.ID}
	if err := db.Create(khac1).Error; err != nil {
		t.Fatalf("seed child: %v", err)
	}
	return
}

func errorCodes(pv *MasterImportPreview) map[string]string {
	out := map[string]string{}
	for _, e := range pv.Errors {
		out[normalizeSKUCode(e.SKU)] = e.ErrorCode
	}
	return out
}

// TestMasterImport_ChildrenGoUnderExistingParents is step 2: children are filed
// under parents that step 1 already created. Every way a row can point at the
// wrong parent (or carry a size nobody can vouch for) drops that SKU with a
// reason, and the rest of the file still imports.
func TestMasterImport_ChildrenGoUnderExistingParents(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	actor := Actor{ID: 1, Role: models.RoleOwner}
	hop, cu, _, _ := seedParentCatalog(t, db)

	rows := []LegacyRow{
		{RowNumber: 1, ParentSKU: "hop nhua", SKU: "Hop nhua be", ProductName: "Hộp nhựa bé", Material: "Mica",
			Length: "80", Width: "60,5", Description: "nắp trượt"},
		{RowNumber: 2, ParentSKU: "HOP-NHUA", SKU: "HOP-CU", Material: "Mica", Size: "100 x 100mm"},
		{RowNumber: 3, ParentSKU: "CHUA-CO", SKU: "MO-COI", Material: "Gỗ lạ"},
		{RowNumber: 4, ParentSKU: "KHAC-1", SKU: "CHAU", Material: "Mica"},
		{RowNumber: 5, ParentSKU: "HOP-NHUA", SKU: "KHAC", Material: "Mica"},
		{RowNumber: 6, SKU: "LE", Material: "Mica", Length: "4in"},
		{RowNumber: 7, ParentSKU: "HOP-NHUA", SKU: "HOP-NHUA"},
		{RowNumber: 8, SKU: "LE-2", Material: "Mica"},
		{RowNumber: 9, ParentSKU: "HOP-NHUA", SKU: "XUNG", Material: "Mica", Length: "10", Width: "10"},
		{RowNumber: 10, ParentSKU: "HOP-NHUA", SKU: "XUNG", Material: "Mica", Length: "20", Width: "20"},
		{RowNumber: 11, ParentSKU: "HOP-NHUA", SKU: "HAI-CHA", Material: "Mica"},
		{RowNumber: 12, ParentSKU: "KHAC", SKU: "HAI-CHA", Material: "Mica"},
		// HOP-CU is top level today, but row 2 files it under HOP-NHUA — so it
		// can't also be a parent in the same file (that would be 3 levels).
		{RowNumber: 13, ParentSKU: "HOP-CU", SKU: "CON-CUA-CU", Material: "Mica"},
		// Half a size is no size.
		{RowNumber: 14, SKU: "NUA-VOI", Material: "Mica", Length: "50"},
		// Bigger than one 100×100 sheet of Mica — can't be cut from it at all.
		{RowNumber: 15, SKU: "QUA-TO", Material: "Mica", Length: "300", Width: "300"},
	}
	pv, err := svc.Preview(actor, "XLSX", "children.xlsx", rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	wantErr := map[string]string{
		"MO-COI":     errParentNotFound,
		"CHAU":       errParentIsChild,
		"KHAC":       errSKUHasChildren,
		"LE":         errDimInvalid,
		"HOP-NHUA":   errParentSelf,
		"XUNG":       errDimConflict,
		"HAI-CHA":    errParentConflict,
		"CON-CUA-CU": errParentIsChild,
		"NUA-VOI":    errDimInvalid,
		"QUA-TO":     errDimTooBig,
	}
	got := errorCodes(pv)
	for code, want := range wantErr {
		if got[code] != want {
			t.Errorf("%s: error %q, want %q (all: %+v)", code, got[code], want, pv.Errors)
		}
	}
	if len(pv.Errors) != len(wantErr) {
		t.Errorf("errors = %+v, want exactly %d", pv.Errors, len(wantErr))
	}
	for i := 1; i < len(pv.Errors); i++ {
		if pv.Errors[i-1].RowNumber > pv.Errors[i].RowNumber {
			t.Fatalf("errors not in file order: %+v", pv.Errors)
		}
	}
	if len(pv.SKUs) != 3 {
		t.Fatalf("planned SKUs = %+v, want HOP-NHUA-BE, HOP-CU, LE-2", pv.SKUs)
	}
	for _, m := range pv.Materials {
		if m.Name == "Gỗ lạ" {
			t.Fatalf("a material only a rejected SKU uses must not be created: %+v", pv.Materials)
		}
	}
	if pv.Summary.ChildSKUs != 2 || pv.Summary.ParentGroups != 1 {
		t.Fatalf("summary = %+v, want 2 children in 1 group", pv.Summary)
	}
	be := skuPlanByCode(t, pv, "HOP-NHUA-BE")
	if be.ParentCode != "HOP-NHUA" || !dimEq(be.LengthMM, 80) || !dimEq(be.WidthMM, 60.5) || be.Description != "nắp trượt" {
		t.Fatalf("child plan = %+v", be)
	}
	// The preview already says what the quota will be: ⌊100×100 / 80×60,5⌋ = 2.
	if be.QuotaByMaterial["Mica"] != 2 {
		t.Fatalf("quota_by_material = %v, want Mica: 2", be.QuotaByMaterial)
	}
	if le2 := skuPlanByCode(t, pv, "LE-2"); len(le2.QuotaByMaterial) != 0 {
		t.Fatalf("a SKU without a size has no quota to show, got %v", le2.QuotaByMaterial)
	}

	res, err := svc.Commit(actor, pv.ImportJobID)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Applied.SKUsCreated != 2 || res.Applied.SKUsUpdated != 1 {
		t.Fatalf("applied = %+v, want 2 created (HOP-NHUA-BE, LE-2) + 1 updated (HOP-CU)", res.Applied)
	}

	var newChild models.SKU
	if err := db.Where("code = ?", "HOP-NHUA-BE").First(&newChild).Error; err != nil {
		t.Fatalf("load new child: %v", err)
	}
	if newChild.ParentID == nil || *newChild.ParentID != hop.ID || !dimEq(newChild.LengthMM, 80) ||
		!dimEq(newChild.WidthMM, 60.5) || newChild.Description != "nắp trượt" {
		t.Fatalf("new child stored as %+v", newChild)
	}
	attached := loadSKU(t, db, cu.ID)
	if attached.ParentID == nil || *attached.ParentID != hop.ID || !dimEq(attached.LengthMM, 100) || !dimEq(attached.WidthMM, 100) {
		t.Fatalf("existing SKU not filed under the parent: %+v", attached)
	}
	if attached.ProductName != "Hộp cũ" {
		t.Fatalf("a blank Tên sản phẩm must not clear the name, got %q", attached.ProductName)
	}
	var lone models.SKU
	if err := db.Where("code = ?", "LE-2").First(&lone).Error; err != nil || lone.ParentID != nil {
		t.Fatalf("standalone SKU: %+v (%v)", lone, err)
	}

	// Re-importing the same file changes nothing.
	pv2, err := svc.Preview(actor, "XLSX", "children.xlsx", rows)
	if err != nil {
		t.Fatalf("re-preview: %v", err)
	}
	res2, err := svc.Commit(actor, pv2.ImportJobID)
	if err != nil {
		t.Fatalf("re-commit: %v", err)
	}
	if res2.Applied.SKUsCreated != 0 || res2.Applied.SKUsUpdated != 0 {
		t.Fatalf("re-import applied %+v, want nothing", res2.Applied)
	}

	// The job remembers what its commit did.
	job, err := svc.Get(pv.ImportJobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Applied == nil || job.Applied.SKUsUpdated != 1 || job.Applied.SKUsCreated != 2 {
		t.Fatalf("stored job applied = %+v", job.Applied)
	}
}

// TestMasterImport_ParentGoneBeforeCommit: a parent deleted between preview and
// commit stops the commit, instead of importing its children as standalone SKUs.
func TestMasterImport_ParentGoneBeforeCommit(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	actor := Actor{ID: 1, Role: models.RoleOwner}
	hop, _, _, _ := seedParentCatalog(t, db)

	pv, err := svc.Preview(actor, "XLSX", "x.xlsx", []LegacyRow{
		{RowNumber: 1, ParentSKU: "HOP-NHUA", SKU: "HOP-BE", Material: "Mica"},
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if err := db.Delete(&models.SKU{}, hop.ID).Error; err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	if _, err := svc.Commit(actor, pv.ImportJobID); err == nil || !strings.Contains(err.Error(), "Xem trước lại") {
		t.Fatalf("commit err = %v, want a conflict asking to preview again", err)
	}
	var n int64
	db.Model(&models.SKU{}).Where("code = ?", "HOP-BE").Count(&n)
	if n != 0 {
		t.Fatalf("the child was created without its parent")
	}
}
