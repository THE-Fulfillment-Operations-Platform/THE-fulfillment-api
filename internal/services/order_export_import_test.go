package services

import (
	"bytes"
	"testing"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// The "export as import template" file must import straight back: same columns
// as the template (clean header), every row valid, and the re-import recreates
// the same order — recipient, SKUs, designs, item count.
func TestExportItemsAsImportXLSX_RoundTrips(t *testing.T) {
	db := newImportDB(t)
	imp := importSvc(db)
	actor := Actor{ID: 1}

	// Seed one order of 3 items (rows of quantity 1 and 2) through the real import.
	grid := [][]string{orderImportTemplateHeaders}
	for _, sample := range orderImportTemplateSample {
		filled := append([]string(nil), sample...)
		for i, h := range orderImportTemplateHeaders {
			switch h {
			case "Seller ID":
				filled[i] = "S1"
			case "MÃ SKU":
				filled[i] = "TESTSKU"
			}
		}
		grid = append(grid, filled)
	}
	data, _ := buildTemplateXLSX("Đơn hàng", grid, orderImportTemplateWidths)
	rows, hdr, _ := ParseXLSX(bytes.NewReader(data))
	prev, err := imp.Preview(actor, 1, "XLSX", "seed.xlsx", rows, hdr)
	if err != nil || prev.ErrorRows != 0 {
		t.Fatalf("seed preview: %v %+v", err, prev)
	}
	if _, err := imp.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	seeded := loadOrders(t, db, 1)

	orders := &OrderService{repo: repositories.New(db), audit: imp.audit}
	if _, _, err := orders.ExportItemsAsImportXLSX(Actor{ID: 1, Role: models.RoleOwner}, repositories.ItemFilter{}); err == nil {
		t.Fatal("only the technician may export orders as an import file")
	}
	out, name, err := orders.ExportItemsAsImportXLSX(Actor{ID: 1, Role: models.RoleTechnician}, repositories.ItemFilter{})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if name == "" {
		t.Error("export needs a filename")
	}

	back, hdr2, err := ParseXLSX(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	if len(hdr2.Missing) != 0 || len(hdr2.Unknown) != 0 || len(hdr2.Retired) != 0 {
		t.Fatalf("exported header is not the import template's: %+v", hdr2)
	}
	if len(back) != 3 {
		t.Fatalf("one row per item: got %d rows, want 3", len(back))
	}
	r := back[0]
	if r.SellerRef != "S1" || r.StoreOrderID != "Etsy-9001" || r.ShippingName != "John Doe" ||
		r.ShippingZip != "73301" || r.ShippingProvince != "TX" || r.OrderDateRaw != "2026-08-20" ||
		r.FrontDesignValue() == "" || r.Mockup == "" || int(r.Quantity) != 1 {
		t.Fatalf("exported row lost data: %+v", r)
	}

	prev2, err := imp.Preview(actor, 1, "XLSX", name, back, hdr2)
	if err != nil || prev2.ErrorRows != 0 || prev2.OrderCount != 1 || prev2.ItemCount != 3 {
		t.Fatalf("re-import preview: err=%v %+v", err, prev2)
	}
	if _, err := imp.Commit(actor, prev2.ImportJobID); err != nil {
		t.Fatalf("re-import commit: %v", err)
	}
	all := loadOrders(t, db, 1)
	if len(all) != 2 || len(all[1].Items) != len(seeded[0].Items) || all[1].ShippingAddress1 != seeded[0].ShippingAddress1 {
		t.Fatalf("re-import must recreate the same order: %+v", all)
	}
	for i, it := range all[1].Items {
		if it.SKUCode != seeded[0].Items[i].SKUCode || it.DesignURL != seeded[0].Items[i].DesignURL {
			t.Errorf("item %d differs after round trip: %+v vs %+v", i+1, it, seeded[0].Items[i])
		}
	}
}
