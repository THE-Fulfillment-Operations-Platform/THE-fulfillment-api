package services

import (
	"fmt"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// newSplitDB builds an in-memory DB with every table the batch-split flow touches.
func newSplitDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.User{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{}, &models.ItemAsset{},
		&models.Batch{}, &models.BatchItem{}, &models.BatchLink{}, &models.StatusHistory{}, &models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newBatchService(db *gorm.DB) *BatchService {
	repo := repositories.New(db)
	return &BatchService{repo: repo, audit: &AuditService{repo: repo}}
}

// seedSplit creates a material (with an optional quota), a SKU mapped to it, an
// APPROVED order and `n` single-quantity items on that SKU. Returns the material
// and the ordered item ids.
func seedSplit(t *testing.T, db *gorm.DB, quota, n int) (*models.Material, []uint) {
	t.Helper()
	mat := &models.Material{Code: "MICA", Name: "Mica"}
	if quota > 0 {
		q := quota
		mat.ProductsPerUnit = &q
	}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	sku := &models.SKU{Code: "MICA-01", Name: "Mica Plate"}
	if err := db.Create(sku).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}
	if err := db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mat.ID, QuantityPerUnit: 1}).Error; err != nil {
		t.Fatalf("seed sku-material: %v", err)
	}
	order := &models.Order{
		InternalCode: "100001", StoreOrderID: "Etsy-1", SellerID: 1,
		SellerStatus: models.SellerStatusProduction, ReviewStatus: models.ReviewApproved,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	ids := make([]uint, 0, n)
	for i := 0; i < n; i++ {
		it := &models.OrderItem{
			OrderID: order.ID, InternalCode: fmt.Sprintf("100001_%d", i+1),
			SKUID: &sku.ID, SKUCode: sku.Code, Quantity: 1,
			InternalStatus: models.StatusPending, DesignStatus: models.DesignReady,
		}
		if err := db.Create(it).Error; err != nil {
			t.Fatalf("seed item %d: %v", i, err)
		}
		ids = append(ids, it.ID)
	}
	return mat, ids
}

// TestBatchCreate_SplitsOverQuota: 25 products, quota 20 → 1 parent + 2 children
// holding 20 and 5 items. Mirrors the web split preview.
func TestBatchCreate_SplitsOverQuota(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 20, 25)

	parent, skipped, err := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{
		MaterialID: 1, OrderItemIDs: ids,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("want 0 skipped, got %d", len(skipped))
	}
	if !parent.IsParent || parent.ChildCount != 2 {
		t.Fatalf("want parent with 2 children, got is_parent=%v child_count=%d", parent.IsParent, parent.ChildCount)
	}
	if len(parent.ChildBatches) != 2 {
		t.Fatalf("want 2 preloaded children, got %d", len(parent.ChildBatches))
	}
	counts := []int{len(parent.ChildBatches[0].Items), len(parent.ChildBatches[1].Items)}
	if counts[0] != 20 || counts[1] != 5 {
		t.Fatalf("want child item counts [20 5], got %v", counts)
	}
	for i, c := range parent.ChildBatches {
		if c.ParentBatchID == nil || *c.ParentBatchID != parent.ID {
			t.Fatalf("child %d parent_batch_id mismatch", i)
		}
		if c.Sequence != i+1 {
			t.Fatalf("child %d sequence = %d, want %d", i, c.Sequence, i+1)
		}
		wantCode := fmt.Sprintf("%s-%d", parent.Code, i+1)
		if c.Code != wantCode {
			t.Fatalf("child %d code = %q, want %q", i, c.Code, wantCode)
		}
	}
	// The parent holds no items of its own.
	if len(parent.Items) != 0 {
		t.Fatalf("parent should hold no items, got %d", len(parent.Items))
	}
}

// TestBatchCreate_WithinQuota_Flat: 5 products, quota 20 → one flat batch.
func TestBatchCreate_WithinQuota_Flat(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 20, 5)

	batch, _, err := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if batch.IsParent || batch.ParentBatchID != nil || batch.ChildCount != 0 {
		t.Fatalf("want flat batch, got is_parent=%v parent=%v child_count=%d", batch.IsParent, batch.ParentBatchID, batch.ChildCount)
	}
	if len(batch.Items) != 5 {
		t.Fatalf("want 5 items on flat batch, got %d", len(batch.Items))
	}
}

// TestBatchCreate_UnlimitedQuota_Flat: no quota set → never split, even for many items.
func TestBatchCreate_UnlimitedQuota_Flat(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 30)

	batch, _, err := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if batch.IsParent {
		t.Fatalf("unlimited quota must not split")
	}
	if len(batch.Items) != 30 {
		t.Fatalf("want 30 items, got %d", len(batch.Items))
	}
}

// TestUpdateStatus_NoRegression: the production board only moves forward. Advancing
// PENDING→PRINTED→CUT is fine; regressing back is rejected (protects the QC gate).
func TestUpdateStatus_NoRegression(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 2) // unlimited quota → one flat batch
	actor := Actor{ID: 1, Role: models.RoleProduction}

	batch, _, err := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Entering fabrication needs the batch's shared print/cut files; this test is
	// about the forward-only rule, so give it both and let the guard pass.
	seedBatchLinks(t, db, batch.ID)

	// Forward is allowed.
	if _, err := svc.UpdateStatus(actor, batch.ID, UpdateStatusInput{Status: string(models.StatusPrinted)}); err != nil {
		t.Fatalf("advance to PRINTED: %v", err)
	}
	if _, err := svc.UpdateStatus(actor, batch.ID, UpdateStatusInput{Status: string(models.StatusCut)}); err != nil {
		t.Fatalf("advance to CUT: %v", err)
	}

	// Backward is rejected.
	if _, err := svc.UpdateStatus(actor, batch.ID, UpdateStatusInput{Status: string(models.StatusPrinted)}); err == nil {
		t.Fatalf("regressing CUT → PRINTED must be rejected")
	}
	if _, err := svc.UpdateStatus(actor, batch.ID, UpdateStatusInput{Status: string(models.StatusPending)}); err == nil {
		t.Fatalf("regressing CUT → PENDING must be rejected")
	}
	got, _ := svc.repo.Batch.FindByID(batch.ID)
	if got.Status != models.StatusCut {
		t.Fatalf("batch should remain CUT after rejected regressions, got %s", got.Status)
	}
}

