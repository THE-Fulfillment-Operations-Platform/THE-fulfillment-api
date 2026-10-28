package services

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/shipping/theapi"
)

// THE keeps a package for its own staff: the account's line takes postcards
// only (THE answers "Route unavailable" for anything else), or the carrier
// refused a postcard after the label was bought and THE took it back to
// pending. FFM shows THE's sentence, never pays or asks again on its own, and
// takes the code and label once THE's staff have handled the package.

const routeSentence = "Gặp sự cố đường bay, báo nhân viên THE xử lý"

// pkgFor finds the fake package of an order number (the live one).
func (f *fakeTHE) pkgFor(orderNumber string) *fakePkg {
	f.mu.Lock()
	defer f.mu.Unlock()
	var last *fakePkg
	for _, p := range f.pkgs {
		if p.orderNumber == orderNumber && p.status != "archived" && p.status != "canceled" {
			last = p
		}
	}
	return last
}

// staffDeliver: THE's staff create the tracking on THE themselves.
func (f *fakeTHE) staffDeliver(orderNumber string) {
	p := f.pkgFor(orderNumber)
	f.mu.Lock()
	defer f.mu.Unlock()
	p.status, p.charged, p.code = "pre-transit", true, "THE00009"+p.id
}

// carrierReturns: the carrier refuses a delivered package; THE refunds it and
// shows it pending again without its code.
func (f *fakeTHE) carrierReturns(orderNumber string) {
	p := f.pkgFor(orderNumber)
	f.mu.Lock()
	defer f.mu.Unlock()
	p.status, p.charged, p.code = "pending", false, ""
}

func (e *theEnv) postcardSKU(t *testing.T) {
	t.Helper()
	if err := e.db.Model(&models.SKU{}).Where("id = ?", e.sku.ID).Update("hs_code", "4909002000347").Error; err != nil {
		t.Fatal(err)
	}
}

func TestTHE_OtherGoodsAreHeldForTHEStaff(t *testing.T) {
	e := newTHEEnv(t)
	e.fake.onlyHS = "4909002000"
	o := e.order(t, "100201") // SKU-1: wood, HS 44209000

	res := e.send(t, o)
	if len(res.Skipped) != 1 || res.Skipped[0].Code != ShipErrHeld || res.Skipped[0].Reason != routeSentence {
		t.Fatalf("want THE's sentence as it is, got %+v", res)
	}
	if e.fake.chargedCount() != 0 {
		t.Fatal("a held package must not be paid")
	}
	sh := e.shipment(t, o.ID)
	if !IsHeldOnTHE(sh) || sh.ExternalID == "" || sh.TrackingCode != "" {
		t.Fatalf("shipment must be held on THE: %+v", sh)
	}

	// sent again: same sentence, and FFM does not ask THE to deliver again
	delivers := e.fake.delivers
	res = e.send(t, o)
	if len(res.Skipped) != 1 || res.Skipped[0].Code != ShipErrHeld || res.Skipped[0].Reason != routeSentence {
		t.Fatalf("second send: %+v", res)
	}
	if e.fake.delivers != delivers || e.fake.creates != 1 {
		t.Fatalf("held: delivers %d→%d creates %d", delivers, e.fake.delivers, e.fake.creates)
	}

	// the poll finds nothing to take while THE's staff have not acted
	if got, err := e.carrier.SyncHeld(context.Background(), 50); err != nil || got != 0 {
		t.Fatalf("poll before staff: %d %v", got, err)
	}

	// THE's staff create the tracking on THE: the poll takes code, label and the
	// last-mile number, with no new package and no payment from FFM
	e.fake.staffDeliver("100201")
	if got, err := e.carrier.SyncHeld(context.Background(), 50); err != nil || got != 1 {
		t.Fatalf("poll after staff: %d %v", got, err)
	}
	sh = e.shipment(t, o.ID)
	if sh.Status != models.ShipmentLabeled || !strings.HasPrefix(sh.TrackingCode, "THE00009") || sh.Error != "" || len(sh.Label) == 0 {
		t.Fatalf("adopted shipment: %+v", sh)
	}
	var fresh models.Order
	e.db.First(&fresh, o.ID)
	if fresh.TrackingNumber != e.fake.lastMile {
		t.Fatalf("last-mile number must reach the order, got %q", fresh.TrackingNumber)
	}
	if e.fake.creates != 1 || e.fake.delivers != delivers {
		t.Fatalf("adoption must not call THE's paid step: creates %d delivers %d", e.fake.creates, e.fake.delivers)
	}
	// and a send now reuses it
	if res := e.send(t, o); len(res.Shipped) != 1 || !res.Shipped[0].THE.Reused {
		t.Fatalf("send after adoption: %+v", res)
	}
}

// The operator sends again after THE's staff handled it but before the poll:
// the send takes the code instead of asking THE to deliver a delivered package.
func TestTHE_SendTakesWhatTHEStaffDid(t *testing.T) {
	e := newTHEEnv(t)
	e.fake.onlyHS = "4909002000"
	o := e.order(t, "100202")
	e.send(t, o)
	e.fake.staffDeliver("100202")
	res := e.send(t, o)
	if len(res.Shipped) != 1 || !strings.HasPrefix(res.Shipped[0].THE.TrackingCode, "THE00009") || res.Shipped[0].THE.Reused {
		t.Fatalf("send after staff: %+v", res)
	}
	if e.fake.delivers != 1 {
		t.Fatalf("only the first, refused delivery may have been asked: %d", e.fake.delivers)
	}
}

