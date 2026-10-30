package services

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/shipping/theapi"
)

// cancelOnTHE plays THE staff cancelling (refunding) every package of an order
// number in THE's CMS — FFM is not told.
func (e *theEnv) cancelOnTHE(orderNumber, status string) {
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	for _, p := range e.fake.pkgs {
		if p.orderNumber == orderNumber && p.charged {
			p.status, p.charged = status, false
		}
	}
}

// ageChecks makes every shipment due for the poll again (sending a label already
// asks THE once, for the last-mile number).
func (e *theEnv) ageChecks() {
	e.db.Model(&models.CarrierShipment{}).Where("1 = 1").Update("status_checked_at", time.Now().Add(-2*recheckEvery))
}

func (e *theEnv) orderRow(t *testing.T, id uint) models.Order {
	t.Helper()
	var o models.Order
	if err := e.db.First(&o, id).Error; err != nil {
		t.Fatal(err)
	}
	return o
}

// The case reported 30/10: THE cancelled a paid package (label + last-mile
// already in FFM). The poll must drop the dead label and tracking, put an order
// already out of the warehouse back to "Chờ gửi hàng", raise a note, keep the
// box from leaving again until it has a new label, and the new label must be a
// NEW paid package — not the dead one handed back.
func TestTHE_CancelledOnTHE_PollVoidsLabelAndTracking(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100051")
	if res := e.send(t, o); len(res.Shipped) != 1 {
		t.Fatalf("label: %+v", res)
	}
	if res := e.send(t, o, ShipSendOptions{ExportOnly: true}); len(res.Shipped) != 1 {
		t.Fatalf("export: %+v", res)
	}
	old := e.shipment(t, o.ID)
	if e.orderRow(t, o.ID).TrackingNumber != e.fake.lastMile || e.sellerStatus(t, o.ID) != models.SellerStatusHandedOff {
		t.Fatal("precondition: labeled, tracked and out of the warehouse")
	}

	e.cancelOnTHE(old.OrderNumber, "canceled")
	e.ageChecks()
	voided, err := e.carrier.SyncLabeledStatus(context.Background(), 30)
	if err != nil || voided != 1 {
		t.Fatalf("poll must void the cancelled package: voided=%d err=%v", voided, err)
	}

	var sh models.CarrierShipment
	e.db.First(&sh, old.ID)
	if sh.Status != models.ShipmentCancelled || !strings.HasPrefix(sh.Error, theCancelledPrefix) || sh.TheStatus != "canceled" {
		t.Fatalf("shipment not voided: %s / %q / %q", sh.Status, sh.Error, sh.TheStatus)
	}
	fresh := e.orderRow(t, o.ID)
	if fresh.TrackingNumber != "" || fresh.SellerStatus != models.SellerStatusProduction || fresh.HandedOverAt != nil {
		t.Fatalf("order keeps the dead tracking / stays out: tracking=%q status=%s", fresh.TrackingNumber, fresh.SellerStatus)
	}
	var note models.Note
	if err := e.db.Where("reason_code = ? AND entity_id = ?", "THE_CANCELLED", o.ID).First(&note).Error; err != nil || !note.IsRequiredAttention {
		t.Fatalf("a needs-attention note must be raised: %v %+v", err, note)
	}
	if _, err := e.carrier.Label(context.Background(), Actor{ID: 1, Role: models.RoleOwner}, o.ID); err == nil {
		t.Fatal("the dead label must not be printable any more")
	}
	// The box carries the dead label: Xuất kho refuses until there is a new one.
	if res := e.send(t, o, ShipSendOptions{ExportOnly: true}); len(res.Shipped) != 0 || len(res.Skipped) != 1 ||
		!strings.Contains(res.Skipped[0].Reason, "huỷ label") {
		t.Fatalf("export must be refused until a new label exists: %+v", res)
	}

	// New label → a NEW paid package; then the box may leave.
	res := e.send(t, o)
	if len(res.Shipped) != 1 || res.Shipped[0].THE == nil || res.Shipped[0].THE.Reused || res.Shipped[0].THE.TrackingCode == old.TrackingCode {
		t.Fatalf("a new label must be a new paid package: %+v", res)
	}
	if e.fake.chargedCount() != 1 {
		t.Fatalf("exactly one live paid package, got %d", e.fake.chargedCount())
	}
	if res := e.send(t, o, ShipSendOptions{ExportOnly: true}); len(res.Shipped) != 1 {
		t.Fatalf("export with the new label: %+v", res)
	}
}

