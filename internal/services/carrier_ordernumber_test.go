package services

import (
	"context"
	"strings"
	"testing"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/shipping/theapi"
)

// THE shows (and CS search by) the order number FFM files a package under:
// the shop's order id. A shop's order id repeats in FFM and may also be on a
// package CS filed on THE, while THE merges a create into any pending package
// with the same number: FFM never adopts, pays or archives a package that is
// not this order's, and files under "<shop id>-<internal code>" instead.

func (e *theEnv) shopID(t *testing.T, o *models.Order, id string) {
	t.Helper()
	if err := e.db.Model(&models.Order{}).Where("id = ?", o.ID).Update("store_order_id", id).Error; err != nil {
		t.Fatal(err)
	}
}

func (f *fakeTHE) byNumber(number string) []*fakePkg {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakePkg
	for _, p := range f.pkgs {
		if p.orderNumber == number {
			out = append(out, p)
		}
	}
	return out
}

func TestTHE_FilesUnderTheShopOrderID(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100301")
	e.shopID(t, o, "TEST-1")
	if res := e.send(t, o); len(res.Shipped) != 1 {
		t.Fatalf("send: %+v", res)
	}
	if got := e.fake.byNumber("TEST-1"); len(got) != 1 || !got[0].charged {
		t.Fatalf("THE must hold one paid package under the shop's order id, got %+v", got)
	}
	if sh := e.shipment(t, o.ID); sh.OrderNumber != "TEST-1" {
		t.Fatalf("shipment order number %q", sh.OrderNumber)
	}
}

func TestTHE_NoShopOrderIDFilesUnderTheInternalCode(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100302")
	e.shopID(t, o, "  ")
	e.send(t, o)
	if got := e.fake.byNumber("100302"); len(got) != 1 {
		t.Fatalf("want the internal code, packages %+v", e.fake.pkgs)
	}
}

// Another FFM order already has a package pending under the shop's order id.
func TestTHE_ShopOrderIDPendingOnAnotherOrder(t *testing.T) {
	e := newTHEEnv(t)
	a := e.order(t, "100303")
	b := e.order(t, "100304")
	e.shopID(t, a, "DUP-1")
	e.shopID(t, b, "DUP-1")
	e.fake.deliverErr = &theapi.Error{Status: 400, Messages: []string{"The balance in the wallet is not enough"}}
	e.send(t, a) // stays pending on THE
	if res := e.send(t, b); len(res.Shipped) != 1 {
		t.Fatalf("second order: %+v", res)
	}
	if sh := e.shipment(t, b.ID); sh.OrderNumber != "DUP-1-100304" {
		t.Fatalf("second order's number %q", sh.OrderNumber)
	}
	pa := e.fake.byNumber("DUP-1")
	if len(pa) != 1 || pa[0].status != "pending" || pa[0].charged {
		t.Fatalf("the first order's pending package must be left alone: %+v", pa)
	}
}

// CS filed the same shop order on THE (other data, still pending): FFM must
// neither archive nor pay it.
func TestTHE_ForeignPendingPackageIsNeverTouched(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100305")
	e.shopID(t, o, "SHOP-9")
	e.fake.pkgs["777"] = &fakePkg{id: "777", orderNumber: "SHOP-9", status: "pending",
		req: theapi.CreatePackageRequest{OrderNumber: "SHOP-9", Weight: 999, Length: 9, Width: 9, Height: 9, Value: 9, Zipcode: "10001", Address1: "1 CS Street", HSCode: "44209000"}}
	if res := e.send(t, o); len(res.Shipped) != 1 {
		t.Fatalf("send: %+v", res)
	}
	cs := e.fake.pkgs["777"]
	if cs.status != "pending" || cs.charged || e.fake.cancels != 0 {
		t.Fatalf("CS's package was touched: %+v, cancels %d", cs, e.fake.cancels)
	}
	sh := e.shipment(t, o.ID)
	if sh.OrderNumber != "SHOP-9-100305" || sh.ExternalID == "777" || e.fake.chargedCount() != 1 {
		t.Fatalf("FFM's own package: %+v charged %d", sh, e.fake.chargedCount())
	}
}

