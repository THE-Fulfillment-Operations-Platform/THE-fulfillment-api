package services

import (
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// seedOrderForShipping builds an approved order whose live items sit at the given
// internal statuses — the shape the ship queue reads.
func seedOrderForShipping(t *testing.T, db *gorm.DB, code string, statuses ...models.InternalStatus) *models.Order {
	t.Helper()
	order := &models.Order{
		InternalCode: code, StoreOrderID: "SO-" + code, SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
		TrackingStatus: models.TrackingNone,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	for i, st := range statuses {
		item := &models.OrderItem{
			OrderID: order.ID, LineNo: i + 1,
			InternalCode:   code + "_" + string(rune('1'+i)),
			SKUCode:        "SKU-1",
			Quantity:       1,
			InternalStatus: st,
		}
		if err := db.Create(item).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}
	return order
}

func opsActor() Actor { return Actor{ID: 1, Role: models.RoleOps} }

// The headline action: tick the QC-finished orders, press send, and they are all
// handed to THE at once — no packing scan in the way.
func TestShipOrdersToCarrier_SendsQCFinishedOrders(t *testing.T) {
	db := newHandoffDB(t)
	f := newFakeProvider()
	svc := newPackingService(db, newSync(t, db, f))

	a := seedOrderForShipping(t, db, "100001", models.StatusQCPassed)
	b := seedOrderForShipping(t, db, "100002", models.StatusQCPassed, models.StatusQCPassed)

	res, err := svc.ShipOrdersToCarrier(opsActor(), []uint{a.ID, b.ID})
	if err != nil {
		t.Fatalf("ShipOrdersToCarrier: %v", err)
	}
	if len(res.Shipped) != 2 || len(res.Skipped) != 0 {
		t.Fatalf("shipped=%d skipped=%v, want 2 shipped", len(res.Shipped), res.Skipped)
	}
	for _, sent := range res.Shipped {
		if !strings.HasPrefix(sent.HandoffCode, "THE-HO-") {
			t.Errorf("handoff code %q does not look like ours", sent.HandoffCode)
		}
	}
	for _, id := range []uint{a.ID, b.ID} {
		var got models.Order
		db.First(&got, id)
		if got.SellerStatus != models.SellerStatusHandedOff {
			t.Errorf("order %d = %s, want HANDED_OFF", id, got.SellerStatus)
		}
	}
	var handoffs int64
	db.Model(&models.Handoff{}).Count(&handoffs)
	if handoffs != 2 {
		t.Fatalf("%d handoff rows, want 2", handoffs)
	}
}

// One unfinished order in a selection of twenty must not sink the whole action,
// and the operator has to be told exactly which one and why.
func TestShipOrdersToCarrier_SkipsWithReasons(t *testing.T) {
	db := newHandoffDB(t)
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))

	ready := seedOrderForShipping(t, db, "100001", models.StatusQCPassed)
	halfDone := seedOrderForShipping(t, db, "100002", models.StatusQCPassed, models.StatusCut)
	alreadySent := seedOrderForShipping(t, db, "100003", models.StatusQCPassed)
	db.Model(alreadySent).Update("seller_status", models.SellerStatusHandedOff)
	notApproved := seedOrderForShipping(t, db, "100004", models.StatusQCPassed)
	db.Model(notApproved).Update("review_status", models.ReviewPending)
	cancelled := seedOrderForShipping(t, db, "100005", models.StatusQCPassed)
	db.Model(cancelled).Update("cancellation_status", models.CancellationApproved)

	res, err := svc.ShipOrdersToCarrier(opsActor(),
		[]uint{ready.ID, halfDone.ID, alreadySent.ID, notApproved.ID, cancelled.ID, 9999})
	if err != nil {
		t.Fatalf("ShipOrdersToCarrier: %v", err)
	}
	if len(res.Shipped) != 1 || res.Shipped[0].OrderID != ready.ID {
		t.Fatalf("shipped %+v, want only order %d", res.Shipped, ready.ID)
	}
	reasons := map[uint]string{}
	for _, s := range res.Skipped {
		reasons[s.OrderID] = s.Reason
	}
	checks := map[uint]string{
		halfDone.ID:    "chưa QC",
		alreadySent.ID: "đã gửi",
		notApproved.ID: "chưa được duyệt",
		cancelled.ID:   "đã huỷ",
		9999:           "Không tìm thấy",
	}
	for id, want := range checks {
		if got, ok := reasons[id]; !ok || !strings.Contains(got, want) {
			t.Errorf("order %d skipped with %q, want something containing %q", id, got, want)
		}
	}
	// The half-finished order must still be sitting in production afterwards.
	var got models.Order
	db.First(&got, halfDone.ID)
	if got.SellerStatus != models.SellerStatusProduction {
		t.Errorf("a skipped order moved to %s", got.SellerStatus)
	}
}

