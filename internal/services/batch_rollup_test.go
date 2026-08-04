package services

import (
	"fmt"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newRollupDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.User{}, &models.Material{}, &models.Order{}, &models.OrderItem{},
		&models.Batch{}, &models.BatchItem{}, &models.BatchLink{}, &models.StatusHistory{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedOrderItems creates the order lines that the batch parts point at, with the
// given ids. The status roll-up joins through them — a part whose line was
// cancelled is not work the batch is waiting on — so a fixture without them has
// no countable parts at all.
func seedOrderItems(db *gorm.DB, t *testing.T, ids ...uint) {
	t.Helper()
	order := &models.Order{InternalCode: "O-1", StoreOrderID: "S-1", SellerID: 1}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	for _, id := range ids {
		it := &models.OrderItem{
			Base: models.Base{ID: id}, OrderID: order.ID,
			InternalCode: fmt.Sprintf("I-%d", id), SKUCode: "SKU-1", Quantity: 1,
			InternalStatus: models.StatusPending, DesignStatus: models.DesignPending,
			CancellationStatus: models.CancellationNone,
		}
		if err := db.Create(it).Error; err != nil {
			t.Fatalf("seed order item %d: %v", id, err)
		}
	}
}

func batchItemStatus(db *gorm.DB, batchID uint, orderItemID uint, to models.InternalStatus, t *testing.T) {
	t.Helper()
	var bi models.BatchItem
	if err := db.Where("batch_id = ? AND order_item_id = ?", batchID, orderItemID).First(&bi).Error; err != nil {
		t.Fatalf("load batch item: %v", err)
	}
	bi.Status = to
	if err := db.Save(&bi).Error; err != nil {
		t.Fatalf("save batch item: %v", err)
	}
}

// TestRecomputeBatchStatus_RollsUpFromItems reproduces the reported inconsistency:
// after a QC scan advances every item in a batch to QC_PASSED, the batch header
// must follow. It also verifies the batch stays at the least-advanced status while
// any item lags behind.
func TestRecomputeBatchStatus_RollsUpFromItems(t *testing.T) {
	db := newRollupDB(t)
	repo := repositories.New(db)
	actor := Actor{ID: 1}

	// A batch at CUT with two items, both CUT.
	seedOrderItems(db, t, 1, 2)
	batch := &models.Batch{Code: "B-1", Status: models.StatusCut}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	if err := db.Create(&[]models.BatchItem{
		{BatchID: batch.ID, OrderItemID: 1, MaterialID: 1, Status: models.StatusCut},
		{BatchID: batch.ID, OrderItemID: 2, MaterialID: 1, Status: models.StatusCut},
	}).Error; err != nil {
		t.Fatalf("seed items: %v", err)
	}

	// Only the first item passes QC → batch must stay CUT (least-advanced).
	batchItemStatus(db, batch.ID, 1, models.StatusQCPassed, t)
	if err := recomputeBatchStatus(repo, batch.ID, actor); err != nil {
		t.Fatalf("recompute (partial): %v", err)
	}
	got, _ := repo.Batch.FindByID(batch.ID)
	if got.Status != models.StatusCut {
		t.Fatalf("one item still CUT → want batch CUT, got %s", got.Status)
	}

	// Now the second item passes QC → batch rolls up to QC_PASSED.
	batchItemStatus(db, batch.ID, 2, models.StatusQCPassed, t)
	if err := recomputeBatchStatus(repo, batch.ID, actor); err != nil {
		t.Fatalf("recompute (all): %v", err)
	}
	got, _ = repo.Batch.FindByID(batch.ID)
	if got.Status != models.StatusQCPassed {
		t.Fatalf("all items QC_PASSED → want batch QC_PASSED, got %s", got.Status)
	}

	// A status-history row must record the batch transition.
	var count int64
	db.Model(&models.StatusHistory{}).
		Where("entity_type = ? AND entity_id = ? AND to_status = ?",
			models.EntityBatch, batch.ID, string(models.StatusQCPassed)).
		Count(&count)
	if count != 1 {
		t.Fatalf("want 1 batch status-history row for QC_PASSED, got %d", count)
	}
}

// TestRecomputeBatchStatus_SingleItem covers the exact screenshot case: a batch
// with a single item that has just been QC-scanned.
func TestRecomputeBatchStatus_SingleItem(t *testing.T) {
	db := newRollupDB(t)
	repo := repositories.New(db)

	seedOrderItems(db, t, 1)
	batch := &models.Batch{Code: "B-2", Status: models.StatusCut}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	if err := db.Create(&models.BatchItem{
		BatchID: batch.ID, OrderItemID: 1, MaterialID: 1, Status: models.StatusQCPassed,
	}).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}

	if err := recomputeBatchStatus(repo, batch.ID, Actor{ID: 1}); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	got, _ := repo.Batch.FindByID(batch.ID)
	if got.Status != models.StatusQCPassed {
		t.Fatalf("single QC'd item → want batch QC_PASSED, got %s", got.Status)
	}
}

// TestRecomputeBatchStatus_IgnoresCancelledItems locks in the cancellation half
// of the roll-up: once a line is cancelled its part is not work anyone will do,
// so the batch must be free to advance on what is left. Without this a single
// cancelled product would pin its batch at PENDING forever.
func TestRecomputeBatchStatus_IgnoresCancelledItems(t *testing.T) {
	db := newRollupDB(t)
	repo := repositories.New(db)

	seedOrderItems(db, t, 1, 2)
	batch := &models.Batch{Code: "B-3", Status: models.StatusPrinted}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	if err := db.Create(&[]models.BatchItem{
		{BatchID: batch.ID, OrderItemID: 1, MaterialID: 1, Status: models.StatusQCPassed},
		{BatchID: batch.ID, OrderItemID: 2, MaterialID: 1, Status: models.StatusPending},
	}).Error; err != nil {
		t.Fatalf("seed items: %v", err)
	}

	// The lagging part still holds the batch back while its line is live.
	if err := recomputeBatchStatus(repo, batch.ID, Actor{ID: 1}); err != nil {
		t.Fatalf("recompute (live): %v", err)
	}
	if got, _ := repo.Batch.FindByID(batch.ID); got.Status != models.StatusPending {
		t.Fatalf("live pending part → want batch PENDING, got %s", got.Status)
	}

	// Cancel that line → the batch is done with what remains.
	if err := db.Model(&models.OrderItem{}).Where("id = ?", 2).
		Update("cancellation_status", models.CancellationApproved).Error; err != nil {
		t.Fatalf("cancel item: %v", err)
	}
	if err := recomputeBatchStatus(repo, batch.ID, Actor{ID: 1}); err != nil {
		t.Fatalf("recompute (cancelled): %v", err)
	}
	if got, _ := repo.Batch.FindByID(batch.ID); got.Status != models.StatusQCPassed {
		t.Fatalf("cancelled part ignored → want batch QC_PASSED, got %s", got.Status)
	}
}
