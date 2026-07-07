package services

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newQCDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.User{}, &models.Seller{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{}, &models.ItemAsset{}, &models.Batch{}, &models.BatchItem{},
		&models.QCRecord{}, &models.StatusHistory{}, &models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestQCPass_RollsBatchUp_EndToEnd drives the real QCService.Pass path (the "Quét
// QC → PASS" flow) and asserts the batch header follows the item up to QC_PASSED.
// This is the exact scenario from the production board: a single-item batch at CUT
// whose item gets QC-passed must end at QC_PASSED — item, order item AND batch.
func TestQCPass_RollsBatchUp_EndToEnd(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}

	// Approved order + one item with a mockup (QC requires a mockup to compare).
	order := &models.Order{
		InternalCode: "ORD-000001", StoreOrderID: "US-1", StoreOrderRef: "US-1", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	item := &models.OrderItem{
		OrderID: order.ID, LineNo: 1, InternalCode: "ORD-000001_1", SKUCode: "BR-A-1.6-KEP",
		Quantity: 1, MockupURL: "https://example.com/mockup.png", InternalStatus: models.StatusCut,
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// A batch at CUT holding that item's production part (also CUT).
	batch := &models.Batch{Code: "B-101002", MaterialID: 1, Status: models.StatusCut}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	bi := &models.BatchItem{BatchID: batch.ID, OrderItemID: item.ID, MaterialID: 1, Status: models.StatusCut}
	if err := db.Create(bi).Error; err != nil {
		t.Fatalf("seed batch item: %v", err)
	}

	// Act: QC PASS by scanning the item code (no batch_item_id → all parts).
	updated, err := qc.Pass(Actor{ID: 1, Role: models.RoleOwner}, QCDecisionInput{
		ScanRef: ScanRef{Code: "ORD-000001_1"},
	})
	if err != nil {
		t.Fatalf("QC Pass failed: %v", err)
	}

	// Item is QC_PASSED...
	if updated.InternalStatus != models.StatusQCPassed {
		t.Fatalf("item: want QC_PASSED, got %s", updated.InternalStatus)
	}
	// ...the batch item is QC_PASSED...
	var gotBI models.BatchItem
	db.First(&gotBI, bi.ID)
	if gotBI.Status != models.StatusQCPassed {
		t.Fatalf("batch item: want QC_PASSED, got %s", gotBI.Status)
	}
	// ...and crucially the BATCH rolled up to QC_PASSED (the reported bug).
	gotBatch, err := repo.Batch.FindByID(batch.ID)
	if err != nil {
		t.Fatalf("reload batch: %v", err)
	}
	if gotBatch.Status != models.StatusQCPassed {
		t.Fatalf("batch did NOT roll up: want QC_PASSED, got %s", gotBatch.Status)
	}
}
