package repositories

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// ---------------------------------------------------------------------------
// Every Preload on a list query is a separate round trip to Postgres, and this
// deployment's Postgres is an internet hop away (~100 ms). Loading associations
// no screen renders is therefore not a style problem — it is most of the wait.
//
// These tests COUNT the queries, because that is the thing that actually costs
// money. Asserting only "SKU is nil" would still pass if someone re-added a
// preload whose result got dropped later.
// ---------------------------------------------------------------------------

// countQueries wraps a DB so every SELECT it issues increments a counter.
func countQueries(t *testing.T, db *gorm.DB) (*gorm.DB, func() int) {
	t.Helper()
	n := 0
	// A fresh session so the callback lives on this handle only and cannot leak
	// into other tests sharing the same in-memory DB.
	sess := db.Session(&gorm.Session{NewDB: true})
	err := sess.Callback().Query().After("gorm:query").Register("test:count", func(*gorm.DB) { n++ })
	if err != nil {
		t.Fatalf("register callback: %v", err)
	}
	// Row-count queries go through the Row callback, not Query.
	err = sess.Callback().Row().After("gorm:row").Register("test:count_row", func(*gorm.DB) { n++ })
	if err != nil {
		t.Fatalf("register row callback: %v", err)
	}
	return sess, func() int { return n }
}

func seedItemWithBatchAndSKU(t *testing.T, db *gorm.DB) {
	t.Helper()
	mat := &models.Material{Code: "MICA", Name: "Mica 3ly"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	sku := &models.SKU{Code: "SKU-1", Name: "SKU 1", ProductName: "SKU 1"}
	if err := db.Create(sku).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}
	if err := db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mat.ID, QuantityPerUnit: 1}).Error; err != nil {
		t.Fatalf("seed sku-material: %v", err)
	}
	item := seedQueueItem(t, db, "100001_1/1", models.DesignReady)
	item.SKUID = &sku.ID
	if err := db.Save(item).Error; err != nil {
		t.Fatalf("link sku: %v", err)
	}
	batchItems(t, db, "B-1", item)
}

// The Orders/Items screen renders nothing off `sku` and nothing off the order's
// seller. Both used to be preloaded anyway — four round trips per request thrown
// away, on top of two more spent fetching batch/material separately.
func TestItemList_LeanByDefault(t *testing.T) {
	db := newQueueTestDB(t)
	seedItemWithBatchAndSKU(t, db)

	counted, queries := countQueries(t, db)
	repo := &OrderItemRepository{db: counted}
	rows, total, err := repo.List(ItemFilter{Page: Page{Page: 1, PageSize: 20}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("want 1 row, got total=%d len=%d", total, len(rows))
	}

	// COUNT + items + Order + batch-items-with-joins. Was 10 before this: the old
	// version also loaded Order.Seller and the whole SKU→materials chain (unused
	// here), and fetched Batch and Material as two more separate trips.
	const wantLean = 4
	if got := queries(); got != wantLean {
		t.Errorf("lean list issued %d queries, want %d — a preload was added back", got, wantLean)
	}

	// What the screen DOES read must still be there.
	if rows[0].Order == nil {
		t.Error("Order not loaded — the list shows store order id, status and dates from it")
	}
	if len(rows[0].BatchItems) != 1 {
		t.Fatalf("BatchItems not loaded: %d", len(rows[0].BatchItems))
	}
	if rows[0].BatchItems[0].Batch == nil || rows[0].BatchItems[0].Material == nil {
		t.Error("batch/material not loaded — the NVL and Batch columns read them")
	}
	// What it does NOT read must be absent.
	if rows[0].SKU != nil {
		t.Error("SKU still preloaded on the lean path")
	}
	// Seller is a value field, so "not loaded" shows up as the zero struct.
	if rows[0].Order.Seller.ID != 0 {
		t.Error("Order.Seller still preloaded on the lean path")
	}
}

// The design queue is the one caller that groups by NVL from the SKU's bill of
// materials, so it opts in — and pays exactly three more round trips for it.
func TestItemList_DesignQueueOptsIntoSKUMaterials(t *testing.T) {
	db := newQueueTestDB(t)
	seedItemWithBatchAndSKU(t, db)

	counted, queries := countQueries(t, db)
	repo := &OrderItemRepository{db: counted}
	rows, _, err := repo.List(ItemFilter{
		Page:             Page{Page: 1, PageSize: 20},
		WithSKUMaterials: true,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// SKU + SKU.Materials + SKU.Materials.Material on top of the lean four.
	const wantWithSKU = 7
	if got := queries(); got != wantWithSKU {
		t.Errorf("design-queue list issued %d queries, want %d", got, wantWithSKU)
	}
	if rows[0].SKU == nil {
		t.Fatal("SKU not loaded despite WithSKUMaterials")
	}
	if len(rows[0].SKU.Materials) != 1 || rows[0].SKU.Materials[0].Material.ID == 0 {
		t.Error("SKU material chain not loaded — the NVL grouping filter reads it")
	}
}

func TestItemList_SellerIsOptIn(t *testing.T) {
	db := newQueueTestDB(t)
	if err := db.Create(&models.Seller{Code: "S1", Name: "Seller One"}).Error; err != nil {
		t.Fatalf("seed seller: %v", err)
	}
	seedItemWithBatchAndSKU(t, db)

	counted, queries := countQueries(t, db)
	repo := &OrderItemRepository{db: counted}
	rows, _, err := repo.List(ItemFilter{
		Page:       Page{Page: 1, PageSize: 20},
		WithSeller: true,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	const wantWithSeller = 5 // lean four + Order.Seller
	if got := queries(); got != wantWithSeller {
		t.Errorf("list with seller issued %d queries, want %d", got, wantWithSeller)
	}
	if rows[0].Order == nil || rows[0].Order.Seller.ID == 0 {
		t.Error("Seller not loaded despite WithSeller")
	}
}
