package repositories

import (
	"testing"
	"time"

	"the-fulfillment/backend/internal/models"
)

// "Đã có file SX" is derived, not stored: PENDING + not closed + both links.
// The list filter must split PENDING exactly the way the badge does, server-side,
// so pagination and totals stay right.
func TestBatchList_FilesFilter(t *testing.T) {
	db := newTestDB(t)
	if err := db.AutoMigrate(&models.Batch{}, &models.BatchItem{}, &models.BatchLink{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mk := func(code string, status models.InternalStatus, links []models.BatchLinkKind, closed bool) {
		b := &models.Batch{Code: code, MaterialID: 1, Status: status}
		if closed {
			now := time.Now()
			b.ClosedAt = &now
		}
		if err := db.Create(b).Error; err != nil {
			t.Fatalf("seed %s: %v", code, err)
		}
		for _, k := range links {
			db.Create(&models.BatchLink{BatchID: b.ID, Kind: k, URL: "https://x/" + code + string(k), LinkUpdatedAt: time.Now()})
		}
	}
	both := []models.BatchLinkKind{models.BatchLinkPrint, models.BatchLinkCut}
	mk("#READY", models.StatusPending, both, false)
	mk("#PRINT-ONLY", models.StatusPending, []models.BatchLinkKind{models.BatchLinkPrint}, false)
	mk("#NONE", models.StatusPending, nil, false)
	mk("#CLOSED", models.StatusPending, both, true)
	mk("#PRINTED", models.StatusPrinted, both, false)
	// A deleted link does not count.
	mk("#DELETED-LINK", models.StatusPending, both, false)
	db.Where("url LIKE ?", "%#DELETED-LINKCUT").Delete(&models.BatchLink{})

	repo := &BatchRepository{db: db}
	codes := func(f BatchFilter) []string {
		f.Page = Page{Page: 1, PageSize: 50}
		rows, total, err := repo.List(f)
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		out := []string{}
		for _, r := range rows {
			out = append(out, r.Code)
		}
		if int(total) != len(out) {
			t.Errorf("total %d != rows %d", total, len(out))
		}
		return out
	}
	if got := codes(BatchFilter{Status: "PENDING", Files: "ready"}); len(got) != 1 || got[0] != "#READY" {
		t.Errorf("ready = %v", got)
	}
	got := codes(BatchFilter{Status: "PENDING", Files: "missing"})
	want := map[string]bool{"#PRINT-ONLY": true, "#NONE": true, "#CLOSED": true, "#DELETED-LINK": true}
	if len(got) != len(want) {
		t.Errorf("missing = %v", got)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("missing should not contain %s", c)
		}
	}
	if got := codes(BatchFilter{Status: "PENDING"}); len(got) != 5 {
		t.Errorf("plain PENDING (dashboard) = %v, want all 5", got)
	}
}
