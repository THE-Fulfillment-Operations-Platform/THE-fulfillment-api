package services

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"the-fulfillment/backend/internal/models"
)

func fptr(v float64) *float64 { return &v }

// A child SKU declares only what differs from its parent; everything else comes
// from the parent, field by field.
func TestEffectiveShipping_FieldByFieldFallback(t *testing.T) {
	parent := &models.SKU{ShipWeightG: fptr(100), ShipLengthCM: fptr(20), ShipWidthCM: fptr(15),
		ShipHeightCM: fptr(2), DeclaredValue: fptr(5), HSCode: "44209000"}
	child := &models.SKU{ShipWeightG: fptr(180)}

	spec := models.EffectiveShipping(child, parent)
	if *spec.WeightG != 180 {
		t.Fatalf("child's own weight must win, got %v", *spec.WeightG)
	}
	if *spec.LengthCM != 20 || *spec.DeclaredValue != 5 || spec.HSCode != "44209000" {
		t.Fatalf("undeclared fields must come from the parent: %+v", spec)
	}
	if m := spec.Missing(); len(m) != 0 {
		t.Fatalf("complete spec reported missing %v", m)
	}
	if m := models.EffectiveShipping(&models.SKU{}, nil).Missing(); len(m) != 4 {
		t.Fatalf("a bare top-level SKU lacks all 4 groups, got %v", m)
	}
}

func TestParseShipCell(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"", 0, true},
		{"120", 120, true},
		{"120,5", 120.5, true},
		{"120.5", 120.5, true},
		{"0,25", 0.25, true},
		{"1,000", 0, false}, // thousands or decimal? refuse
		{"1.000", 0, false},
		{"1.000,5", 0, false},
		{"abc", 0, false},
		{"-3", 0, false},
	}
	for _, c := range cases {
		got, msg := parseShipCell(c.in)
		if (msg == "") != c.ok {
			t.Errorf("parseShipCell(%q): msg=%q want ok=%v", c.in, msg, c.ok)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("parseShipCell(%q) = %v want %v", c.in, got, c.want)
		}
	}
}

func TestNormalizeHSCode(t *testing.T) {
	if got, err := normalizeHSCode(" 3926.40.00 "); err != nil || got != "39264000" {
		t.Fatalf("dots/spaces must be dropped: %q %v", got, err)
	}
	if got, err := normalizeHSCode(""); err != nil || got != "" {
		t.Fatalf("blank clears: %q %v", got, err)
	}
	for _, bad := range []string{"39A64000", "1234", "12345678901234"} {
		if _, err := normalizeHSCode(bad); err == nil {
			t.Errorf("normalizeHSCode(%q) must be refused", bad)
		}
	}
}

