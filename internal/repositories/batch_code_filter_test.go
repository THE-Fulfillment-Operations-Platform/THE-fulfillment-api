package repositories

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// seedBatchWithOrder creates one batch producing one order (one line) and
// returns the batch id. parentID != nil makes it a child batch.
func seedBatchWithOrder(t *testing.T, db *gorm.DB, code string, parentID *uint) uint {
	t.Helper()
	order := &models.Order{
		InternalCode: code, StoreOrderID: "SO-" + code, SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order %s: %v", code, err)
	}
	item := &models.OrderItem{
		OrderID: order.ID, InternalCode: code + "_1/1", SKUCode: "SKU-" + code, Quantity: 1,
		InternalStatus: models.StatusPending, DesignStatus: models.DesignPending,
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("seed item %s: %v", code, err)
	}
	batch := &models.Batch{Code: "#B-" + code, MaterialID: 1, Status: models.StatusPending, ParentBatchID: parentID}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch %s: %v", code, err)
	}
	if err := db.Create(&models.BatchItem{
		BatchID: batch.ID, OrderItemID: item.ID, MaterialID: 1, Status: models.StatusPending,
	}).Error; err != nil {
		t.Fatalf("seed batch item %s: %v", code, err)
	}
	return batch.ID
}

// TestBatchListCodeFilter covers the reverse lookup on the batch list: scan or
// paste an order's internal code (or one item's tem code) and get exactly the
// batch(es) producing that order — including child batches, which the default
// list hides.
func TestBatchListCodeFilter(t *testing.T) {
	db := newQueueTestDB(t)
	repo := &BatchRepository{db: db}

	flat := seedBatchWithOrder(t, db, "100001", nil)
	parent := &models.Batch{Code: "#B-PARENT", MaterialID: 1, Status: models.StatusPending}
	if err := db.Create(parent).Error; err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	child := seedBatchWithOrder(t, db, "100002", &parent.ID)

	page := Page{Page: 1, PageSize: 20}

	listIDs := func(f BatchFilter) []uint {
		t.Helper()
		rows, _, err := repo.List(f)
		if err != nil {
			t.Fatalf("List(%+v): %v", f, err)
		}
		ids := make([]uint, 0, len(rows))
		for _, b := range rows {
			ids = append(ids, b.ID)
		}
		return ids
	}

	// Order internal code and item tem code both name the flat batch, and only it.
	for _, code := range []string{"100001", "100001_1/1", " 100001 "} {
		if got := listIDs(BatchFilter{Page: page, Code: code}); len(got) != 1 || got[0] != flat {
			t.Errorf("Code=%q: got batch ids %v, want [%d]", code, got, flat)
		}
	}

	// An order produced in a child batch must surface that child even though the
	// default list hides children.
	if got := listIDs(BatchFilter{Page: page, Code: "100002"}); len(got) != 1 || got[0] != child {
		t.Errorf("Code=100002: got batch ids %v, want child [%d]", got, child)
	}

	// Unknown code: no batches, not the unfiltered list.
	if got := listIDs(BatchFilter{Page: page, Code: "999999"}); len(got) != 0 {
		t.Errorf("Code=999999: got batch ids %v, want none", got)
	}

	// No code: default child-hiding still applies (flat + parent, no child).
	got := listIDs(BatchFilter{Page: page})
	for _, id := range got {
		if id == child {
			t.Errorf("default list leaked child batch %d: %v", child, got)
		}
	}
}

// TestBatchDetailPreloadsSeller pins the nested join behind the QR label print:
// the tem shows the seller's name, which rides in via OrderItem.Order.Seller —
// a three-level join that only fails at query time, never at compile time.
func TestBatchDetailPreloadsSeller(t *testing.T) {
	db := newQueueTestDB(t)
	repo := &BatchRepository{db: db}

	if err := db.Create(&models.Seller{Code: "SEL-1", Name: "Shop ABC", Status: "active"}).Error; err != nil {
		t.Fatalf("seed seller: %v", err)
	}
	batchID := seedBatchWithOrder(t, db, "100010", nil)

	b, err := repo.FindByID(batchID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if len(b.Items) != 1 || b.Items[0].OrderItem == nil || b.Items[0].OrderItem.Order == nil {
		t.Fatalf("batch detail missing item/order preload: %+v", b.Items)
	}
	if got := b.Items[0].OrderItem.Order.Seller.Name; got != "Shop ABC" {
		t.Errorf("seller name not preloaded: got %q, want %q", got, "Shop ABC")
	}
}
