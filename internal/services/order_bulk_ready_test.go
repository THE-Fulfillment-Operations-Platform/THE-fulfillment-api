package services

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newBulkReadyDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{}, &models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newOrderService(db *gorm.DB) *OrderService {
	repo := repositories.New(db)
	return &OrderService{repo: repo, audit: &AuditService{repo: repo}}
}

// seedReadyCandidate creates an approved order + one item in a given design state,
// with or without a mockup, and returns the item.
func seedReadyCandidate(t *testing.T, db *gorm.DB, code string, status models.DesignStatus, mockup string, approved bool) *models.OrderItem {
	t.Helper()
	review := models.ReviewApproved
	if !approved {
		review = models.ReviewPending
	}
	order := &models.Order{
		InternalCode: "ORD-" + code,
		StoreOrderID: "SO-" + code,
		SellerID:     1,
		ReviewStatus: review,
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
		MockupURL:    mockup,
		DesignStatus: status,
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("seed item %s: %v", code, err)
	}
	return item
}

// TestBulkSetDesignReady covers every branch of the one-shot "set ready" a designer
// uses to clear a material's worth of orders at once: eligible items flip to READY,
// while ineligible ones are reported back with a reason instead of failing the call,
// so one bad row never blocks the rest.
func TestBulkSetDesignReady(t *testing.T) {
	db := newBulkReadyDB(t)
	svc := newOrderService(db)
	const mockup = "https://example.com/m.png"

	ok1 := seedReadyCandidate(t, db, "OK-1", models.DesignPending, mockup, true)
	ok2 := seedReadyCandidate(t, db, "OK-2", models.DesignInProgress, mockup, true)
	noMockup := seedReadyCandidate(t, db, "NO-MOCKUP", models.DesignPending, "", true)
	already := seedReadyCandidate(t, db, "ALREADY", models.DesignReady, mockup, true)
	notApproved := seedReadyCandidate(t, db, "PENDING-REVIEW", models.DesignPending, mockup, false)
	cancelled := seedReadyCandidate(t, db, "CANCELLED", models.DesignPending, mockup, true)
	cancelled.CancellationStatus = models.CancellationApproved
	if err := db.Save(cancelled).Error; err != nil {
		t.Fatalf("cancel item: %v", err)
	}

	res, err := svc.BulkSetDesignReady(Actor{ID: 1}, []uint{
		ok1.ID, ok2.ID, noMockup.ID, already.ID, notApproved.ID, cancelled.ID, 999999,
	})
	if err != nil {
		t.Fatalf("bulk set ready: %v", err)
	}

	// Exactly the two eligible items flipped.
	if len(res.ReadyIDs) != 2 {
		t.Fatalf("ReadyIDs = %v, want the 2 eligible items", res.ReadyIDs)
	}
	assertDesignStatus(t, db, ok1.ID, models.DesignReady)
	assertDesignStatus(t, db, ok2.ID, models.DesignReady)

	// Ineligible items are skipped with a reason; the already-ready one is silent.
	reasons := map[string]string{}
	for _, s := range res.Skipped {
		reasons[s.ItemCode] = s.Reason
	}
	wantSkips := map[string]string{
		"NO-MOCKUP":      "thiếu mockup",
		"PENDING-REVIEW": "chưa duyệt",
		"CANCELLED":      "đã huỷ",
	}
	for code, want := range wantSkips {
		if reasons[code] != want {
			t.Errorf("skip reason for %s = %q, want %q", code, reasons[code], want)
		}
	}
	if _, listed := reasons["ALREADY"]; listed {
		t.Errorf("an already-ready item must not be reported as skipped")
	}
	// The missing id is skipped too (blank code, "không tồn tại").
	var sawMissing bool
	for _, s := range res.Skipped {
		if s.ItemID == 999999 && s.Reason == "không tồn tại" {
			sawMissing = true
		}
	}
	if !sawMissing {
		t.Errorf("a non-existent id must be reported as skipped")
	}

	// Ineligible items keep their original status.
	assertDesignStatus(t, db, noMockup.ID, models.DesignPending)
	assertDesignStatus(t, db, cancelled.ID, models.DesignPending)
	assertDesignStatus(t, db, notApproved.ID, models.DesignPending)
}

func TestBulkSetDesignReady_EmptyInput(t *testing.T) {
	db := newBulkReadyDB(t)
	svc := newOrderService(db)
	res, err := svc.BulkSetDesignReady(Actor{ID: 1}, nil)
	if err != nil {
		t.Fatalf("empty bulk set ready: %v", err)
	}
	if len(res.ReadyIDs) != 0 || len(res.Skipped) != 0 {
		t.Errorf("empty input should be a no-op, got ready=%v skipped=%v", res.ReadyIDs, res.Skipped)
	}
}

func assertDesignStatus(t *testing.T, db *gorm.DB, id uint, want models.DesignStatus) {
	t.Helper()
	var it models.OrderItem
	if err := db.First(&it, id).Error; err != nil {
		t.Fatalf("reload item %d: %v", id, err)
	}
	if it.DesignStatus != want {
		t.Errorf("item %d design_status = %q, want %q", id, it.DesignStatus, want)
	}
}
