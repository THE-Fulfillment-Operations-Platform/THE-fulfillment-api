package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

// TestDesignSeq_RunsOneTwoThree pins the STT rule: the number prefixing a design
// file counts position within the archive (1, 2, 3, …), not the order's DailySeq
// — which numbers the whole day and so jumps (001, 038, 040) when only some
// orders are in the pull.
func TestDesignSeq_RunsOneTwoThree(t *testing.T) {
	var seq designSeq
	used := map[string]int{}

	got := make([]string, 0, 3)
	for _, sku := range []string{"BR-SH-2-KEP", "BR-A-1-6-KEP", "NL-R-9"} {
		got = append(got, designFileName("", seq.next(), sku, 1, models.DesignSideSingle, "x/y.png", used))
		seq.commit()
	}

	want := []string{
		"001_BR-SH-2-KEP_1.png",
		"002_BR-A-1-6-KEP_1.png",
		"003_NL-R-9_1.png",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// An item whose every design URL fails to download must not burn an STT, or the
// numbering in the delivered ZIP would read 001, 003, 004 with no 002 present.
func TestDesignSeq_SkippedItemLeavesNoGap(t *testing.T) {
	var seq designSeq
	used := map[string]int{}

	first := designFileName("", seq.next(), "AAA", 1, models.DesignSideSingle, "a.png", used)
	seq.commit()

	// Second item: name computed, but nothing written — no commit.
	_ = designFileName("", seq.next(), "BBB", 1, models.DesignSideSingle, "b.png", used)

	third := designFileName("", seq.next(), "CCC", 1, models.DesignSideSingle, "c.png", used)
	seq.commit()

	if first != "001_AAA_1.png" {
		t.Errorf("first: got %q", first)
	}
	if third != "002_CCC_1.png" {
		t.Errorf("third should reuse the skipped number: got %q, want 002_CCC_1.png", third)
	}
}

// Both sides of one item are one physical line, so they share an STT and differ
// only by the _FRONT/_BACK suffix.
func TestDesignFileName_SidesShareSeq(t *testing.T) {
	var seq designSeq
	used := map[string]int{}

	front := designFileName("Batch_101001", seq.next(), "WDHWB-10IN", 2, models.DesignSideFront, "f.pdf", used)
	back := designFileName("Batch_101001", seq.next(), "WDHWB-10IN", 2, models.DesignSideBack, "b.pdf", used)
	seq.commit()

	if front != "Batch_101001/001_WDHWB-10IN_2_FRONT.pdf" {
		t.Errorf("front: got %q", front)
	}
	if back != "Batch_101001/001_WDHWB-10IN_2_BACK.pdf" {
		t.Errorf("back: got %q", back)
	}
}