// "Tạo label THE" on an order whose paid package THE killed meanwhile must not
// hand the dead one back as "already done" — it makes a new one at once.
func TestTHE_LabelAgainAfterTHECancelMakesANewOne(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100052")
	e.send(t, o)
	old := e.shipment(t, o.ID)
	e.cancelOnTHE(old.OrderNumber, "canceled")

	res := e.send(t, o)
	if len(res.Shipped) != 1 || res.Shipped[0].THE == nil || res.Shipped[0].THE.Reused {
		t.Fatalf("must not reuse the cancelled package: %+v", res)
	}
	if e.fake.delivers != 2 || e.fake.chargedCount() != 1 {
		t.Fatalf("want a second paid delivery: delivers=%d charged=%d", e.fake.delivers, e.fake.chargedCount())
	}
}

// Printing asks THE first: a label THE killed is refused (and voided) even
// before the poll gets to it.
func TestTHE_PrintRefusesALabelTHEKilled(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100053")
	e.send(t, o)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	if _, err := e.carrier.Label(context.Background(), owner, o.ID); err != nil {
		t.Fatalf("live label must print: %v", err)
	}
	e.cancelOnTHE(e.shipment(t, o.ID).OrderNumber, "expired")
	_, err := e.carrier.Label(context.Background(), owner, o.ID)
	if err == nil || !strings.Contains(err.Error(), theCancelledPrefix) {
		t.Fatalf("an expired label must be refused with the reason, got %v", err)
	}
}

// An alive package is only recorded (status + check time), the poll does not
// ask THE about it again within recheckEvery, a number CS typed is never
// cleared, and the manual check works.
func TestTHE_RecheckRecordsAliveAndRespectsInterval(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100054")
	e.send(t, o)
	e.db.Model(&models.Order{}).Where("id = ?", o.ID).Update("tracking_number", "CS-TYPED-9")
	e.ageChecks()

	gets := e.fake.gets
	if n, _ := e.carrier.SyncLabeledStatus(context.Background(), 30); n != 0 {
		t.Fatalf("alive package voided: %d", n)
	}
	sh := e.shipment(t, o.ID)
	if sh.TheStatus != "pre-transit" || sh.StatusCheckedAt == nil {
		t.Fatalf("status not recorded: %q %v", sh.TheStatus, sh.StatusCheckedAt)
	}
	if e.fake.gets != gets+1 {
		t.Fatalf("one ask per package, got %d", e.fake.gets-gets)
	}
	e.carrier.SyncLabeledStatus(context.Background(), 30)
	if e.fake.gets != gets+1 {
		t.Fatal("asked again before recheckEvery")
	}
	// Older check → asked again.
	e.db.Model(&models.CarrierShipment{}).Where("id = ?", sh.ID).Update("status_checked_at", time.Now().Add(-2*recheckEvery))
	e.cancelOnTHE(sh.OrderNumber, "canceled")
	if n, _ := e.carrier.SyncLabeledStatus(context.Background(), 30); n != 1 {
		t.Fatal("stale check must be redone and catch the cancel")
	}
	if got := e.orderRow(t, o.ID).TrackingNumber; got != "CS-TYPED-9" {
		t.Fatalf("a number CS typed must never be cleared, got %q", got)
	}
}

