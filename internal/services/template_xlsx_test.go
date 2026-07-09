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
		if _, ok := headerToField[normalizeHeader(h)]; !ok {
			t.Errorf("template header %q does not map to any parser field", h)
		}
	}

	// Parse the generated workbook through the real importer.
	rows, err := ParseXLSX(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 sample rows, got %d", len(rows))
	}
	r0 := rows[0]
	// Spot-check that columns landed in the right fields — including the Vietnamese
	// "Mã ảnh" header (→ ImageCode), the one that used to come out garbled.
	if r0.StoreOrderID != "Etsy-9001" || r0.SKU != "WOOD-01" || r0.ImageCode != "IMG-9001" ||
		r0.Quantity != 1 || r0.ShippingCountry != "US" {
		t.Fatalf("row0 columns not split cleanly: %+v", r0)
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
	if len(rows) != 3 {
		t.Fatalf("want 3 sample rows, got %d", len(rows))
	}
	if rows[0].SKU != "BRA-1.6-KEP" || rows[0].Material != "Mica trong 3 ly" {
		t.Fatalf("row0 not split cleanly: %+v", rows[0])
	}
}
