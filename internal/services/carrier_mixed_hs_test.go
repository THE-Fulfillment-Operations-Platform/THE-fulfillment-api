package services

import "testing"

// #6323: a parcel with a postcard and a mica sign is declared as the mica sign,
// even when the postcard line is worth more; all-postcard parcels stay postcards.
func TestIsPostcardHS(t *testing.T) {
	for hs, want := range map[string]bool{
		"4909002000347": true, "4909002000": true, " 4909002000347 ": true,
		"3926400090307": false, "9503000090301": false, "": false,
	} {
		if got := isPostcardHS(hs); got != want {
			t.Fatalf("isPostcardHS(%q) = %v, want %v", hs, got, want)
		}
	}
}