// Export → fill → import round-trips: the parent row declares the family, a
// child row overrides its weight, and the preview says what each SKU will still
// lack. Nothing is written before Commit.
func TestSKUShippingImport_RoundTrip(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	parent := models.SKU{Code: "ASK", Name: "Bảng gỗ"}
	if err := db.Create(&parent).Error; err != nil {
		t.Fatal(err)
	}
	child := models.SKU{Code: "ASK-5X5", Name: "Bảng gỗ 5x5", ParentID: &parent.ID}
	lone := models.SKU{Code: "LWD-12IN", Name: "Thớt"}
	if err := db.Create(&child).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&lone).Error; err != nil {
		t.Fatal(err)
	}

	data, _, err := svc.SKUShippingExportXLSX()
	if err != nil {
		t.Fatal(err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sheet := f.GetSheetList()[0]
	rows, _ := f.GetRows(sheet)
	if len(rows) != 4 || rows[1][0] != "ASK" || rows[2][0] != "ASK-5X5" || rows[2][1] != "ASK" {
		t.Fatalf("export must list the parent then its child: %v", rows)
	}
	// Fill: parent gets the whole family's data, the child only a weight, the
	// standalone SKU nothing.
	set := func(cell, v string) {
		if err := f.SetCellValue(sheet, cell, v); err != nil {
			t.Fatal(err)
		}
	}
	set("D2", "100")
	set("E2", "20")
	set("F2", "15")
	set("G2", "2,5")
	set("H2", "5")
	set("I2", "4420.90.00")
	set("D3", "180")
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}

	fileRows, present, err := ParseSKUShippingXLSX(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := svc.PreviewSKUShippingImport(fileRows, present)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.CanCommit || preview.Changed != 2 || preview.Unchanged != 1 || preview.Errors != 0 {
		t.Fatalf("preview: %+v", preview)
	}
	if preview.Incomplete != 1 {
		t.Fatalf("only the standalone SKU should still lack data, got %d incomplete", preview.Incomplete)
	}
	for _, r := range preview.Rows {
		if r.Code == "ASK-5X5" && len(r.Missing) != 0 {
			t.Fatalf("the child inherits the parent's NEW values in the same file: missing %v", r.Missing)
		}
	}
	var untouched models.SKU
	db.First(&untouched, parent.ID)
	if untouched.ShipWeightG != nil {
		t.Fatal("preview must not write")
	}

	if _, err := svc.CommitSKUShippingImport(Actor{}, fileRows, present); err != nil {
		t.Fatal(err)
	}
	var p, c models.SKU
	db.First(&p, parent.ID)
	db.First(&c, child.ID)
	if p.ShipWeightG == nil || *p.ShipWeightG != 100 || *p.ShipHeightCM != 2.5 || p.HSCode != "44209000" {
		t.Fatalf("parent not saved: %+v", p)
	}
	if c.ShipWeightG == nil || *c.ShipWeightG != 180 || c.ShipLengthCM != nil || c.HSCode != "" {
		t.Fatalf("child must keep only its own weight (the rest inherited): %+v", c)
	}

	// Re-importing the untouched export of the new state is a no-op.
	again, _, _ := svc.SKUShippingExportXLSX()
	fileRows2, present2, _ := ParseSKUShippingXLSX(bytes.NewReader(again))
	p2, _ := svc.PreviewSKUShippingImport(fileRows2, present2)
	if p2.Changed != 0 || p2.CanCommit {
		t.Fatalf("re-import of an untouched export must change nothing: %+v", p2)
	}
}

// One bad row refuses the whole file: half a catalogue updated is worse than
// none, because nobody can tell which half.
func TestSKUShippingImport_ErrorRowBlocksCommit(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	db.Create(&models.SKU{Code: "A1", Name: "A"})
	csv := "Mã SKU,Cân nặng (g)\nA1,120\nKHONG-CO,50\nA1,130\n"
	rows, present, err := ParseSKUShippingCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := svc.PreviewSKUShippingImport(rows, present)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Errors != 2 || preview.CanCommit {
		t.Fatalf("unknown SKU + duplicate row must both be errors: %+v", preview)
	}
	if _, err := svc.CommitSKUShippingImport(Actor{}, rows, present); err == nil {
		t.Fatal("commit must refuse a file with error rows")
	}
	var a models.SKU
	db.Where("code = ?", "A1").First(&a)
	if a.ShipWeightG != nil {
		t.Fatal("nothing may be written when the file is refused")
	}
}

// A file with only some shipping columns touches only those columns.
func TestSKUShippingImport_AbsentColumnsUntouched(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	db.Create(&models.SKU{Code: "A1", Name: "A", HSCode: "44209000", DeclaredValue: fptr(7)})
	rows, present, err := ParseSKUShippingCSV(strings.NewReader("SKU,Weight (g)\nA1,250\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CommitSKUShippingImport(Actor{}, rows, present); err != nil {
		t.Fatal(err)
	}
	var a models.SKU
	db.Where("code = ?", "A1").First(&a)
	if a.ShipWeightG == nil || *a.ShipWeightG != 250 || a.HSCode != "44209000" || a.DeclaredValue == nil || *a.DeclaredValue != 7 {
		t.Fatalf("only the weight may change: %+v", a)
	}
}