func TestTHE_PostcardsGoThrough(t *testing.T) {
	e := newTHEEnv(t)
	e.fake.onlyHS = "4909002000"
	e.postcardSKU(t)
	o := e.order(t, "100203")
	res := e.send(t, o)
	if len(res.Shipped) != 1 || len(res.Skipped) != 0 || !res.Shipped[0].THE.HasLabel {
		t.Fatalf("postcard: %+v", res)
	}
	if sh := e.shipment(t, o.ID); sh.Status != models.ShipmentLabeled || sh.HSCode != "4909002000347" {
		t.Fatalf("postcard shipment: %+v", sh)
	}
}

// A postcard the carrier refused after delivery (JD answers minutes later):
// THE refunds it and shows it pending again. FFM voids the code and label,
// holds it, does not buy it again, and takes the new code once THE's staff
// have fixed and re-created it.
func TestTHE_ReturnedPostcardIsHeldUntilTHEStaffAct(t *testing.T) {
	e := newTHEEnv(t)
	e.fake.onlyHS = "4909002000"
	e.fake.lastMile = "" // JD: no last-mile number yet
	e.postcardSKU(t)
	o := e.order(t, "100204")
	if res := e.send(t, o); len(res.Shipped) != 1 {
		t.Fatalf("first send: %+v", res)
	}
	oldCode := e.shipment(t, o.ID).TrackingCode

	e.fake.carrierReturns("100204")
	if _, err := e.carrier.SyncLastMile(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	sh := e.shipment(t, o.ID)
	if !IsHeldOnTHE(sh) || sh.TrackingCode != "" || len(sh.Label) != 0 || sh.LabeledAt != nil ||
		!strings.Contains(sh.Error, oldCode) {
		t.Fatalf("returned shipment must be held, code and label void: %+v", sh)
	}

	// the operator sends again: held, no second purchase
	delivers := e.fake.delivers
	res := e.send(t, o)
	if len(res.Skipped) != 1 || res.Skipped[0].Code != ShipErrHeld || !strings.Contains(res.Skipped[0].Reason, "THE đã đưa đơn về chờ xử lý") {
		t.Fatalf("send while returned: %+v", res)
	}
	if e.fake.delivers != delivers || e.fake.chargedCount() != 0 {
		t.Fatalf("a returned package must not be bought again: delivers %d→%d", delivers, e.fake.delivers)
	}

	// THE's staff fix it and create the tracking again
	e.fake.lastMile = "9400111899223344550000"
	e.fake.staffDeliver("100204")
	if got, err := e.carrier.SyncHeld(context.Background(), 50); err != nil || got != 1 {
		t.Fatalf("poll after staff: %d %v", got, err)
	}
	sh = e.shipment(t, o.ID)
	var fresh models.Order
	e.db.First(&fresh, o.ID)
	if sh.Status != models.ShipmentLabeled || sh.TrackingCode == "" || fresh.TrackingNumber != "9400111899223344550000" {
		t.Fatalf("after staff: %+v, order tracking %q", sh, fresh.TrackingNumber)
	}
}

// The order's data changed in FFM while held (e.g. the address fixed): that
// is a new package — the held one is archived on THE and the new one sent.
func TestTHE_HeldPackageReplacedWhenOrderChanges(t *testing.T) {
	e := newTHEEnv(t)
	e.fake.onlyHS = "4909002000"
	e.postcardSKU(t)
	e.fake.lastMile = ""
	o := e.order(t, "100205")
	e.send(t, o)
	e.fake.carrierReturns("100205")
	e.carrier.SyncLastMile(context.Background(), 50)

	e.db.Model(&models.Order{}).Where("id = ?", o.ID).Update("shipping_address1", "1427 Harrison Street")
	res := e.send(t, o)
	if len(res.Shipped) != 1 || e.fake.cancels != 1 || e.fake.creates != 2 || e.fake.chargedCount() != 1 {
		t.Fatalf("replace: %+v cancels %d creates %d charged %d", res, e.fake.cancels, e.fake.creates, e.fake.chargedCount())
	}
}

// The scan station shows THE's sentence as the error of the scan.
func TestTHE_ScanShowsTHESentence(t *testing.T) {
	e := newTHEEnv(t)
	e.fake.onlyHS = "4909002000"
	o := e.order(t, "100206")
	_, err := e.packing.ShipScannedOrderWith(context.Background(), opsActor(), o.InternalCode, ShipSendOptions{})
	ae, ok := apperr.As(err)
	if !ok || ae.Message != routeSentence || ae.Status != http.StatusConflict {
		t.Fatalf("scan: %v", err)
	}
}

func TestTHEErrorMessage_RouteUnavailable(t *testing.T) {
	err := &theapi.Error{Status: 400, Message: "Route unavailable", Messages: []string{routeSentence}}
	if got := theErrorMessage(err); got != routeSentence {
		t.Fatalf("got %q", got)
	}
	var te *theapi.Error
	if !errors.As(error(err), &te) || !te.RouteUnavailable() {
		t.Fatal("RouteUnavailable")
	}
	other := &theapi.Error{Status: 400, Message: "Bad request", Messages: []string{"Failed to create labels"}}
	if other.RouteUnavailable() || theErrorMessage(other) != "THE từ chối: Failed to create labels" {
		t.Fatalf("other refusal: %q", theErrorMessage(other))
	}
}
