package services

import "testing"

// The swap that actually happened in production: order 100001 arrived with
// ShippingZip="MN" and ShippingProvince="55112", and nothing noticed — both
// columns are free text and go straight onto the shipping label.
//
// The detector guesses from the shape of the strings, so the bar it has to clear
// is NOT "catches every swap" — it is "never cries wolf on a legitimate address".
// A false positive on every Canadian order would train staff to ignore the
// warning, which is worse than not having it.
func TestZipStateSwapped(t *testing.T) {
	cases := []struct {
		name     string
		zip      string
		province string
		want     bool
	}{
		// --- the real bug ---
		{"US state in the zip column", "MN", "55112", true},
		{"lowercase state code", "mn", "55112", true},
		{"zip+4 in the province column", "TX", "73301-1234", true},
		{"padded by spreadsheet export", " CA ", " 90210 ", true},

		// --- correct US addresses: must stay silent ---
		{"correct US", "55112", "MN", false},
		{"correct US zip+4", "73301-1234", "TX", false},

		// --- other countries: the same columns, different shapes ---
		// Canadian postal codes carry letters but are never two characters.
		{"canada", "K1A 0B1", "ON", false},
		// The UK has no state; province is empty, so there is nothing to swap with.
		{"uk", "SW1A 1AA", "", false},
		// Province written out in full is not a zip, whatever the zip looks like.
		{"vietnam full province name", "70000", "Hồ Chí Minh", false},
		// Two-letter zip but the province is a name, not digits — not a swap.
		{"two letters but province is a name", "XX", "Minnesota", false},

		// --- empty / partial rows must not warn ---
		{"both empty", "", "", false},
		{"zip only", "55112", "", false},
		{"province only", "", "MN", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := zipStateSwapped(tc.zip, tc.province); got != tc.want {
				t.Errorf("zipStateSwapped(%q, %q) = %v, want %v", tc.zip, tc.province, got, tc.want)
			}
		})
	}
}
