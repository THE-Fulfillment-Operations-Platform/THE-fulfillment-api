package services

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// ---------------------------------------------------------------------------
// The carrier's word is news about the ORDER, not only about the parcel.
//
// Before this, seller status was only ever advanced by a human at the packing /
// shipping desk. Nobody presses a button when a parcel is delivered, so orders
// sat at "Đã bàn giao" forever while the tracking badge beside them already read
// "Đã giao" — the same order describing itself two different ways.
// ---------------------------------------------------------------------------

func advanceFixture(t *testing.T) (*gorm.DB, *TrackingSyncService) {
	t.Helper()
	db := newTrackingDB(t)
	repo := repositories.New(db)
	return db, &TrackingSyncService{repo: repo}
}

func reloadOrder(t *testing.T, db *gorm.DB, id uint) *models.Order {
	t.Helper()
	var o models.Order
	if err := db.First(&o, id).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	return &o
}

func TestAdvanceSellerStatus_CarrierStatesMapToLifecycle(t *testing.T) {
	cases := []struct {
		name    string
		from    models.SellerStatus
		carrier models.TrackingStatus
		want    models.SellerStatus
	}{
		{"delivered ends the lifecycle", models.SellerStatusHandedOff, models.TrackingDelivered, models.SellerStatusDelivered},
		{"in transit means shipped", models.SellerStatusHandedOff, models.TrackingInTransit, models.SellerStatusShipped},
		{"out for delivery means shipped", models.SellerStatusHandedOff, models.TrackingOutForDelivery, models.SellerStatusShipped},
		{"pick up means shipped", models.SellerStatusHandedOff, models.TrackingPickUp, models.SellerStatusShipped},
		{"shipped order still reaches delivered", models.SellerStatusShipped, models.TrackingDelivered, models.SellerStatusDelivered},

		// Nothing scanned yet: HANDED_OFF already says exactly that.
		{"pending changes nothing", models.SellerStatusHandedOff, models.TrackingPending, models.SellerStatusHandedOff},
		{"pre-transit changes nothing", models.SellerStatusHandedOff, models.TrackingPreTransit, models.SellerStatusHandedOff},
		// Trouble with the parcel is the tracking badge's job to report, not a
		// reason to move the order forward or backward.
		{"undelivered changes nothing", models.SellerStatusShipped, models.TrackingUndelivered, models.SellerStatusShipped},
		{"exception changes nothing", models.SellerStatusShipped, models.TrackingException, models.SellerStatusShipped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, svc := advanceFixture(t)
			o := seedOrderAtStage(t, db, "100001", tc.from, "940011")

			svc.advanceSellerStatus(o, tc.carrier)

			if got := reloadOrder(t, db, o.ID).SellerStatus; got != tc.want {
				t.Errorf("seller status = %q, want %q", got, tc.want)
			}
		})
	}
}

// A return-leg scan, or a provider replaying an older state, must not walk a
// delivered order back down the timeline.
func TestAdvanceSellerStatus_NeverGoesBackwards(t *testing.T) {
	db, svc := advanceFixture(t)
	o := seedOrderAtStage(t, db, "100001", models.SellerStatusDelivered, "940011")

	svc.advanceSellerStatus(o, models.TrackingInTransit)

	if got := reloadOrder(t, db, o.ID).SellerStatus; got != models.SellerStatusDelivered {
		t.Errorf("delivered order walked back to %q", got)
	}
}

// A cancelled order keeps the state it was cancelled in. Its paper trail says how
// far it got before being pulled, and a late carrier scan must not rewrite that
// into a delivery the seller was told was cancelled.
func TestAdvanceSellerStatus_LeavesCancelledOrderAlone(t *testing.T) {
	db, svc := advanceFixture(t)
	o := seedOrderAtStage(t, db, "100001", models.SellerStatusHandedOff, "940011")
	o.ReviewStatus = models.ReviewCancelled
	if err := db.Save(o).Error; err != nil {
		t.Fatalf("cancel order: %v", err)
	}

	svc.advanceSellerStatus(o, models.TrackingDelivered)

	if got := reloadOrder(t, db, o.ID).SellerStatus; got != models.SellerStatusHandedOff {
		t.Errorf("cancelled order advanced to %q", got)
	}
}

