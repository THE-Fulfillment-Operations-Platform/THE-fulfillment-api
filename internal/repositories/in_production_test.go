package repositories

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// seedOrderWithItem creates one order carrying a single line in the given state
// and returns the order id.
func seedOrderWithItem(t *testing.T, db *gorm.DB, code string, status models.InternalStatus, cancellation models.CancellationStatus, batched bool) uint {
	t.Helper()
	order := &models.Order{
		InternalCode: "ORD-" + code, StoreOrderID: "SO-" + code, SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order %s: %v", code, err)
	}
	item := &models.OrderItem{
		OrderID: order.ID, InternalCode: "ITM-" + code, SKUCode: "SKU-" + code, Quantity: 1,
		InternalStatus: status, DesignStatus: models.DesignPending, CancellationStatus: cancellation,
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("seed item %s: %v", code, err)
	}
	if batched {
		if err := db.Create(&models.BatchItem{
			BatchID: 1, OrderItemID: item.ID, MaterialID: 1, Status: models.StatusPending,
		}).Error; err != nil {
			t.Fatalf("seed batch item %s: %v", code, err)
		}
	}
	return order.ID
}

// TestInProductionIDs covers the query behind the seller-facing cancel buttons:
// an order with work in flight may only be cancelled with ops approval (and is
// still billed), so getting this set wrong either hides the button or hands out
// a free cancellation on an order the factory already paid for.
func TestInProductionIDs(t *testing.T) {
	db := newQueueTestDB(t)
	repo := &OrderRepository{db: db}

	untouched := seedOrderWithItem(t, db, "A", models.StatusPending, models.CancellationNone, false)
	// Batched but not started: the material is committed, so this counts.
	batched := seedOrderWithItem(t, db, "B", models.StatusPending, models.CancellationNone, true)
	printed := seedOrderWithItem(t, db, "C", models.StatusPrinted, models.CancellationNone, false)
	// A cancelled line is history — it must not make its order look busy.
	cancelled := seedOrderWithItem(t, db, "D", models.StatusPrinted, models.CancellationApproved, true)

	got, err := repo.InProductionIDs([]uint{untouched, batched, printed, cancelled})
	if err != nil {
		t.Fatalf("InProductionIDs: %v", err)
	}
	for id, want := range map[uint]bool{
		untouched: false, batched: true, printed: true, cancelled: false,
	} {
		if got[id] != want {
			t.Fatalf("order %d: got in-production %v, want %v", id, got[id], want)
		}
	}

	empty, err := repo.InProductionIDs(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty input: got %v / %v", empty, err)
	}
}

// TestItemListIncludeCancelled pins the escape hatch on the item list. Cancelling
// an order cancels every line in it, and cancelled lines are hidden from the list
// because they are not work — so without IncludeCancelled, asking the order screen
// for cancelled orders answers "none", whatever is in the database.
func TestItemListIncludeCancelled(t *testing.T) {
	db := newQueueTestDB(t)
	repo := &OrderItemRepository{db: db}

	seedOrderWithItem(t, db, "LIVE", models.StatusPending, models.CancellationNone, false)
	seedOrderWithItem(t, db, "GONE", models.StatusPrinted, models.CancellationApproved, false)

	rows, total, err := repo.List(ItemFilter{Page: Page{Page: 1, PageSize: 50}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].InternalCode != "ITM-LIVE" {
		t.Fatalf("work list must hide cancelled lines, got %d rows (total %d)", len(rows), total)
	}

	rows, total, err = repo.List(ItemFilter{Page: Page{Page: 1, PageSize: 50}, IncludeCancelled: true})
	if err != nil {
		t.Fatalf("list (include cancelled): %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("history list must show cancelled lines, got %d rows (total %d)", len(rows), total)
	}
}
