package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

// TestDeleteOrders_SkipsInProduction: the tick-and-delete removes orders that
// have not reached production and reports the rest back — even to OWNER, who
// can still delete those one at a time from the order detail.
func TestDeleteOrders_SkipsInProduction(t *testing.T) {
	db := newBulkReadyDB(t)
	if err := db.AutoMigrate(&models.Batch{}, &models.BatchItem{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := newOrderService(db)

	fresh := seedReadyCandidate(t, db, "FRESH", models.DesignPending, "", false)
	batched := seedReadyCandidate(t, db, "BATCHED", models.DesignReady, "https://x/m.png", true)
	if err := db.Create(&models.BatchItem{BatchID: 1, OrderItemID: batched.ID, MaterialID: 1}).Error; err != nil {
		t.Fatalf("seed batch item: %v", err)
	}
	moved := seedReadyCandidate(t, db, "MOVED", models.DesignReady, "https://x/m.png", true)
	if err := db.Model(moved).Update("internal_status", models.StatusPrinted).Error; err != nil {
		t.Fatalf("advance item: %v", err)
	}

	owner := Actor{ID: 1, Role: models.RoleOwner}
	res, err := svc.DeleteOrders(owner, []uint{fresh.OrderID, batched.OrderID, moved.OrderID, fresh.OrderID, 9999})
	if err != nil {
		t.Fatalf("DeleteOrders: %v", err)
	}
	if len(res.DeletedIDs) != 1 || res.DeletedIDs[0] != fresh.OrderID {
		t.Fatalf("deleted = %v, want only %d", res.DeletedIDs, fresh.OrderID)
	}
	if len(res.Skipped) != 3 {
		t.Fatalf("skipped = %+v, want batched, moved, missing", res.Skipped)
	}

	var live int64
	db.Model(&models.Order{}).Count(&live)
	if live != 2 {
		t.Fatalf("live orders = %d, want 2", live)
	}
	var logged int64
	db.Model(&models.AuditLog{}).Where("action = ?", "ORDER_DELETE").Count(&logged)
	if logged != 1 {
		t.Fatalf("ORDER_DELETE entries = %d, want 1", logged)
	}
}

func TestDeleteOrders_RejectsNonAdmin(t *testing.T) {
	db := newBulkReadyDB(t)
	svc := newOrderService(db)
	o := seedReadyCandidate(t, db, "X", models.DesignPending, "", false)
	if _, err := svc.DeleteOrders(Actor{ID: 2, Role: models.RoleOps}, []uint{o.OrderID}); err == nil {
		t.Fatal("OPS must not bulk-delete orders")
	}
}
