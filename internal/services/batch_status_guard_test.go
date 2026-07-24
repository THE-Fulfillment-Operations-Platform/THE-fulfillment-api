package services

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// seedGuardBatch creates a flat batch holding one item, ready to have its status
// advanced.
func seedGuardBatch(t *testing.T, db *gorm.DB, code string) *models.Batch {
	t.Helper()
	material := &models.Material{Code: "NVL-" + code, Name: "NVL"}
	if err := db.Create(material).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	order := &models.Order{
		InternalCode: "ORD-" + code, StoreOrderID: "SO-" + code, SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	item := &models.OrderItem{
		OrderID: order.ID, InternalCode: "IT-" + code, SKUCode: "SKU-1", Quantity: 1,
		DesignStatus: models.DesignReady,
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	batch := &models.Batch{Code: code, MaterialID: material.ID, Status: models.StatusPending}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	bi := &models.BatchItem{BatchID: batch.ID, OrderItemID: item.ID, MaterialID: material.ID, Status: models.StatusPending}
	if err := db.Create(bi).Error; err != nil {
		t.Fatalf("seed batch item: %v", err)
	}
	return batch
}

// seedBatchLinks gives a batch both production files — the precondition for entering
// fabrication. Tests that exercise other status rules use this to get past the guard.
func seedBatchLinks(t *testing.T, db *gorm.DB, batchID uint) {
	t.Helper()
	addGuardLink(t, db, batchID, models.BatchLinkPrint)
	addGuardLink(t, db, batchID, models.BatchLinkCut)
}

func addGuardLink(t *testing.T, db *gorm.DB, batchID uint, kind models.BatchLinkKind) {
	t.Helper()
	l := &models.BatchLink{BatchID: batchID, Kind: kind, URL: "https://files/" + string(kind)}
	if err := db.Create(l).Error; err != nil {
		t.Fatalf("seed %s link: %v", kind, err)
	}
}

// TestUpdateStatus_RequiresProductionLinks locks in the rule that a batch cannot be
// marked printed or cut before its shared print AND cut files exist: nobody can have
// printed without a print file, and a batch advanced without them would also sit in
// the design queue forever (it clears only once both links are set).
func TestUpdateStatus_RequiresProductionLinks(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	batch := seedGuardBatch(t, db, "#GUARD-1")

	// No links at all — both fabrication stages are refused, and the message names
	// what is missing.
	for _, status := range []models.InternalStatus{models.StatusPrinted, models.StatusCut} {
		_, err := svc.UpdateStatus(Actor{ID: 1}, batch.ID, UpdateStatusInput{Status: string(status)})
		if err == nil {
			t.Fatalf("%s must be refused while the batch has no production links", status)
		}
		if !strings.Contains(err.Error(), "link in") || !strings.Contains(err.Error(), "link cắt") {
			t.Errorf("error for %s should name both missing links, got: %v", status, err)
		}
	}

	// Print link only is still not enough — the customer requires both files.
	addGuardLink(t, db, batch.ID, models.BatchLinkPrint)
	_, err := svc.UpdateStatus(Actor{ID: 1}, batch.ID, UpdateStatusInput{Status: string(models.StatusPrinted)})
	if err == nil {
		t.Fatal("PRINTED must still be refused while the cut link is missing")
	}
	if !strings.Contains(err.Error(), "link cắt") {
		t.Errorf("error should name the missing cut link, got: %v", err)
	}

	// Both links present — the batch advances.
	addGuardLink(t, db, batch.ID, models.BatchLinkCut)
	updated, err := svc.UpdateStatus(Actor{ID: 1}, batch.ID, UpdateStatusInput{Status: string(models.StatusPrinted)})
	if err != nil {
		t.Fatalf("PRINTED with both links: %v", err)
	}
	if updated.Status != models.StatusPrinted {
		t.Errorf("batch status = %q, want PRINTED", updated.Status)
	}
}

// TestUpdateStatus_PendingNeedsNoLinks keeps the guard off the stage that represents
// "not started": only entering fabrication requires the files.
func TestUpdateStatus_PendingNeedsNoLinks(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	batch := seedGuardBatch(t, db, "#GUARD-2")

	if _, err := svc.UpdateStatus(Actor{ID: 1}, batch.ID, UpdateStatusInput{Status: string(models.StatusPending)}); err != nil {
		t.Fatalf("PENDING must not require links: %v", err)
	}
}

// TestUpdateStatus_ParentBatchSkipsLinkGuard covers the split case: a parent holds no
// items and can carry no links (SetBatchLink rejects it), so guarding it would lock
// its status forever. Each child is guarded on its own instead.
func TestUpdateStatus_ParentBatchSkipsLinkGuard(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	material := &models.Material{Code: "NVL-PARENT", Name: "NVL"}
	if err := db.Create(material).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	parent := &models.Batch{Code: "#GUARD-P", MaterialID: material.ID, IsParent: true, Status: models.StatusPending}
	if err := db.Create(parent).Error; err != nil {
		t.Fatalf("seed parent: %v", err)
	}

	if _, err := svc.UpdateStatus(Actor{ID: 1}, parent.ID, UpdateStatusInput{Status: string(models.StatusPrinted)}); err != nil {
		t.Fatalf("parent batch must not be blocked by the link guard: %v", err)
	}
}
