package services

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// TestBatchDelete_ReleasesItemsForRebatch is the core promise AND the unique-index
// regression test: deleting a PENDING batch must hard-delete its parts, blank the
// stamped print/cut file, drop the item back to PENDING — and, crucially, let the
// very same items be batched again. A soft delete of batch_items would pass every
// other assertion and die here on idx_item_material_attempt (no deleted_at
// predicate) because NextAttempts never sees soft-deleted rows.
func TestBatchDelete_ReleasesItemsForRebatch(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	actor := Actor{ID: 1, Role: models.RoleDesigner}
	_, ids := seedSplit(t, db, 0, 2)

	batch, _, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.SetBatchLink(actor, batch.ID, SetBatchLinkInput{Kind: "PRINT", URL: "https://files.example.com/print-1.pdf"}); err != nil {
		t.Fatalf("set link: %v", err)
	}

	if err := svc.Delete(actor, batch.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Header soft-deleted (gone from queries, still there for audit)…
	if err := db.First(&models.Batch{}, batch.ID).Error; err != gorm.ErrRecordNotFound {
		t.Fatalf("batch should be soft-deleted, got err=%v", err)
	}
	var kept int64
	db.Unscoped().Model(&models.Batch{}).Where("id = ?", batch.ID).Count(&kept)
	if kept != 1 {
		t.Fatalf("batch header should survive unscoped, got %d", kept)
	}
	// …parts hard-deleted: even Unscoped finds nothing.
	var parts int64
	db.Unscoped().Model(&models.BatchItem{}).Where("batch_id = ?", batch.ID).Count(&parts)
	if parts != 0 {
		t.Fatalf("batch items must be hard-deleted, %d rows remain", parts)
	}

	// Items are back in the pool: PENDING, stamped print file gone.
	var items []models.OrderItem
	if err := db.Find(&items, ids).Error; err != nil {
		t.Fatalf("load items: %v", err)
	}
	for _, it := range items {
		if it.InternalStatus != models.StatusPending {
			t.Fatalf("item %d should be PENDING, got %s", it.ID, it.InternalStatus)
		}
		if it.PrintFileURL != "" {
			t.Fatalf("item %d should have print_file_url cleared, got %q", it.ID, it.PrintFileURL)
		}
	}

	// The same items regroup into a fresh batch with nothing skipped.
	again, skipped, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("re-batch after delete: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("re-batch skipped %v, want none", skipped)
	}
	var newParts int64
	db.Model(&models.BatchItem{}).Where("batch_id = ?", again.ID).Count(&newParts)
	if newParts != 2 {
		t.Fatalf("new batch should hold 2 parts, got %d", newParts)
	}
}

// TestBatchDelete_BlockedOnceProductionStarted: an advanced header refuses, and —
// header lag aside — so does a single touched part under a still-PENDING header.
func TestBatchDelete_BlockedOnceProductionStarted(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	actor := Actor{ID: 1, Role: models.RoleDesigner}
	_, ids := seedSplit(t, db, 0, 2)

	batch, _, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// One part touched by production while the header still says PENDING.
	var part models.BatchItem
	if err := db.Where("batch_id = ?", batch.ID).First(&part).Error; err != nil {
		t.Fatalf("load part: %v", err)
	}
	if err := db.Model(&part).Update("status", models.StatusPrinted).Error; err != nil {
		t.Fatalf("advance part: %v", err)
	}
	if err := svc.Delete(actor, batch.ID); err == nil {
		t.Fatal("delete should refuse a batch with a produced part")
	}

	// Header advanced too: still refused, batch and parts untouched.
	if err := db.Model(&models.Batch{}).Where("id = ?", batch.ID).Update("status", models.StatusPrinted).Error; err != nil {
		t.Fatalf("advance header: %v", err)
	}
	if err := svc.Delete(actor, batch.ID); err == nil {
		t.Fatal("delete should refuse a PRINTED batch")
	}
	var parts int64
	db.Model(&models.BatchItem{}).Where("batch_id = ?", batch.ID).Count(&parts)
	if parts != 2 {
		t.Fatalf("refused delete must leave parts intact, got %d", parts)
	}
}

// TestBatchDelete_ParentTree: deleting a split parent takes the whole tree;
// deleting a child on its own is refused.
func TestBatchDelete_ParentTree(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	actor := Actor{ID: 1, Role: models.RoleDesigner}
	_, ids := seedSplit(t, db, 2, 5) // quota 2, 5 products → parent + 3 children

	parent, _, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !parent.IsParent {
		t.Fatalf("expected a split parent")
	}
	var child models.Batch
	if err := db.Where("parent_batch_id = ?", parent.ID).First(&child).Error; err != nil {
		t.Fatalf("load child: %v", err)
	}

	if err := svc.Delete(actor, child.ID); err == nil {
		t.Fatal("deleting a child on its own should be refused")
	}

	if err := svc.Delete(actor, parent.ID); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	var liveBatches int64
	db.Model(&models.Batch{}).Where("id = ? OR parent_batch_id = ?", parent.ID, parent.ID).Count(&liveBatches)
	if liveBatches != 0 {
		t.Fatalf("parent tree should be gone, %d batches remain", liveBatches)
	}
	var parts int64
	db.Unscoped().Model(&models.BatchItem{}).Count(&parts)
	if parts != 0 {
		t.Fatalf("all parts should be hard-deleted, %d remain", parts)
	}

	// Every released item is back to PENDING and can regroup.
	if _, skipped, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids}); err != nil {
		t.Fatalf("re-batch after tree delete: %v", err)
	} else if len(skipped) != 0 {
		t.Fatalf("re-batch skipped %v, want none", skipped)
	}
}
