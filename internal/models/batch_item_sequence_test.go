package models

import (
	"testing"
	"time"
)

func TestNumberBatchItems(t *testing.T) {
	mk := func(id uint, cancel CancellationStatus) BatchItem {
		bi := BatchItem{OrderItem: &OrderItem{CancellationStatus: cancel}}
		bi.ID = id
		return bi
	}
	items := []BatchItem{
		mk(7, CancellationNone),
		mk(3, CancellationNone),
		mk(5, CancellationApproved), // dòng đã huỷ: không số, không chiếm số
		mk(9, CancellationRequested),
		{}, // không preload OrderItem: vẫn đánh số
	}
	items[4].ID = 11
	NumberBatchItems(items)

	wantID := []uint{3, 5, 7, 9, 11}
	wantSeq := []int{1, 0, 2, 3, 4}
	for i := range items {
		if items[i].ID != wantID[i] || items[i].Sequence != wantSeq[i] {
			t.Fatalf("pos %d: id=%d seq=%d, want id=%d seq=%d", i, items[i].ID, items[i].Sequence, wantID[i], wantSeq[i])
		}
	}

	// Gọi lại (hoặc lọc bớt rồi gọi) không đổi số của những phần còn nguyên.
	NumberBatchItems(items)
	if items[2].Sequence != 2 {
		t.Fatalf("renumber changed seq: %d", items[2].Sequence)
	}
}

func TestNumberBatchItems_ScrappedAndCancelledInterleaved(t *testing.T) {
	now := time.Now()
	mk := func(id uint, cancel CancellationStatus, scrapped bool) BatchItem {
		bi := BatchItem{OrderItem: &OrderItem{CancellationStatus: cancel}}
		bi.ID = id
		if scrapped {
			bi.ScrappedAt = &now
		}
		return bi
	}
	items := []BatchItem{
		mk(1, CancellationNone, false),
		mk(2, CancellationSeller, false),
		mk(3, CancellationNone, true),
		mk(4, CancellationNone, false),
		mk(5, CancellationApproved, true),
		mk(6, CancellationNone, false),
	}
	NumberBatchItems(items)
	want := []int{1, 0, 0, 2, 0, 3}
	for i := range items {
		if items[i].Sequence != want[i] {
			t.Fatalf("id %d: seq=%d want %d", items[i].ID, items[i].Sequence, want[i])
		}
	}
}
