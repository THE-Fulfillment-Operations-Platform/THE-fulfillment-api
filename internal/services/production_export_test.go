package services

import (
	"encoding/json"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"

	"the-fulfillment/backend/internal/models"
)

// TestImportRowJSONAcceptsTemplateLabels verifies the paste/JSON import path maps
// the seller template's own column labels (incl. the Vietnamese "Mã ảnh") onto
// ImportRow — not only the struct's canonical keys.
func TestImportRowJSONAcceptsTemplateLabels(t *testing.T) {
	payload := []byte(`{"StoreOrderID":"Etsy-1","Account":"acc-01","Mã ảnh":"IMG-9001","SKU":"WOOD-01","Quantity":2,"Mockup":"https://m/1"}`)
	var r ImportRow
	if err := json.Unmarshal(payload, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.ImageCode != "IMG-9001" {
		t.Errorf("ImageCode = %q, want IMG-9001", r.ImageCode)
	}
	if r.Account != "acc-01" {
		t.Errorf("Account = %q, want acc-01", r.Account)
	}
	if r.StoreOrderID != "Etsy-1" || r.SKU != "WOOD-01" || r.Mockup != "https://m/1" {
		t.Errorf("unexpected row: %+v", r)
	}
	if int(r.Quantity) != 2 {
		t.Errorf("Quantity = %d, want 2", int(r.Quantity))
	}
}

// TestImportRowJSONRoundTrip verifies marshal→unmarshal is lossless, protecting
// the internal RawRows (preview→commit) round-trip.
func TestImportRowJSONRoundTrip(t *testing.T) {
	in := ImportRow{StoreOrderID: "E1", Account: "a1", ImageCode: "IMG", SKU: "W1", Quantity: 3, Note: "n"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ImportRow
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.StoreOrderID != "E1" || out.Account != "a1" || out.ImageCode != "IMG" ||
		out.SKU != "W1" || int(out.Quantity) != 3 || out.Note != "n" {
		t.Errorf("round-trip lost data: %+v", out)
	}
}

// TestRowsFromRecordsMapsSellerColumns verifies the seller-template header
// mapping, including the newly-added Account column and the Vietnamese "Mã ảnh"
// (image code) header.
func TestRowsFromRecordsMapsSellerColumns(t *testing.T) {
	records := [][]string{
		{"StoreOrderID", "Account", "StoreName", "Quantity", "SKU", "Mã ảnh", "Design", "Mockup", "EngraveText", "ShippingName", "ShippingAddress1", "ShippingCountry", "IOSS", "Note"},
		{"Etsy-1", "acc-01", "MyStore", "3", "WOOD-01", "IMG-77", "design-a", "https://m.example.com/1.png", "Love", "John Doe", "1 Main St", "US", "IM123", "rush"},
	}
	rows, err := rowsFromRecords("CSV", records)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	checks := map[string]struct{ got, want string }{
		"StoreOrderID": {r.StoreOrderID, "Etsy-1"},
		"Account":      {r.Account, "acc-01"},
		"StoreName":    {r.StoreName, "MyStore"},
		"SKU":          {r.SKU, "WOOD-01"},
		"ImageCode":    {r.ImageCode, "IMG-77"},
		"Design":       {r.Design, "design-a"},
		"Mockup":       {r.Mockup, "https://m.example.com/1.png"},
		"EngraveText":  {r.EngraveText, "Love"},
		"ShippingName": {r.ShippingName, "John Doe"},
		"IOSS":         {r.IOSS, "IM123"},
		"Note":         {r.Note, "rush"},
	}
	for name, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", name, c.got, c.want)
		}
	}
	if int(r.Quantity) != 3 {
		t.Errorf("Quantity: got %d, want 3", int(r.Quantity))
	}
}

// TestURLValidationHelpers verifies that a bare design code is not treated as a
// malformed URL, while a real URL attempt that is not http(s) is rejected.
func TestURLValidationHelpers(t *testing.T) {
	if !isValidHTTPURL("https://x.com/a.png") {
		t.Error("valid https URL should pass")
	}
	if isValidHTTPURL("ftp://x.com") {
		t.Error("ftp URL should fail http(s) check")
	}
	if isValidHTTPURL("just some text") {
		t.Error("plain text should fail")
	}
	if looksLikeURL("design-a") {
		t.Error("bare reference code should not look like a URL")
	}
	if !looksLikeURL("http://x.com") {
		t.Error("http string should look like a URL")
	}
	if !looksLikeURL("gs://bucket/key") {
		t.Error("scheme:// string should look like a URL")
	}
}