// TestUpdateStatus_OwnerRegression: OWNER may step a batch back (fix a mistaken
// advance), but an already QC-passed part is never dragged down, and a fully
// QC-passed batch stays locked even for OWNER.
func TestUpdateStatus_OwnerRegression(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	mat := &models.Material{Code: "MICA", Name: "Mica"}
	db.Create(mat)
	order := &models.Order{
		InternalCode: "O1", StoreOrderID: "S1", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	db.Create(order)
	// itemA already QC-passed, itemB still in production (CUT) → mixed batch.
	itemA := &models.OrderItem{OrderID: order.ID, LineNo: 1, InternalCode: "O1_1", Quantity: 1, InternalStatus: models.StatusQCPassed}
	itemB := &models.OrderItem{OrderID: order.ID, LineNo: 2, InternalCode: "O1_2", Quantity: 1, InternalStatus: models.StatusCut}
	db.Create(itemA)
	db.Create(itemB)
	batch := &models.Batch{Code: "B1", MaterialID: mat.ID, Status: models.StatusCut}
	db.Create(batch)
	biA := &models.BatchItem{BatchID: batch.ID, OrderItemID: itemA.ID, MaterialID: mat.ID, Status: models.StatusQCPassed}
	biB := &models.BatchItem{BatchID: batch.ID, OrderItemID: itemB.ID, MaterialID: mat.ID, Status: models.StatusCut}
	db.Create(biA)
	db.Create(biB)
	// This test is about the OWNER regression rule, not the link guard.
	seedBatchLinks(t, db, batch.ID)

	// OWNER regresses CUT → PRINTED.
	if _, err := svc.UpdateStatus(owner, batch.ID, UpdateStatusInput{Status: string(models.StatusPrinted)}); err != nil {
		t.Fatalf("owner regression should be allowed: %v", err)
	}
	var gotA, gotB models.BatchItem
	db.First(&gotA, biA.ID)
	db.First(&gotB, biB.ID)
	if gotA.Status != models.StatusQCPassed {
		t.Fatalf("QC-passed part must not be dragged down, got %s", gotA.Status)
	}
	if gotB.Status != models.StatusPrinted {
		t.Fatalf("in-production part should drop to PRINTED, got %s", gotB.Status)
	}

	// A fully QC-passed batch is locked even for OWNER.
	qcBatch := &models.Batch{Code: "B2", MaterialID: mat.ID, Status: models.StatusQCPassed}
	db.Create(qcBatch)
	db.Create(&models.BatchItem{BatchID: qcBatch.ID, OrderItemID: itemA.ID, MaterialID: mat.ID, Status: models.StatusQCPassed})
	if _, err := svc.UpdateStatus(owner, qcBatch.ID, UpdateStatusInput{Status: string(models.StatusPrinted)}); err == nil {
		t.Fatalf("a QC-passed batch must stay locked even for OWNER")
	}
}

// TestParentStatusRollup: a parent's status follows the least-advanced child, and
// only reaches QC_PASSED when every child has. Children reach QC_PASSED via the QC
// station (their items pass QC), never via the production board.
func TestParentStatusRollup(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 20, 25)

	parent, _, err := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	child1, child2 := parent.ChildBatches[0].ID, parent.ChildBatches[1].ID
	actor := Actor{ID: 1, Role: models.RoleProduction}

	// The production board must NOT be able to set QC_PASSED — QC is a product-level
	// gate done once at the QC station, not a per-material board transition.
	if _, err := svc.UpdateStatus(actor, child1, UpdateStatusInput{Status: string(models.StatusQCPassed)}); err == nil {
		t.Fatalf("board UpdateStatus(QC_PASSED) must be rejected, got no error")
	}

	// qcChild simulates the QC-station outcome: every part of the child batch passes
	// QC, then the batch (and its parent) roll up.
	qcChild := func(childID uint) {
		t.Helper()
		parts, err := svc.repo.Batch.BatchItemsForBatch(childID)
		if err != nil {
			t.Fatalf("load child items: %v", err)
		}
		for i := range parts {
			parts[i].Status = models.StatusQCPassed
			if err := svc.repo.Batch.UpdateBatchItem(&parts[i]); err != nil {
				t.Fatalf("update child item: %v", err)
			}
		}
		if err := recomputeBatchStatus(svc.repo, childID, actor); err != nil {
			t.Fatalf("recompute child: %v", err)
		}
	}

	// Only the first child QC'd → parent stays PENDING (child2 lags).
	qcChild(child1)
	got, _ := svc.repo.Batch.FindByID(parent.ID)
	if got.Status != models.StatusPending {
		t.Fatalf("one child QC'd → parent should stay PENDING, got %s", got.Status)
	}

	// The second child QC'd too → parent rolls up to QC_PASSED.
	qcChild(child2)
	got, _ = svc.repo.Batch.FindByID(parent.ID)
	if got.Status != models.StatusQCPassed {
		t.Fatalf("all children QC'd → parent should be QC_PASSED, got %s", got.Status)
	}
}
