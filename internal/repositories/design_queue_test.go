package repositories

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

func newQueueTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{},
		&models.Batch{}, &models.BatchItem{}, &models.BatchLink{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedQueueItem creates an approved order carrying one item in the given design
// state and returns the item.
func seedQueueItem(t *testing.T, db *gorm.DB, code string, status models.DesignStatus) *models.OrderItem {
	t.Helper()
	order := &models.Order{
		InternalCode: "ORD-" + code,
		StoreOrderID: "SO-" + code,
		SellerID:     1,
		ReviewStatus: models.ReviewApproved,
		SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order %s: %v", code, err)
	}
	item := &models.OrderItem{
		OrderID:      order.ID,
		InternalCode: code,
		SKUCode:      "SKU-1",
		Quantity:     1,
		MockupURL:    "https://example.com/mockup.png",
		DesignStatus: status,
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("seed item %s: %v", code, err)
	}
	return item
}

// batchItems schedules items into a new batch and returns the batch.
func batchItems(t *testing.T, db *gorm.DB, code string, items ...*models.OrderItem) *models.Batch {
	t.Helper()
	batch := &models.Batch{Code: code, MaterialID: 1, Status: models.StatusPending}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch %s: %v", code, err)
	}
	for _, it := range items {
		bi := &models.BatchItem{BatchID: batch.ID, OrderItemID: it.ID, MaterialID: 1, Status: models.StatusPending}
		if err := db.Create(bi).Error; err != nil {
			t.Fatalf("seed batch item: %v", err)
		}
	}
	return batch
}

