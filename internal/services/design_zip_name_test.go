package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

// TestDesignFileName_PrefixesInternalCode pins the naming rule: a design file is
// named INTERNALCODE_SKU_QUANTITY so a designer opening it sees which order/SKU it
// belongs to. The internal code's "/" (e.g. 100001_1/3) must be sanitized to "-"
// so it never splits the file across sub-folders inside the ZIP.
func TestDesignFileName_PrefixesInternalCode(t *testing.T) {
	used := map[string]int{}

	got := designFileName("", "100001_1/3", "BR-SH-2-KEP", 2, models.DesignSideSingle, "x/y.png", used)
	want := "100001_1-3_BR-SH-2-KEP_2.png"
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

	if first != "100001_1-1_AAA_1.png" {
		t.Errorf("first: got %q", first)
	}
	if second != "100001_1-1_AAA_1-2.png" {
		t.Errorf("second should be de-duplicated: got %q, want 100001_1-1_AAA_1-2.png", second)
	}
}

// Both sides of one item share the same internal-code prefix (it is one physical
// line) and differ only by the _FRONT/_BACK suffix; the folder prefixes both.
func TestDesignFileName_SidesShareInternalCode(t *testing.T) {
	used := map[string]int{}

	front := designFileName("Batch_101001", "100001_2/3", "WDHWB-10IN", 2, models.DesignSideFront, "f.pdf", used)
	back := designFileName("Batch_101001", "100001_2/3", "WDHWB-10IN", 2, models.DesignSideBack, "b.pdf", used)

	if front != "Batch_101001/100001_2-3_WDHWB-10IN_2_FRONT.pdf" {
		t.Errorf("front: got %q", front)
	}
	if back != "Batch_101001/100001_2-3_WDHWB-10IN_2_BACK.pdf" {
		t.Errorf("back: got %q", back)
	}
}
