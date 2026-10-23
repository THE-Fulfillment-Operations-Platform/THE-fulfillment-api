package repositories

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// seedBatchWithOrder creates one batch producing one order (one line) and
// returns the batch id.
func seedBatchWithOrder(t *testing.T, db *gorm.DB, code string) uint {
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
	batch := &models.Batch{Code: "#B-" + code, MaterialID: 1, Status: models.StatusPending}
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
// batch producing that order.
func TestBatchListCodeFilter(t *testing.T) {
	db := newQueueTestDB(t)
	repo := &BatchRepository{db: db}

	first := seedBatchWithOrder(t, db, "100001")
	second := seedBatchWithOrder(t, db, "100002")

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

	// Order internal code and item tem code both name the first batch, and only it.
	for _, code := range []string{"100001", "100001_1/1", " 100001 "} {
		if got := listIDs(BatchFilter{Page: page, Code: code}); len(got) != 1 || got[0] != first {
			t.Errorf("Code=%q: got batch ids %v, want [%d]", code, got, first)
		}
	}
	if got := listIDs(BatchFilter{Page: page, Code: "100002"}); len(got) != 1 || got[0] != second {
		t.Errorf("Code=100002: got batch ids %v, want [%d]", got, second)
	}

	// Unknown code: no batches, not the unfiltered list.
	if got := listIDs(BatchFilter{Page: page, Code: "999999"}); len(got) != 0 {
		t.Errorf("Code=999999: got batch ids %v, want none", got)
	}

	// No code: every batch.
	if got := listIDs(BatchFilter{Page: page}); len(got) != 2 {
		t.Errorf("default list: got %v, want both batches", got)
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
	batchID := seedBatchWithOrder(t, db, "100010")

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

// TestBatchListSellerAndStoreOrderFilter: find the batch an order of one shop sits
// in — by seller, by (part of) the shop order id, or both.
func TestBatchListSellerAndStoreOrderFilter(t *testing.T) {
	db := newQueueTestDB(t)
	repo := &BatchRepository{db: db}

	first := seedBatchWithOrder(t, db, "100011")  // store order "SO-100011", seller 1
	second := seedBatchWithOrder(t, db, "100012") // store order "SO-100012", seller 1
	db.Model(&models.Order{}).Where("internal_code = ?", "100012").Update("seller_id", 2)
	page := Page{Page: 1, PageSize: 20}

	ids := func(f BatchFilter) []uint {
		t.Helper()
		f.Page = page
		rows, _, err := repo.List(f)
		if err != nil {
			t.Fatalf("List(%+v): %v", f, err)
		}
		out := []uint{}
		for _, b := range rows {
			out = append(out, b.ID)
		}
		return out
	}
	one, two := uint(1), uint(2)
	if got := ids(BatchFilter{SellerID: &one}); len(got) != 1 || got[0] != first {
		t.Errorf("seller 1: %v, want [%d]", got, first)
	}
	if got := ids(BatchFilter{SellerID: &two}); len(got) != 1 || got[0] != second {
		t.Errorf("seller 2: %v, want [%d]", got, second)
	}
	for _, q := range []string{"so-100012", "100012", " SO-1000"} {
		got := ids(BatchFilter{StoreOrder: q})
		want := 1
		if q == " SO-1000" {
			want = 2
		}
		if len(got) != want {
			t.Errorf("StoreOrder=%q: %v, want %d batch(es)", q, got, want)
		}
	}
	if got := ids(BatchFilter{SellerID: &one, StoreOrder: "100012"}); len(got) != 0 {
		t.Errorf("seller 1 + order of seller 2 must be empty, got %v", got)
	}
	if got := ids(BatchFilter{StoreOrder: "nope"}); len(got) != 0 {
		t.Errorf("unknown store order: %v, want none", got)
	}
}

// TestBatchListBatchCodeAndQ: the production board's one search box finds a batch
// by its code, an order code, a shop order id or the seller's name.
func TestBatchListBatchCodeAndQ(t *testing.T) {
	db := newQueueTestDB(t)
	repo := &BatchRepository{db: db}
	first := seedBatchWithOrder(t, db, "100021")
	second := seedBatchWithOrder(t, db, "100022")
	db.Create(&models.Seller{Code: "SEL-Q", Name: "Shop Hoa Hồng", Status: "active"})
	var s models.Seller
	db.Where("code = ?", "SEL-Q").First(&s)
	db.Model(&models.Order{}).Where("internal_code = ?", "100021").Update("seller_id", s.ID+50) // another seller
	db.Model(&models.Order{}).Where("internal_code = ?", "100022").Update("seller_id", s.ID)
	page := Page{Page: 1, PageSize: 20}

	ids := func(f BatchFilter) []uint {
		t.Helper()
		f.Page = page
		rows, _, err := repo.List(f)
		if err != nil {
			t.Fatalf("List(%+v): %v", f, err)
		}
		out := []uint{}
		for _, b := range rows {
			out = append(out, b.ID)
		}
		return out
	}
	one := func(got []uint, want uint, what string) {
		t.Helper()
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: got %v, want [%d]", what, got, want)
		}
	}
	one(ids(BatchFilter{BatchCode: "#B-100021"}), first, "BatchCode with #? code is #B-…")
	one(ids(BatchFilter{BatchCode: "b-100022"}), second, "BatchCode contains, case-insensitive")
	for _, q := range []string{"#B-100022", "100022", "100022_1/1", "so-100022", "hồng", "HOA HỒNG"} {
		one(ids(BatchFilter{Q: q}), second, "Q="+q)
	}
	if got := ids(BatchFilter{Q: "khong-co"}); len(got) != 0 {
		t.Errorf("unknown Q: %v, want none", got)
	}
}
