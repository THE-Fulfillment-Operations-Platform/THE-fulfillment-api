package services

import (
	"testing"

	"the-fulfillment/backend/internal/repositories"
)

// "Quantity 20" and "20 rows of Quantity 1" must make the same order: 20 items,
// each quantity 1, each with its own internal code.
func TestExpandQuantityRows_SameAsRepeatedRows(t *testing.T) {
	big := []ImportRow{{StoreOrderID: "A", SKU: "S1", Quantity: 20}}
	rep := make([]ImportRow, 20)
	for i := range rep {
		rep[i] = ImportRow{StoreOrderID: "A", SKU: "S1", Quantity: 1}
	}
	a := orderItemsFromRows(7, big, nil)
	b := orderItemsFromRows(7, rep, nil)
	if len(a) != 20 || len(b) != 20 {
		t.Fatalf("both shapes must give 20 items, got %d and %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Quantity != 1 || a[i].InternalCode != b[i].InternalCode || a[i].LineNo != b[i].LineNo {
			t.Fatalf("item %d differs: %+v vs %+v", i+1, a[i], b[i])
		}
	}
	if a[0].InternalCode != "100007_1/20" || a[19].InternalCode != "100007_20/20" {
		t.Errorf("codes = %s … %s", a[0].InternalCode, a[19].InternalCode)
	}
	// Mixed: 2 + 1 + 3 → 6 items numbered 1..6 of 6, in row order.
	mixed := orderItemsFromRows(7, []ImportRow{{SKU: "A", Quantity: 2}, {SKU: "B", Quantity: 1}, {SKU: "C", Quantity: 3}}, nil)
	if len(mixed) != 6 || mixed[5].InternalCode != "100007_6/6" || mixed[2].SKUCode != "B" || mixed[3].SKUCode != "C" {
		t.Fatalf("mixed rows: %+v", mixed)
	}
	// The caller's rows are not touched.
	if big[0].Quantity != 20 {
		t.Errorf("input row mutated: %d", big[0].Quantity)
	}
}

// A mistyped quantity must not create thousands of items.
func TestValidateRow_QuantityCap(t *testing.T) {
	s := &ImportService{}
	row := ImportRow{StoreOrderID: "A", SKU: "S1", Quantity: MaxImportQuantity + 1, ShippingName: "x", ShippingAddress1: "y", ShippingCountry: "US"}
	e := s.validateRow(1, row, nil, repositories.SellerIdentity{ID: 1, Code: "S", Name: "S"})
	if e == nil || e.ErrorCode != "QTY_TOO_LARGE" {
		t.Fatalf("quantity above the cap must be refused with QTY_TOO_LARGE, got %+v", e)
	}
}

// One product → bare order code; two or more → base_pos/total.
func TestItemInternalCode_SingleProductHasNoSuffix(t *testing.T) {
	if got := itemInternalCode(3216, 1, 1); got != "103216" {
		t.Errorf("single product = %q, want 103216", got)
	}
	if got := itemInternalCode(3216, 2, 3); got != "103216_2/3" {
		t.Errorf("multi product = %q, want 103216_2/3", got)
	}
	// Quantity 2 on a single line = two products → suffixes are back.
	items := orderItemsFromRows(3216, []ImportRow{{SKU: "A", Quantity: 2}}, nil)
	if items[0].InternalCode != "103216_1/2" || items[1].InternalCode != "103216_2/2" {
		t.Errorf("qty 2 line: %s, %s", items[0].InternalCode, items[1].InternalCode)
	}
}
