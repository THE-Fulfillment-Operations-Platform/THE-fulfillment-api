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
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusHandedOff,
		ShippingName:     "McKenna Garland",
		ShippingAddress1: "49 14th Ave SW",
		ShippingCity:     "NEW BRIGHTON",
		ShippingProvince: "MN",
		ShippingZip:      "55112",
		ShippingCountry:  "US",
		ShippingPhone:    "1122334455",
		ShippingEmail:    "nghiatesst@gmail.com",
		ShippingMethod:   "normal",
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
		{"shipping_email", v.ShippingEmail, "nghiatesst@gmail.com"},
		{"shipping_method", v.ShippingMethod, "normal"},
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

	if v.ShippingName != "" || v.ShippingAddress1 != "" || v.ShippingEmail != "" {
		t.Errorf("list view leaked recipient fields: %+v", v)
	}
	// omitempty must actually drop the keys, not send them as empty strings.
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"shipping_name", "shipping_address1", "shipping_email", "note"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("list payload still contains %q: %s", key, raw)
		}
	}
}