// TestSeqStr verifies an unassigned (0) production sequence exports as blank.
func TestSeqStr(t *testing.T) {
	if got := seqStr(0); got != "" {
		t.Errorf("seqStr(0) = %q, want empty string", got)
	}
	if got := seqStr(7); got != "7" {
		t.Errorf("seqStr(7) = %q, want \"7\"", got)
	}
}

// TestNormalizeHeaderNFD verifies an NFD-decomposed "Mã ảnh" header still maps to
// the image-code column after NFC normalization.
func TestNormalizeHeaderNFD(t *testing.T) {
	nfc := "Mã ảnh"
	nfd := norm.NFD.String(nfc)
	if nfd == nfc {
		t.Skip("no distinct NFD form on this platform")
	}
	if normalizeHeader(nfd) != normalizeHeader(nfc) {
		t.Errorf("NFD header %q normalized to %q, want %q", nfd, normalizeHeader(nfd), normalizeHeader(nfc))
	}
	if _, ok := headerToField[normalizeHeader(nfd)]; !ok {
		t.Errorf("normalized NFD header %q has no field mapping", normalizeHeader(nfd))
	}
}

// TestProductionTemplateGrid verifies the legacy production-template column order
// (17 columns, with "Mã nội bộ" appearing twice) and the per-field row mapping.
func TestProductionTemplateGrid(t *testing.T) {
	order := &models.Order{StoreOrderID: "Etsy-1", ShippingName: "John Doe"}
	item := models.OrderItem{
		InternalCode:       "ORD-000001_1",
		SKUCode:            "WOOD-01",
		QCDescription:      "Sign 20cm khắc tên",
		ImageCode:          "IMG-77",
		ProductionSequence: 5,
		Quantity:           3,
		DesignURL:          "https://d/1",
		MockupURL:          "https://m/1",
		ProductionFileName: "wood-01.pdf",
		PrintFileURL:       "https://p/1",
		CutFileURL:         "https://c/1",
		Order:              order,
	}
	batch := &models.Batch{
		Code:     "#101001",
		Material: models.Material{Name: "Gỗ", Code: "WOOD"},
		Items:    []models.BatchItem{{OrderItem: &item, Material: &models.Material{Name: "Gỗ", Code: "WOOD"}}},
	}
	batch.CreatedAt = time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)

	grid := ProductionTemplateGrid(batch)
	if len(grid) != 2 {
		t.Fatalf("want header + 1 data row, got %d rows", len(grid))
	}

	wantHeaders := []string{
		"Mã nội bộ", "SỐ Batch", "Ngày", "Order ID", "SKU", "Loại VL",
		"Mô tả Sp để QC (Hiện lên phần QC)", "Mã ảnh (copy bên TĐN Ctr + Ship + V...)",
		"Số thứ tự", "Số lượng", "Link ảnh", "Mock up", "Mã nội bộ", "Tên khách",
		"Tên File", "Link in", "Link cắt",
	}
	header := grid[0]
	if len(header) != 17 {
		t.Fatalf("want 17 header columns, got %d", len(header))
	}
	for i, h := range wantHeaders {
		if header[i] != h {
			t.Errorf("header[%d]: got %q, want %q", i, header[i], h)
		}
	}

	wantRow := []string{
		"ORD-000001_1", "#101001", "2026-07-06", "Etsy-1", "WOOD-01", "Gỗ",
		"Sign 20cm khắc tên", "IMG-77", "5", "3", "https://d/1", "https://m/1",
		"ORD-000001_1", "John Doe", "wood-01.pdf", "https://p/1", "https://c/1",
	}
	row := grid[1]
	if len(row) != 17 {
		t.Fatalf("want 17 row columns, got %d", len(row))
	}
	for i := range wantRow {
		if row[i] != wantRow[i] {
			t.Errorf("row[%d]: got %q, want %q", i, row[i], wantRow[i])
		}
	}
}