// The order screen's "Kiểm tra với THE" catches it immediately.
func TestTHE_ManualCheckCatchesCancel(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100055")
	e.send(t, o)
	e.cancelOnTHE(e.shipment(t, o.ID).OrderNumber, "canceled")
	rows, err := e.carrier.CheckOrderOnTHE(context.Background(), Actor{ID: 1, Role: models.RoleOwner}, o.ID)
	if err != nil || len(rows) == 0 || rows[0].Status != models.ShipmentCancelled || rows[0].HasLabel {
		t.Fatalf("manual check: err=%v rows=%+v", err, rows)
	}
}

// A cancelled package keeps showing its THE code. A package held for THE's staff
// that they cancelled must therefore not be adopted as "handled, here is your
// label": the order stops waiting and needs a new label.
func TestTHE_HeldPackageCancelledIsNotAdopted(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100056")
	e.send(t, o)
	sh := e.shipment(t, o.ID)
	// Held for THE's staff: unpaid row, package cancelled there but with a code.
	e.db.Model(&models.CarrierShipment{}).Where("id = ?", sh.ID).Updates(map[string]any{
		"status": models.ShipmentCreated, "error": heldOnTHEPrefix + "line không nhận", "tracking_code": "",
	})
	e.cancelOnTHE(sh.OrderNumber, "canceled")

	got, err := e.carrier.SyncHeld(context.Background(), 50)
	if err != nil || got != 0 {
		t.Fatalf("a cancelled held package must not count as handled: got=%d err=%v", got, err)
	}
	var fresh models.CarrierShipment
	e.db.First(&fresh, sh.ID)
	if fresh.Status != models.ShipmentCancelled || !strings.HasPrefix(fresh.Error, theCancelledPrefix) {
		t.Fatalf("held row must be closed: %s %q", fresh.Status, fresh.Error)
	}
}

// After an unclear delivery FFM asks THE what happened: a dead package with a
// code is not "delivered after all".
func TestTHE_UnclearDeliveryDoesNotAdoptADeadPackage(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100057")
	e.send(t, o)
	sh := e.shipment(t, o.ID)
	e.db.Model(&models.CarrierShipment{}).Where("id = ?", sh.ID).Updates(map[string]any{
		"status": models.ShipmentCreated, "error": unclearDeliveryPrefix + "timeout", "tracking_code": "",
	})
	e.age(t, o.ID, 2*unclearDeliveryWait)
	e.cancelOnTHE(sh.OrderNumber, "canceled")

	res := e.send(t, o)
	if len(res.Shipped) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("must not be adopted as paid: %+v", res)
	}
	var got models.CarrierShipment
	e.db.First(&got, sh.ID)
	if got.Status == models.ShipmentLabeled {
		t.Fatal("a cancelled package was adopted as a paid label")
	}
}

// DELIVERED is real history: the record is closed and a human is told, but the
// order keeps its status and number. SHIPPED is not delivered yet: back to queue.
func TestTHE_CancelAfterShippedAndDelivered(t *testing.T) {
	for _, tc := range []struct {
		status models.SellerStatus
		back   bool
	}{{models.SellerStatusDelivered, false}, {models.SellerStatusShipped, true}} {
		e := newTHEEnv(t)
		o := e.order(t, "100058")
		e.send(t, o)
		sh := e.shipment(t, o.ID)
		e.db.Model(&models.Order{}).Where("id = ?", o.ID).Update("seller_status", tc.status)
		e.cancelOnTHE(sh.OrderNumber, "canceled")
		e.ageChecks()
		if n, _ := e.carrier.SyncLabeledStatus(context.Background(), 30); n != 1 {
			t.Fatalf("%s: not voided", tc.status)
		}
		fresh := e.orderRow(t, o.ID)
		if tc.back && (fresh.SellerStatus != models.SellerStatusProduction || fresh.TrackingNumber != "") {
			t.Fatalf("%s must go back to the queue without the dead number: %s %q", tc.status, fresh.SellerStatus, fresh.TrackingNumber)
		}
		if !tc.back && (fresh.SellerStatus != tc.status || fresh.TrackingNumber == "") {
			t.Fatalf("%s must be kept as is: %s %q", tc.status, fresh.SellerStatus, fresh.TrackingNumber)
		}
		var notes int64
		e.db.Model(&models.Note{}).Where("reason_code = ?", "THE_CANCELLED").Count(&notes)
		if notes != 1 {
			t.Fatalf("%s: want exactly one note, got %d", tc.status, notes)
		}
	}
}

