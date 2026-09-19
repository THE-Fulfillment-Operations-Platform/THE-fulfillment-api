package services

import (
	"bytes"
	"testing"
)

// The order-import template is only useful if a user can download it, fill it in,
// and upload it back unchanged. That means every header the template ships MUST
// be one the parser recognises, and the sample rows must survive a round-trip.
func TestOrderImportTemplateXLSX_RoundTrips(t *testing.T) {
	svc := &ImportService{}
	data, name, err := svc.OrderImportTemplateXLSX()
	if err != nil {
		t.Fatal(err)
	}
	if name != "order-import-template.xlsx" {
		t.Fatalf("unexpected filename: %s", name)
	}

	// Every header must normalize to a headerToField key — otherwise a column the
	// template advertises would be silently dropped on re-upload.
	for _, h := range orderImportTemplateHeaders {
		if _, ok := headerToField[headerKey(h)]; !ok {
			t.Errorf("template header %q does not map to any parser field", h)
		}
	}

	// Parse the generated workbook through the real importer.
	rows, hdr, err := ParseXLSX(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 sample rows, got %d", len(rows))
	}
	// The template must satisfy its own required-column rule and carry no column
	// the parser cannot place.
	if len(hdr.Missing) != 0 || len(hdr.Unknown) != 0 || len(hdr.Retired) != 0 {
		t.Fatalf("template header report should be clean, got %+v", hdr)
	}
	r0 := rows[0]
	// Spot-check that columns landed in the right fields — including the Vietnamese
	// headers "Mã ảnh (nếu có)" (→ ImageCode) and "Địa chỉ nhận" vs its "(Phụ)"
	// sibling, the pair most at risk of collapsing onto each other.
	if r0.StoreOrderID != "Etsy-9001" || r0.SKU != "WOOD-01" || r0.ImageCode != "IMG-9001" ||
		r0.Quantity != 1 || r0.ShippingCountry != "US" || r0.ShippingName != "John Doe" ||
		r0.ShippingAddress1 != "12 Main St" || r0.ShippingAddress2 != "" ||
		r0.OrderDateRaw != "2026-08-20" || r0.SellerRef != "SELLER01" {
		t.Fatalf("row0 columns not split cleanly: %+v", r0)
	}
	if rows[1].BackDesign == "" || rows[1].BackDesign == rows[1].FrontDesignValue() {
		t.Fatalf("row1 should carry a distinct back design: %+v", rows[1])
	}
}

func TestMasterTemplateXLSX_RoundTrips(t *testing.T) {
	svc := &MasterImportService{}
	data, name, err := svc.MasterTemplateXLSX()
	if err != nil {
		t.Fatal(err)
	}
	if name != "master-data-template.xlsx" {
		t.Fatalf("unexpected filename: %s", name)
	}
	rows, err := ParseLegacyFile("XLSX", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("want 5 sample rows, got %d", len(rows))
	}
	if rows[0].SKU != "HOP-NHUA-BE" || rows[0].Material != "Mica trong 3 ly" ||
		rows[0].ParentSKU != "HOP-NHUA" || rows[0].Length != "80" || rows[0].Width != "60" {
		t.Fatalf("row0 not split cleanly: %+v", rows[0])
	}
	// The sample's sizes must read back as millimetres, not trip the parser.
	for _, r := range rows {
		if _, _, msg := parseRowDims(r); msg != "" {
			t.Fatalf("sample row %d has an unreadable size: %s", r.RowNumber, msg)
		}
	}
	// The combo sample row must survive the round-trip and split into 2 materials.
	combo := rows[4]
	if combo.SKU != "COMBO-A2-GAI" {
		t.Fatalf("combo row not split cleanly: %+v", combo)
	}
	if mats := splitMaterials(combo.Material); len(mats) != 2 {
		t.Fatalf("combo cell should split into 2 materials, got %v", mats)
	}
}
