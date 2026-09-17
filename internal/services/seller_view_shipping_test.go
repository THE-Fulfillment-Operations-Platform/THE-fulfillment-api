package services

import (
	"encoding/json"
	"strings"
	"testing"

	"the-fulfillment/backend/internal/models"
)

func shippingOrder() models.Order {
	return models.Order{
		InternalCode: "100001", StoreOrderID: "US-0925-008", SellerID: 1,
		StoreName: "SpringCityShopArt", Account: "AM399",
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusHandedOff,
		ShippingName:     "McKenna Garland",
		ShippingAddress1: "49 14th Ave SW",
		ShippingCity:     "NEW BRIGHTON",
		ShippingProvince: "MN",
		ShippingZip:      "55112",
		ShippingCountry:  "US",
		ShippingPhone:    "1122334455",
		Note:             "Giữ nguyên kích thước như file",
	}
}

// The seller portal showed every recipient field as "—" because the DTO simply
// had no place to put them. This pins the detail payload down field by field, so
// dropping one again fails here instead of on the seller's screen.
func TestToSellerView_DetailCarriesRecipient(t *testing.T) {
	v := toSellerView(shippingOrder(), true, true)

	for _, c := range []struct{ field, got, want string }{
		{"shipping_name", v.ShippingName, "McKenna Garland"},
		{"shipping_address1", v.ShippingAddress1, "49 14th Ave SW"},
		{"shipping_city", v.ShippingCity, "NEW BRIGHTON"},
		{"shipping_province", v.ShippingProvince, "MN"},
		{"shipping_zip", v.ShippingZip, "55112"},
		{"shipping_country", v.ShippingCountry, "US"},
		{"shipping_phone", v.ShippingPhone, "1122334455"},
		{"note", v.Note, "Giữ nguyên kích thước như file"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
}

// The list renders 20 rows and none of them show an address, so the block must
// not ride along there — otherwise every page carries 20 unused address blocks.
func TestToSellerView_ListOmitsRecipient(t *testing.T) {
	v := toSellerView(shippingOrder(), false, true)

	if v.ShippingName != "" || v.ShippingAddress1 != "" || v.ShippingPhone != "" {
		t.Errorf("list view leaked recipient fields: %+v", v)
	}
	// omitempty must actually drop the keys, not send them as empty strings.
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"shipping_name", "shipping_address1", "shipping_phone", "note"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("list payload still contains %q: %s", key, raw)
		}
	}
}

// The list prints the SKUs beside the store order id so a seller can tell orders
// apart by product. A repeated SKU is one entry with its quantities summed, and a
// line the seller already cancelled is no longer part of what they will receive.
func TestToSellerView_ListSummarisesLiveSKUs(t *testing.T) {
	o := shippingOrder()
	o.Items = []models.OrderItem{
		{SKUCode: "CANVAS-16X20", Quantity: 1},
		{SKUCode: "MUG-11OZ", Quantity: 2},
		{SKUCode: "CANVAS-16X20", Quantity: 3},
		{SKUCode: "POSTER-A3", Quantity: 1, CancellationStatus: models.CancellationSeller},
	}
	v := toSellerView(o, false, true)

	want := []SellerSKULine{{SKUCode: "CANVAS-16X20", Quantity: 4}, {SKUCode: "MUG-11OZ", Quantity: 2}}
	if len(v.SKUs) != len(want) {
		t.Fatalf("skus = %+v, want %+v", v.SKUs, want)
	}
	for i := range want {
		if v.SKUs[i] != want[i] {
			t.Errorf("skus[%d] = %+v, want %+v", i, v.SKUs[i], want[i])
		}
	}
	if v.ItemCount != 3 {
		t.Errorf("item_count = %d, want 3", v.ItemCount)
	}
	if v.Account != "AM399" {
		t.Errorf("account = %q, want AM399", v.Account)
	}
}

// Cancelling a whole order cascades onto every line. The list must still say what
// the order held — a seller billed for a cancelled order has to recognise it.
func TestToSellerView_CancelledOrderKeepsSKUs(t *testing.T) {
	o := shippingOrder()
	o.ReviewStatus = models.ReviewCancelled
	o.CancellationStatus = models.CancellationSeller
	o.Items = []models.OrderItem{
		{SKUCode: "CANVAS-16X20", Quantity: 2, CancellationStatus: models.CancellationSeller},
		{SKUCode: "MUG-11OZ", Quantity: 1, CancellationStatus: models.CancellationSeller},
	}
	v := toSellerView(o, false, false)

	want := []SellerSKULine{{SKUCode: "CANVAS-16X20", Quantity: 2}, {SKUCode: "MUG-11OZ", Quantity: 1}}
	if len(v.SKUs) != len(want) || v.SKUs[0] != want[0] || v.SKUs[1] != want[1] {
		t.Errorf("skus = %+v, want %+v", v.SKUs, want)
	}
}
