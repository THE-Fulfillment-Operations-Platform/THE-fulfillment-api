package services

import (
	"encoding/json"
	"strings"
	"testing"

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
		{"StoreOrderID", "Account", "StoreName", "Quantity", "SKU", "Mã ảnh", "Design", "Mockup", "EngraveText", "ShippingName", "ShippingAddress1", "ShippingCountry", "Note"},
		{"Etsy-1", "acc-01", "MyStore", "3", "WOOD-01", "IMG-77", "design-a", "https://m.example.com/1.png", "Love", "John Doe", "1 Main St", "US", "rush"},
	}
	rows, _, err := rowsFromRecords("CSV", records)
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
// The production sheet is the five columns the factory asked for (2026-10-04):
// batch, internal code, SKU, quantity, design link — one row per live item.
func TestProductionTemplateGrid(t *testing.T) {
	order := &models.Order{StoreOrderID: "Etsy-1", ShippingName: "John Doe"}
	item := models.OrderItem{
		InternalCode: "ORD-000001_1", SKUCode: "WOOD-01", Quantity: 3,
		DesignURL: " https://d/1 ", MockupURL: "https://m/1", Order: order,
	}
	gone := models.OrderItem{InternalCode: "ORD-000002_1", SKUCode: "WOOD-02", Quantity: 1,
		CancellationStatus: models.CancellationApproved}
	batch := &models.Batch{
		Code:  "#101001",
		Items: []models.BatchItem{{OrderItem: &item}, {OrderItem: &gone}},
	}

	grid := ProductionTemplateGrid(batch)
	want := [][]string{
		{"Số batch", "Mã nội bộ", "SKU", "Số lượng", "Link design"},
		{"#101001", "ORD-000001_1", "WOOD-01", "3", "https://d/1"},
	}
	if len(grid) != len(want) {
		t.Fatalf("got %d rows, want %d (cancelled items are left out): %v", len(grid), len(want), grid)
	}
	for r := range want {
		if strings.Join(grid[r], "|") != strings.Join(want[r], "|") {
			t.Errorf("row %d = %v, want %v", r, grid[r], want[r])
		}
	}
}

// A two-sided product needs its back design too: the column appears only when
// the batch holds one, so one-sided batches keep exactly five columns.
func TestProductionTemplateGrid_BackDesignColumnOnlyWhenNeeded(t *testing.T) {
	front := models.OrderItem{InternalCode: "A", SKUCode: "S", Quantity: 1, DesignURL: "https://f/a"}
	both := models.OrderItem{InternalCode: "B", SKUCode: "S", Quantity: 2, DesignURL: "https://f/b", BackDesignURL: "https://b/b"}
	grid := ProductionTemplateGrid(&models.Batch{Code: "#1", Items: []models.BatchItem{{OrderItem: &front}, {OrderItem: &both}}})
	if got := strings.Join(grid[0], "|"); got != "Số batch|Mã nội bộ|SKU|Số lượng|Link design|Link design mặt sau" {
		t.Fatalf("header = %s", got)
	}
	if grid[1][5] != "" || grid[2][5] != "https://b/b" {
		t.Errorf("back design cells = %q, %q", grid[1][5], grid[2][5])
	}
}