func queueCodes(t *testing.T, repo *Repositories) map[string]bool {
	t.Helper()
	rows, _, err := repo.OrderItem.List(ItemFilter{
		Page:           Page{Page: 1, PageSize: 50},
		NeedsDesign:    true,
		ReviewApproved: true,
	})
	if err != nil {
		t.Fatalf("list design queue: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.InternalCode] = true
	}
	return got
}

// TestDesignQueue_KeepsBatchedItemsUntilBatchHasBothLinks locks in the flow fix for
// the batch-level production files. Files are ganged per batch (many designs on one
// sheet) and attached to the batch, so an item reaching READY does not mean the
// design work is done — its batch still owes BOTH a print file and a cut file (every
// product here needs both). The queue must keep showing batched items until their
// batch carries both links, which is what lets a designer filter by batch/material
// and clear a whole batch at once. A batch with only one of the two links is still
// outstanding; only when both are set do its rows drop, or the queue never drains.
func TestDesignQueue_KeepsBatchedItemsUntilBatchHasBothLinks(t *testing.T) {
	db := newQueueTestDB(t)
	repo := New(db)

	pending := seedQueueItem(t, db, "ITEM-PENDING", models.DesignPending)
	awaiting := seedQueueItem(t, db, "ITEM-AWAITING", models.DesignReady)
	linked := seedQueueItem(t, db, "ITEM-LINKED", models.DesignReady)
	loose := seedQueueItem(t, db, "ITEM-READY-UNBATCHED", models.DesignReady)
	_ = loose

	batchItems(t, db, "#100001", awaiting)
	withLinks := batchItems(t, db, "#100002", linked)

	addLink := func(batchID uint, kind models.BatchLinkKind) {
		t.Helper()
		l := &models.BatchLink{BatchID: batchID, Kind: kind, URL: "https://example.com/" + string(kind) + ".pdf"}
		if err := db.Create(l).Error; err != nil {
			t.Fatalf("seed %s link: %v", kind, err)
		}
	}

	// Before any link exists both batched items are outstanding work.
	got := queueCodes(t, repo)
	if !got[pending.InternalCode] {
		t.Errorf("unfinished item %s must stay in the queue", pending.InternalCode)
	}
	if !got[awaiting.InternalCode] || !got[linked.InternalCode] {
		t.Errorf("item in a batch without both links must stay in the queue")
	}
	// A READY item that was never batched has nothing left for the designer to do
	// and no batch to gang it into, so it must not linger in the queue.
	if got["ITEM-READY-UNBATCHED"] {
		t.Errorf("ready, unbatched item must not appear in the queue")
	}

	// Only the print link so far — the batch still owes a cut file, so its item stays.
	addLink(withLinks.ID, models.BatchLinkPrint)
	got = queueCodes(t, repo)
	if !got[linked.InternalCode] {
		t.Errorf("item must stay in the queue while its batch still lacks the cut link")
	}

	// Both links present now — the item is finally done and drops off.
	addLink(withLinks.ID, models.BatchLinkCut)
	got = queueCodes(t, repo)
	if got[linked.InternalCode] {
		t.Errorf("item must leave the queue once its batch has both links")
	}
	if !got[awaiting.InternalCode] {
		t.Errorf("item in the still-unlinked batch must remain in the queue")
	}
	if !got[pending.InternalCode] {
		t.Errorf("unfinished item must be unaffected by another batch's links")
	}
}

// TestDesignQueue_FilterByMaterial covers "gom theo NVL": the queue can be narrowed
// to items whose SKU is mapped to one material, so the designer sees exactly the
// orders that could go onto that material's sheet — including ones not yet batched.
func TestDesignQueue_FilterByMaterial(t *testing.T) {
	db := newQueueTestDB(t)
	repo := New(db)

	wood := &models.Material{Code: "WOOD-3MM", Name: "Gỗ 3mm"}
	mica := &models.Material{Code: "MICA-2MM", Name: "Mica 2mm"}
	if err := db.Create(wood).Error; err != nil {
		t.Fatalf("seed wood: %v", err)
	}
	if err := db.Create(mica).Error; err != nil {
		t.Fatalf("seed mica: %v", err)
	}
	woodSKU := &models.SKU{Code: "SKU-WOOD", Name: "Wood sign"}
	micaSKU := &models.SKU{Code: "SKU-MICA", Name: "Mica plate"}
	if err := db.Create(woodSKU).Error; err != nil {
		t.Fatalf("seed wood sku: %v", err)
	}
	if err := db.Create(micaSKU).Error; err != nil {
		t.Fatalf("seed mica sku: %v", err)
	}
	if err := db.Create(&models.SKUMaterial{SKUID: woodSKU.ID, MaterialID: wood.ID, QuantityPerUnit: 1}).Error; err != nil {
		t.Fatalf("map wood: %v", err)
	}
	if err := db.Create(&models.SKUMaterial{SKUID: micaSKU.ID, MaterialID: mica.ID, QuantityPerUnit: 1}).Error; err != nil {
		t.Fatalf("map mica: %v", err)
	}

	woodItem := seedQueueItem(t, db, "ITEM-WOOD", models.DesignPending)
	woodItem.SKUID = &woodSKU.ID
	if err := db.Save(woodItem).Error; err != nil {
		t.Fatalf("attach wood sku: %v", err)
	}
	micaItem := seedQueueItem(t, db, "ITEM-MICA", models.DesignPending)
	micaItem.SKUID = &micaSKU.ID
	if err := db.Save(micaItem).Error; err != nil {
		t.Fatalf("attach mica sku: %v", err)
	}

	rows, _, err := repo.OrderItem.List(ItemFilter{
		Page:           Page{Page: 1, PageSize: 50},
		NeedsDesign:    true,
		ReviewApproved: true,
		MaterialID:     &wood.ID,
	})
	if err != nil {
		t.Fatalf("list by material: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.InternalCode] = true
	}
	if !got["ITEM-WOOD"] {
		t.Errorf("wood item must appear when filtering by wood material")
	}
	if got["ITEM-MICA"] {
		t.Errorf("mica item must be excluded when filtering by wood material")
	}

	// The NVL dropdown is built from this: only materials with work in the queue,
	// never the whole catalog. An unused material must not be offered at all.
	unused := &models.Material{Code: "UNUSED-NVL", Name: "Không dùng"}
	if err := db.Create(unused).Error; err != nil {
		t.Fatalf("seed unused material: %v", err)
	}
	facets, err := repo.OrderItem.DesignQueueMaterials(ItemFilter{
		Page: Page{Page: 1, PageSize: 50}, NeedsDesign: true, ReviewApproved: true,
	})
	if err != nil {
		t.Fatalf("queue materials: %v", err)
	}
	counts := map[string]int64{}
	for _, f := range facets {
		counts[f.MaterialCode] = f.ItemCount
	}
	if counts["WOOD-3MM"] != 1 || counts["MICA-2MM"] != 1 {
		t.Errorf("facet counts = %v, want one item each for wood and mica", counts)
	}
	if _, offered := counts["UNUSED-NVL"]; offered {
		t.Errorf("a material with no queue items must not be offered in the filter")
	}
}

// TestSetProductionFileForBatch_StampsEveryLiveItem covers the fan-out that makes
// "same batch → same print file" true for the per-item columns the production
// export and QC read. Cancelled lines are never produced, so they must be skipped.
func TestSetProductionFileForBatch_StampsEveryLiveItem(t *testing.T) {
	db := newQueueTestDB(t)
	repo := New(db)

	a := seedQueueItem(t, db, "A", models.DesignReady)
	b := seedQueueItem(t, db, "B", models.DesignReady)
	cancelled := seedQueueItem(t, db, "C", models.DesignReady)
	outside := seedQueueItem(t, db, "D", models.DesignReady)

	cancelled.CancellationStatus = models.CancellationApproved
	if err := db.Save(cancelled).Error; err != nil {
		t.Fatalf("cancel item: %v", err)
	}

	batch := batchItems(t, db, "#100001", a, b, cancelled)
	batchItems(t, db, "#100002", outside)

	const url = "https://example.com/print.pdf"
	n, err := repo.OrderItem.SetProductionFileForBatch(batch.ID, "print_file_url", url)
	if err != nil {
		t.Fatalf("stamp print file: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 live items stamped, got %d", n)
	}

	check := func(id uint, want string) {
		t.Helper()
		var got models.OrderItem
		if err := db.First(&got, id).Error; err != nil {
			t.Fatalf("reload item %d: %v", id, err)
		}
		if got.PrintFileURL != want {
			t.Errorf("item %d print_file_url = %q, want %q", id, got.PrintFileURL, want)
		}
	}
	check(a.ID, url)
	check(b.ID, url)
	check(cancelled.ID, "") // cancelled line is never produced
	check(outside.ID, "")   // another batch must not be touched
}
