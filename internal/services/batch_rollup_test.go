package services

import (
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
