package services

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newCascadeDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Order{}, &models.OrderItem{},
		&models.Batch{}, &models.BatchItem{}, &models.StatusHistory{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestCascadeOrderCancellation is the guarantee that "đơn đã huỷ" means the same
// thing to the seller and to the factory. Every operational queue decides what is
// work from the ITEM's cancellation status, so cancelling only the order header
// would leave its products printing, batching and packing as usual.
func TestCascadeOrderCancellation(t *testing.T) {
	db := newCascadeDB(t)
	repo := repositories.New(db)
	actor := Actor{ID: 7}

	order := &models.Order{
		InternalCode: "O-1", StoreOrderID: "S-1", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	items := []models.OrderItem{
		// A line already in production…
		{OrderID: order.ID, InternalCode: "I-1", SKUCode: "SKU-1", Quantity: 1,
			InternalStatus: models.StatusPrinted, CancellationStatus: models.CancellationNone},
		// …one the seller had separately asked to drop, with its own reason…
		{OrderID: order.ID, InternalCode: "I-2", SKUCode: "SKU-2", Quantity: 1,
			InternalStatus: models.StatusPending, CancellationStatus: models.CancellationRequested,
			CancellationReason: "khách đổi ý về sản phẩm này"},
		// …and one already cancelled outright, which must stay untouched.
		{OrderID: order.ID, InternalCode: "I-3", SKUCode: "SKU-3", Quantity: 1,
			InternalStatus: models.StatusPending, CancellationStatus: models.CancellationSeller,
			CancellationReason: "huỷ trước đó"},
	}
	if err := db.Create(&items).Error; err != nil {
		t.Fatalf("seed items: %v", err)
	}
	batch := &models.Batch{Code: "B-1", Status: models.StatusPending}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	if err := db.Create(&[]models.BatchItem{
		{BatchID: batch.ID, OrderItemID: items[0].ID, MaterialID: 1, Status: models.StatusPrinted},
		{BatchID: batch.ID, OrderItemID: items[1].ID, MaterialID: 1, Status: models.StatusPending},
	}).Error; err != nil {
		t.Fatalf("seed batch items: %v", err)
	}

	order.ReviewStatus = models.ReviewCancelled
	order.CancellationStatus = models.CancellationApproved
	order.CancellationReason = "khách huỷ đơn"
	order.CancelStage = models.CancelStageInProduction
	order.CancelBillable = true
	if err := repo.Order.Update(order); err != nil {
		t.Fatalf("update order: %v", err)
	}

	now := time.Now()
	if err := cascadeOrderCancellation(repo, actor, order, now, "Huỷ đơn được duyệt"); err != nil {
		t.Fatalf("cascade: %v", err)
	}

	var got []models.OrderItem
	if err := db.Order("internal_code asc").Find(&got).Error; err != nil {
		t.Fatalf("reload items: %v", err)
	}
	for _, it := range got[:2] {
		if it.CancellationStatus != models.CancellationApproved {
			t.Fatalf("%s: want APPROVED, got %s", it.InternalCode, it.CancellationStatus)
		}
		if !it.CancelBillable || it.CancelStage != models.CancelStageInProduction {
			t.Fatalf("%s: want billable IN_PRODUCTION, got %v/%s", it.InternalCode, it.CancelBillable, it.CancelStage)
		}
	}
	// The order's reason only fills in where the line had none of its own.
	if got[0].CancellationReason != "khách huỷ đơn" {
		t.Fatalf("I-1 reason: got %q", got[0].CancellationReason)
	}
	if got[1].CancellationReason != "khách đổi ý về sản phẩm này" {
		t.Fatalf("I-2 must keep its own reason, got %q", got[1].CancellationReason)
	}
	// An already-cancelled line is history: not restamped, not rebilled.
	if got[2].CancellationStatus != models.CancellationSeller || got[2].CancelBillable {
		t.Fatalf("I-3 must stay untouched, got %s/%v", got[2].CancellationStatus, got[2].CancelBillable)
	}

	// The batch lost every part it was waiting on, so it must not sit on the
	// production board pretending there is work left.
	reloaded, err := repo.Batch.FindLite(batch.ID)
	if err != nil {
		t.Fatalf("reload batch: %v", err)
	}
	if reloaded.Status != models.StatusPending {
		t.Fatalf("empty batch keeps its status, got %s", reloaded.Status)
	}
	parts, err := repo.Batch.BatchItemsForBatch(batch.ID)
	if err != nil {
		t.Fatalf("batch parts: %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("want no countable parts left, got %d", len(parts))
	}
}