// Sending goods out is an operations decision. QC and the print floor check and
// make things; they do not decide what leaves the building.
func TestShipOrdersToCarrier_RoleGuard(t *testing.T) {
	db := newHandoffDB(t)
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))
	order := seedOrderForShipping(t, db, "100001", models.StatusQCPassed)

	for _, role := range []models.Role{models.RoleQC, models.RoleProduction, models.RoleDesigner, models.RoleSeller, models.RoleCS} {
		if _, err := svc.ShipOrdersToCarrier(Actor{ID: 2, Role: role}, []uint{order.ID}); err == nil {
			t.Errorf("%s was allowed to ship orders", role)
		}
	}
	for _, role := range []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RolePacking, models.RoleShipping} {
		db.Model(&models.Order{}).Where("id = ?", order.ID).Update("seller_status", models.SellerStatusProduction)
		if _, err := svc.ShipOrdersToCarrier(Actor{ID: 2, Role: role}, []uint{order.ID}); err != nil {
			t.Errorf("%s should be allowed to ship orders: %v", role, err)
		}
	}
}

// Sending is what starts tracking. An order whose number CS attached earlier must
// begin being watched the moment it goes out.
func TestShipOrdersToCarrier_StartsTracking(t *testing.T) {
	db := newHandoffDB(t)
	f := newFakeProvider()
	f.parcels["940011"] = &fakeParcel{Carrier: "USPS", Status: "In Transit", Location: "JAMAICA, NY"}
	svc := newPackingService(db, newSync(t, db, f))

	order := seedOrderForShipping(t, db, "100001", models.StatusQCPassed)
	db.Model(order).Updates(map[string]any{
		"tracking_number": "940011", "tracking_status": models.TrackingPending,
	})
	order.TrackingNumber = "940011"
	order.TrackingStatus = models.TrackingPending

	if _, err := svc.ShipOrdersToCarrier(opsActor(), []uint{order.ID}); err != nil {
		t.Fatalf("ShipOrdersToCarrier: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		var got models.Order
		db.First(&got, order.ID)
		return got.TrackingStatus == models.TrackingInTransit
	}, "tracking did not start after the order was sent")

	if got := f.parcels["940011"].Description; got != "FFM:SO-100001" {
		t.Errorf("parcel description = %q, want FFM:SO-100001", got)
	}
}

// Guard rails on the bulk input itself.
func TestShipOrdersToCarrier_RejectsBadSelections(t *testing.T) {
	db := newHandoffDB(t)
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))

	if _, err := svc.ShipOrdersToCarrier(opsActor(), nil); err == nil {
		t.Error("an empty selection should be rejected")
	}
	// Zero ids are noise from the UI, not orders.
	if _, err := svc.ShipOrdersToCarrier(opsActor(), []uint{0, 0}); err == nil {
		t.Error("a selection of only zero-ids should be rejected")
	}
	tooMany := make([]uint, MaxShipBatch+1)
	for i := range tooMany {
		tooMany[i] = uint(i + 1)
	}
	if _, err := svc.ShipOrdersToCarrier(opsActor(), tooMany); err == nil {
		t.Errorf("a selection above %d should be rejected", MaxShipBatch)
	}
}

// An order with no live items left (everything cancelled) is not "finished" — it
// is empty, and shipping an empty parcel would be nonsense.
func TestShipOrdersToCarrier_RefusesEmptyOrder(t *testing.T) {
	db := newHandoffDB(t)
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))

	order := seedOrderForShipping(t, db, "100001", models.StatusQCPassed)
	db.Model(&models.OrderItem{}).Where("order_id = ?", order.ID).
		Update("cancellation_status", models.CancellationApproved)

	res, err := svc.ShipOrdersToCarrier(opsActor(), []uint{order.ID})
	if err != nil {
		t.Fatalf("ShipOrdersToCarrier: %v", err)
	}
	if len(res.Shipped) != 0 {
		t.Fatal("an order with no live items was shipped")
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "không còn sản phẩm") {
		t.Fatalf("skip reason = %+v", res.Skipped)
	}
}
