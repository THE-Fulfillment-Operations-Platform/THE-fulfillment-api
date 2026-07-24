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
		&models.Order{}, &models.OrderItem{}, &models.ItemAsset{}, &models.Batch{}, &models.BatchItem{}, &models.BatchLink{},
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

// TestQCPass_ComboWaitsForAllMaterials asserts QC is a single product-level gate:
// a combo (SKU with 2 materials → 2 per-material batches) can only be QC-passed
// once EVERY material part is produced, and one PASS then marks the whole product
// (all parts) QC_PASSED — not once per NVL.
func TestQCPass_ComboWaitsForAllMaterials(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}
	actor := Actor{ID: 1, Role: models.RoleOwner}

	wood := &models.Material{Code: "WOOD", Name: "Gỗ"}
	mica := &models.Material{Code: "MICA", Name: "Mica"}
	db.Create(wood)
	db.Create(mica)

	sku := &models.SKU{Code: "COMBO-01", Name: "Đèn combo", IsCombo: true}
	db.Create(sku)
	db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: wood.ID, QuantityPerUnit: 1})
	db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mica.ID, QuantityPerUnit: 1})

	order := &models.Order{
		InternalCode: "ORD-COMBO-1", StoreOrderID: "US-9", StoreOrderRef: "US-9", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	db.Create(order)
	item := &models.OrderItem{
		OrderID: order.ID, LineNo: 1, InternalCode: "ORD-COMBO-1_1", SKUID: &sku.ID, SKUCode: sku.Code,
		Quantity: 1, MockupURL: "https://example.com/m.png", InternalStatus: models.StatusPending,
	}
	db.Create(item)

	// Only the wood part is batched and produced; mica is not batched at all.
	woodBatch := &models.Batch{Code: "B-W", MaterialID: wood.ID, Status: models.StatusCut}
	db.Create(woodBatch)
	woodBI := &models.BatchItem{BatchID: woodBatch.ID, OrderItemID: item.ID, MaterialID: wood.ID, Status: models.StatusCut}
	db.Create(woodBI)

	// QC must refuse: the mica part isn't produced yet → product not complete.
	if _, err := qc.Pass(actor, QCDecisionInput{ScanRef: ScanRef{Code: item.InternalCode}}); err == nil {
		t.Fatalf("QC must be blocked while a material part is unproduced")
	}

	// Now the mica part exists but is still PENDING → still blocked.
	micaBatch := &models.Batch{Code: "B-M", MaterialID: mica.ID, Status: models.StatusPending}
	db.Create(micaBatch)
	micaBI := &models.BatchItem{BatchID: micaBatch.ID, OrderItemID: item.ID, MaterialID: mica.ID, Status: models.StatusPending}
	db.Create(micaBI)
	if _, err := qc.Pass(actor, QCDecisionInput{ScanRef: ScanRef{Code: item.InternalCode}}); err == nil {
		t.Fatalf("QC must be blocked while a material part is still PENDING")
	}

	// Mica only PRINTED (in-progress, not cut) → still blocked: QC needs every part
	// to reach CUT, not merely started.
	micaBI.Status = models.StatusPrinted
	db.Save(micaBI)
	if _, err := qc.Pass(actor, QCDecisionInput{ScanRef: ScanRef{Code: item.InternalCode}}); err == nil {
		t.Fatalf("QC must be blocked while a material part is only PRINTED (not CUT)")
	}

	// Mica cut → the whole product is complete. One PASS QC's everything.
	micaBI.Status = models.StatusCut
	db.Save(micaBI)
	updated, err := qc.Pass(actor, QCDecisionInput{ScanRef: ScanRef{Code: item.InternalCode}})
	if err != nil {
		t.Fatalf("QC should pass once every material is produced: %v", err)
	}
	if updated.InternalStatus != models.StatusQCPassed {
		t.Fatalf("item: want QC_PASSED, got %s", updated.InternalStatus)
	}
	// Both parts QC_PASSED from the single scan (product-level QC).
	var w, m models.BatchItem
	db.First(&w, woodBI.ID)
	db.First(&m, micaBI.ID)
	if w.Status != models.StatusQCPassed || m.Status != models.StatusQCPassed {
		t.Fatalf("both parts should be QC_PASSED from one scan, got wood=%s mica=%s", w.Status, m.Status)
	}

	// A re-scan of the finished product is refused (no duplicate QC).
	if _, err := qc.Pass(actor, QCDecisionInput{ScanRef: ScanRef{Code: item.InternalCode}}); err == nil {
		t.Fatalf("re-scanning an already QC-passed product must be refused")
	}
}