// The seller's order history is where this shows up on their screen, and nobody
// in the factory witnessed the delivery — so it must be attributed to nobody
// (changed_by_id NULL), which the seller view renders as "Hệ thống".
func TestAdvanceSellerStatus_WritesSystemHistory(t *testing.T) {
	db, svc := advanceFixture(t)
	o := seedOrderAtStage(t, db, "100001", models.SellerStatusHandedOff, "940011")

	svc.advanceSellerStatus(o, models.TrackingDelivered)

	var rows []models.StatusHistory
	if err := db.Where("entity_type = ? AND entity_id = ?", models.EntityOrder, o.ID).Find(&rows).Error; err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 history row, got %d", len(rows))
	}
	if rows[0].FromStatus != "HANDED_OFF" || rows[0].ToStatus != "DELIVERED" {
		t.Errorf("history row = %s→%s, want HANDED_OFF→DELIVERED", rows[0].FromStatus, rows[0].ToStatus)
	}
	if rows[0].ChangedByID != nil {
		t.Errorf("changed_by_id = %v, want NULL (system)", *rows[0].ChangedByID)
	}
}

// The orders already stuck when this shipped are the whole reason the reconcile
// pass exists: DueForSync drops a delivered parcel for good, so nothing would
// ever look at them again.
func TestReconcileSellerStatus_FixesOrdersFrozenByTheOldBug(t *testing.T) {
	db, svc := advanceFixture(t)

	stuck := seedOrderAtStage(t, db, "100001", models.SellerStatusHandedOff, "940011")
	stuck.TrackingStatus = models.TrackingDelivered
	if err := db.Save(stuck).Error; err != nil {
		t.Fatalf("seed stuck order: %v", err)
	}
	moving := seedOrderAtStage(t, db, "100002", models.SellerStatusHandedOff, "940022")
	moving.TrackingStatus = models.TrackingInTransit
	if err := db.Save(moving).Error; err != nil {
		t.Fatalf("seed moving order: %v", err)
	}
	// Already consistent — must not be counted or rewritten.
	fine := seedOrderAtStage(t, db, "100003", models.SellerStatusDelivered, "940033")
	fine.TrackingStatus = models.TrackingDelivered
	if err := db.Save(fine).Error; err != nil {
		t.Fatalf("seed consistent order: %v", err)
	}

	if fixed := svc.reconcileSellerStatus(50); fixed != 2 {
		t.Errorf("reconciled %d orders, want 2", fixed)
	}
	if got := reloadOrder(t, db, stuck.ID).SellerStatus; got != models.SellerStatusDelivered {
		t.Errorf("stuck order = %q, want DELIVERED", got)
	}
	if got := reloadOrder(t, db, moving.ID).SellerStatus; got != models.SellerStatusShipped {
		t.Errorf("in-transit order = %q, want SHIPPED", got)
	}
}

// A delivered order is still handed over: its journey must stay readable, and the
// cancellation rules must still treat it as at least shipped (billable in full).
func TestDeliveredKeepsDownstreamRules(t *testing.T) {
	if !models.SellerStatusDelivered.HandedOver() {
		t.Error("DELIVERED is not handed over — its tracking journey would disappear")
	}
	if got := orderCancelStage(models.SellerStatusDelivered, true); got != models.CancelStageShipped {
		t.Errorf("cancel stage of a delivered order = %q, want SHIPPED", got)
	}
	if !orderPacked(models.SellerStatusDelivered) {
		t.Error("a delivered order does not count as packed-or-beyond")
	}
}
