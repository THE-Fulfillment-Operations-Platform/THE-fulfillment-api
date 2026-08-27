package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

// TestDesignFileName_PrefixesSKU pins the naming rule: a design file is named
// SKU_INTERNALCODE_QUANTITY — SKU first so the extracted folder sorts every file
// of one SKU together, internal code + quantity after so a designer opening it
// still sees which order line it belongs to. The internal code's "/" (e.g.
// 100001_1/3) must be sanitized to "-" so it never splits the file across
// sub-folders inside the ZIP.
func TestDesignFileName_PrefixesSKU(t *testing.T) {
	used := map[string]int{}

	got := designFileName("", "100001_1/3", "BR-SH-2-KEP", 2, models.DesignSideSingle, "x/y.png", used)
	want := "BR-SH-2-KEP_100001_1-3_2.png"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Two files that would resolve to the same name (same internal code + SKU + side,
// e.g. a duplicated URL) must not overwrite each other — usedNames appends -2, -3…
func TestDesignFileName_DedupsCollisions(t *testing.T) {
	used := map[string]int{}

	first := designFileName("", "100001_1/1", "AAA", 1, models.DesignSideSingle, "a.png", used)
	second := designFileName("", "100001_1/1", "AAA", 1, models.DesignSideSingle, "b.png", used)

	if first != "AAA_100001_1-1_1.png" {
		t.Errorf("first: got %q", first)
	}
	if second != "AAA_100001_1-1_1-2.png" {
		t.Errorf("second should be de-duplicated: got %q, want AAA_100001_1-1_1-2.png", second)
	}
}

// Both sides of one item share the same SKU_INTERNALCODE prefix (it is one
// physical line) and differ only by the _FRONT/_BACK suffix; the folder prefixes
// both.
func TestDesignFileName_SidesShareInternalCode(t *testing.T) {
	used := map[string]int{}

	front := designFileName("Batch_101001", "100001_2/3", "WDHWB-10IN", 2, models.DesignSideFront, "f.pdf", used)
	back := designFileName("Batch_101001", "100001_2/3", "WDHWB-10IN", 2, models.DesignSideBack, "b.pdf", used)

	if front != "Batch_101001/WDHWB-10IN_100001_2-3_2_FRONT.pdf" {
		t.Errorf("front: got %q", front)
	}
	if back != "Batch_101001/WDHWB-10IN_100001_2-3_2_BACK.pdf" {
		t.Errorf("back: got %q", back)
	}
}

// The ZIP's entry order must match the order the extracted folder sorts in —
// by SKU, then by internal code — so browsing the archive before extracting shows
// one SKU's files together instead of the query's newest-first order.
func TestSortDesignItemsBySKU(t *testing.T) {
	items := []*models.OrderItem{
		{InternalCode: "100004_1/1", SKUCode: "WDHWB-10IN"},
		{InternalCode: "100002_1/1", SKUCode: "BR-SH-2-KEP"},
		{InternalCode: "100001_1/1", SKUCode: "WDHWB-10IN"},
		{InternalCode: "100003_1/1", SKUCode: "BR-SH-2-KEP"},
	}

	sortDesignItemsBySKU(items)

	want := []string{"100002_1/1", "100003_1/1", "100001_1/1", "100004_1/1"}
	for i, code := range want {
		if items[i].InternalCode != code {
			t.Fatalf("position %d: got %s/%s, want internal code %s",
				i, items[i].SKUCode, items[i].InternalCode, code)
		}
	}
}
