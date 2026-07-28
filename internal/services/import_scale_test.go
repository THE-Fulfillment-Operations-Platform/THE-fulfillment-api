package services

import (
	"strconv"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// TestImport_ScalesToThousandsOfOrders is the round-trip guard for the order
// import. The commit used to create one order at a time — a sequence reservation,
// an INSERT, a code stamp, then the item/asset/note inserts, five to six
// statements PER ORDER. A thousand-order file therefore meant thousands of
// round-trips, which against a database in another region is tens of minutes.
// Everything now goes out in batches, so the statement count grows with batches,
// not with rows — while every guarantee (unique codes, per-day sequence, items,
// assets, attention notes) still holds.
func TestImport_ScalesToThousandsOfOrders(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	const orders = 1000
	rows := make([]ImportRow, 0, orders)
	for i := 0; i < orders; i++ {
		rows = append(rows, row("BULK-"+strconv.Itoa(i), "IMG"+strconv.Itoa(i)))
	}

	prev, err := svc.Preview(actor, 1, "XLSX", "bulk.xlsx", rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.ValidRows != orders || prev.ErrorRows != 0 {
		t.Fatalf("preview = %d valid / %d errors, want %d/0", prev.ValidRows, prev.ErrorRows, orders)
	}

	stmts := 0
	count := func(*gorm.DB) { stmts++ }
	for _, reg := range []func(string, func(*gorm.DB)) error{
		db.Callback().Query().After("gorm:query").Register,
		db.Callback().Create().After("gorm:create").Register,
		db.Callback().Update().After("gorm:update").Register,
		db.Callback().Row().After("gorm:row").Register,
		db.Callback().Raw().After("gorm:raw").Register,
	} {
		if err := reg("test:count", count); err != nil {
			t.Fatalf("register callback: %v", err)
		}
	}

	job, err := svc.Commit(actor, prev.ImportJobID)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if job.CreatedCount != orders {
		t.Fatalf("created %d orders, want %d", job.CreatedCount, orders)
	}
	// 1 job read + 1 SKU lookup + 1 sequence reservation + batched inserts/updates
	// for orders, codes, items and assets (200 per batch) + 1 job save. Nowhere
	// near the ~5000 an order-at-a-time commit would issue.
	if stmts > 40 {
		t.Fatalf("committing %d orders issued %d statements, want batched writes", orders, stmts)
	}

	// Every order must still hold a distinct, id-derived internal code and its own
	// per-day sequence — no placeholder may survive the transaction.
	var saved []models.Order
	if err := db.Order("id asc").Find(&saved).Error; err != nil {
		t.Fatalf("load orders: %v", err)
	}
	if len(saved) != orders {
		t.Fatalf("stored %d orders, want %d", len(saved), orders)
	}
	codes := make(map[string]bool, orders)
	seqs := make(map[int]bool, orders)
	for _, o := range saved {
		if o.InternalCode == "" || o.InternalCode[:3] == "TMP" {
			t.Fatalf("order %d kept a placeholder code %q", o.ID, o.InternalCode)
		}
		if o.InternalCode != internalBaseCode(o.ID) {
			t.Fatalf("order %d code = %q, want %q", o.ID, o.InternalCode, internalBaseCode(o.ID))
		}
		if codes[o.InternalCode] {
			t.Fatalf("duplicate internal code %q", o.InternalCode)
		}
		codes[o.InternalCode] = true
		if o.DailySeq < 1 || seqs[o.DailySeq] {
			t.Fatalf("order %d has a bad/duplicate daily seq %d", o.ID, o.DailySeq)
		}
		seqs[o.DailySeq] = true
	}

	var items []models.OrderItem
	if err := db.Find(&items).Error; err != nil {
		t.Fatalf("load items: %v", err)
	}
	if len(items) != orders {
		t.Fatalf("stored %d items, want %d", len(items), orders)
	}
	for _, it := range items {
		if it.InternalCode != itemInternalCode(it.OrderID, 1, 1) {
			t.Fatalf("item %d code = %q, want %q", it.ID, it.InternalCode, itemInternalCode(it.OrderID, 1, 1))
		}
	}
	// Every row here carries a mockup, so each item records exactly one asset and
	// no attention note is raised.
	var assetCount, noteCount int64
	db.Model(&models.ItemAsset{}).Count(&assetCount)
	db.Model(&models.Note{}).Count(&noteCount)
	if assetCount != orders || noteCount != 0 {
		t.Fatalf("assets=%d notes=%d, want %d/0", assetCount, noteCount, orders)
	}
}