// A package still pending on THE that another FFM order holds (its row closed
// in FFM), same data: not adopted, even though THE hands it back.
func TestTHE_PackageOfAnotherFFMOrderIsNotAdopted(t *testing.T) {
	e := newTHEEnv(t)
	a := e.order(t, "100306")
	b := e.order(t, "100307")
	e.shopID(t, a, "SAME-1")
	e.shopID(t, b, "SAME-1")
	e.fake.deliverErr = &theapi.Error{Status: 400, Messages: []string{"The balance in the wallet is not enough"}}
	e.send(t, a)
	held := e.shipment(t, a.ID)
	e.db.Model(&models.CarrierShipment{}).Where("id = ?", held.ID).Update("status", models.ShipmentFailed)

	if res := e.send(t, b); len(res.Shipped) != 1 {
		t.Fatalf("second order: %+v", res)
	}
	sb := e.shipment(t, b.ID)
	if sb.ExternalID == held.ExternalID || sb.OrderNumber != "SAME-1-100307" {
		t.Fatalf("adopted another order's package: %+v (theirs %s)", sb, held.ExternalID)
	}
	if p := e.fake.pkgs[held.ExternalID]; p.charged {
		t.Fatal("the other order's package must not be paid")
	}
}

// A package filed before the change, under the internal code, keeps it: no
// re-creation just because the order now has a shop id to show.
func TestTHE_PendingPackageKeepsItsOrderNumber(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100308")
	e.shopID(t, o, "")
	e.fake.deliverErr = &theapi.Error{Status: 400, Messages: []string{"The balance in the wallet is not enough"}}
	e.send(t, o)
	e.shopID(t, o, "LATER-1")
	if res := e.send(t, o); len(res.Shipped) != 1 {
		t.Fatalf("resend: %+v", res)
	}
	if e.fake.creates != 1 || e.fake.cancels != 0 || e.shipment(t, o.ID).OrderNumber != "100308" {
		t.Fatalf("creates %d cancels %d number %q", e.fake.creates, e.fake.cancels, e.shipment(t, o.ID).OrderNumber)
	}
}

func TestTHE_ShopOrderIDTooLongFallsBack(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100309")
	e.shopID(t, o, strings.Repeat("X", 60))
	e.send(t, o)
	if e.shipment(t, o.ID).OrderNumber != "100309" {
		t.Fatalf("number %q", e.shipment(t, o.ID).OrderNumber)
	}
}

func TestFileSafe(t *testing.T) {
	if got := fileSafe("#BW149485-1-C1 / AM 42.x"); got != "_BW149485-1-C1___AM_42.x" {
		t.Fatalf("got %q", got)
	}
	_ = context.Background
}

// Sent under the shop's order id, refused at the paid step, sent again: the
// same pending package is paid, nothing is re-created or archived.
func TestTHE_ResendKeepsTheShopOrderID(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100310")
	e.shopID(t, o, "AM-435-US-NG 4213041887")
	e.fake.deliverErr = &theapi.Error{Status: 400, Messages: []string{"The balance in the wallet is not enough"}}
	e.send(t, o)
	if res := e.send(t, o); len(res.Shipped) != 1 {
		t.Fatalf("resend: %+v", res)
	}
	if e.fake.creates != 1 || e.fake.cancels != 0 || e.fake.chargedCount() != 1 {
		t.Fatalf("creates %d cancels %d charged %d", e.fake.creates, e.fake.cancels, e.fake.chargedCount())
	}
	if got := e.fake.byNumber("AM-435-US-NG 4213041887"); len(got) != 1 || !got[0].charged {
		t.Fatalf("packages under the shop id: %+v", got)
	}
}