// THE unreachable must never kill a label: printing still works from the stored
// copy, Xuất kho still goes, the poll gives up the round after the first failure
// and nothing is voided.
func TestTHE_UnreachableTHEChangesNothing(t *testing.T) {
	e := newTHEEnv(t)
	o1, o2 := e.order(t, "100059"), e.order(t, "100060")
	e.send(t, o1)
	e.send(t, o2)
	e.ageChecks()
	e.fake.getErr = fmt.Errorf("%w: timeout", theapi.ErrTransport)

	gets := e.fake.gets
	n, err := e.carrier.SyncLabeledStatus(context.Background(), 30)
	if err != nil || n != 0 {
		t.Fatalf("poll: n=%d err=%v", n, err)
	}
	if e.fake.gets != gets+1 {
		t.Fatalf("the round must stop at the first failure, asked %d times", e.fake.gets-gets)
	}
	if _, err := e.carrier.Label(context.Background(), Actor{ID: 1, Role: models.RoleOwner}, o1.ID); err != nil {
		t.Fatalf("stored label must still print: %v", err)
	}
	if res := e.send(t, o1, ShipSendOptions{ExportOnly: true}); len(res.Shipped) != 1 {
		t.Fatalf("export must still go: %+v", res)
	}
	if e.shipment(t, o1.ID).Status != models.ShipmentLabeled {
		t.Fatal("a label was voided on a network error")
	}
}

// The ship queue names the order whose label THE killed, and stops once it has
// a new one.
func TestTHE_ShipQueueFlagsTheKilledLabel(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100061")
	e.send(t, o)
	sh := e.shipment(t, o.ID)
	e.cancelOnTHE(sh.OrderNumber, "canceled")
	e.ageChecks()
	e.carrier.SyncLabeledStatus(context.Background(), 30)

	latest, err := e.carrier.repo.Carrier.LatestShipmentsByOrder([]uint{o.ID})
	if err != nil {
		t.Fatal(err)
	}
	last := latest[o.ID]
	if !cancelledByTHE(&last) {
		t.Fatalf("latest shipment must read as killed by THE: %+v", last.Status)
	}
	e.send(t, o)
	latest, _ = e.carrier.repo.Carrier.LatestShipmentsByOrder([]uint{o.ID})
	last = latest[o.ID]
	if cancelledByTHE(&last) || last.Status != models.ShipmentLabeled {
		t.Fatalf("a new label must clear the flag: %s", last.Status)
	}
}

// An order FFM cancelled itself is not "killed by THE": no block, no note.
func TestTHE_OwnCancelIsNotFlagged(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100062")
	e.send(t, o)
	if _, err := e.carrier.CancelShipment(context.Background(), Actor{ID: 1, Role: models.RoleOwner}, o.ID, "nhầm"); err != nil {
		t.Fatal(err)
	}
	if reason := e.carrier.ExportLabelBlock(context.Background(), o.ID); reason != "" {
		t.Fatalf("FFM's own cancel must not block export: %s", reason)
	}
	var notes int64
	e.db.Model(&models.Note{}).Where("reason_code = ?", "THE_CANCELLED").Count(&notes)
	if notes != 0 {
		t.Fatalf("no THE_CANCELLED note for FFM's own cancel, got %d", notes)
	}
}
