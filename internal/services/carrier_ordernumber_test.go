package services

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

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

// Đơn bảo hành (#3092) đi THE như đơn thường: mã shop "SON1-BH1" khác mã gốc
// "SON1", nên mua label không đụng package của đơn gốc và không bị chặn. Seller
// không bị tính khoản nào — FFM không có phí ship tính seller, chi phí label
// chỉ ghi ở carrier_shipments.
func TestTHE_WarrantyOrderBuysItsLabelLikeAnyOrder(t *testing.T) {
	e := newTHEEnv(t)
	root := e.order(t, "100401")
	e.shopID(t, root, "SON1")
	if res := e.send(t, root); len(res.Shipped) != 1 {
		t.Fatalf("root send: %+v", res)
	}
	bh := e.order(t, "100402")
	e.shopID(t, bh, "SON1-BH1")
	res := e.send(t, bh)
	if len(res.Shipped) != 1 || len(res.Skipped) != 0 {
		t.Fatalf("-BH1 must ship normally: %+v", res)
	}
	if got := e.fake.byNumber("SON1-BH1"); len(got) != 1 || !got[0].charged {
		t.Fatalf("THE must hold one paid package under SON1-BH1, got %+v", got)
	}
	if got := e.fake.byNumber("SON1"); len(got) != 1 {
		t.Fatalf("root package must stay alone under SON1, got %+v", got)
	}
	if sh := e.shipment(t, bh.ID); sh.OrderNumber != "SON1-BH1" {
		t.Fatalf("shipment order number %q", sh.OrderNumber)
	}
}

// ---------- THE's rule from 27/11/2026: no two live packages under one number ----------
//
// THE refuses (duplicate_order_number) a create whose order number already has
// a package with a label or a THE code that is not cancelled; a pending one is
// still merged into. A shop's order id repeats in FFM (split orders, re-imports),
// so FFM must not file a second order under a number another FFM order holds on
// THE, and when THE refuses one anyway (a package filed outside FFM), it files
// once more under "<shop id>-<internal code>".

// lastShipment is the order's newest shipment row, live or not.
func (e *theEnv) lastShipment(t *testing.T, orderID uint) models.CarrierShipment {
	t.Helper()
	rows, err := e.carrier.repo.Carrier.ShipmentsForOrder(orderID)
	if err != nil || len(rows) == 0 {
		t.Fatalf("no shipment rows for order %d: %v", orderID, err)
	}
	return rows[0]
}

// Nobody holds the shop's order id: it goes to THE as it is, no refusal.
func TestTHE_FreeShopOrderIDGoesBare(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100501")
	e.shopID(t, o, "#BW159368-1-S1-C1")
	if res := e.send(t, o); len(res.Shipped) != 1 {
		t.Fatalf("send: %+v", res)
	}
	if sh := e.shipment(t, o.ID); sh.OrderNumber != "#BW159368-1-S1-C1" {
		t.Fatalf("number %q", sh.OrderNumber)
	}
	if e.fake.creates != 1 || e.fake.dupRefusals != 0 {
		t.Fatalf("creates %d refusals %d", e.fake.creates, e.fake.dupRefusals)
	}
}

// Another FFM order already has a PAID label under the shop's order id (it was
// not pending any more, so the old check let the bare number through): the
// second order is filed under "<shop id>-<internal code>" straight away —
// matched trimmed and in any case, as THE matches it. THE never has to refuse.
func TestTHE_ShopOrderIDLabeledOnAnotherOrderGetsSuffix(t *testing.T) {
	e := newTHEEnv(t)
	a := e.order(t, "100502")
	b := e.order(t, "100503")
	e.shopID(t, a, "#BW159368-1-S1-C1")
	e.shopID(t, b, "  #bw159368-1-s1-c1 ")
	if res := e.send(t, a); len(res.Shipped) != 1 {
		t.Fatalf("first order: %+v", res)
	}
	if sa := e.shipment(t, a.ID); sa.Status != models.ShipmentLabeled || sa.OrderNumber != "#BW159368-1-S1-C1" {
		t.Fatalf("first order's shipment %+v", sa)
	}
	if res := e.send(t, b); len(res.Shipped) != 1 {
		t.Fatalf("second order: %+v", res)
	}
	sb := e.shipment(t, b.ID)
	if sb.OrderNumber != "#bw159368-1-s1-c1-100503" || sb.Status != models.ShipmentLabeled {
		t.Fatalf("second order's shipment %q %s", sb.OrderNumber, sb.Status)
	}
	if e.fake.creates != 2 || e.fake.dupRefusals != 0 || e.fake.chargedCount() != 2 {
		t.Fatalf("creates %d refusals %d charged %d", e.fake.creates, e.fake.dupRefusals, e.fake.chargedCount())
	}
	if got := e.fake.byNumber("#BW159368-1-S1-C1"); len(got) != 1 || !got[0].charged || got[0].status != "pre-transit" {
		t.Fatalf("the first order's package must be left alone: %+v", got)
	}
}

// The other order's label was cancelled (THE refunded it), or its create
// failed: it does not hold the number any more, the bare shop id is used.
func TestTHE_CancelledOrFailedShipmentDoesNotHoldTheNumber(t *testing.T) {
	e := newTHEEnv(t)
	a := e.order(t, "100504")
	e.shopID(t, a, "CAN-1")
	e.send(t, a)
	if _, err := e.carrier.CancelShipment(context.Background(), Actor{ID: 1, Role: models.RoleOwner}, a.ID, "nhầm đơn"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	f := e.order(t, "100505")
	e.shopID(t, f, "CAN-1")
	e.fake.createErr = &theapi.Error{Status: 400, Messages: []string{"The weight is required"}}
	e.send(t, f)
	if sf := e.lastShipment(t, f.ID); sf.Status != models.ShipmentFailed || sf.OrderNumber != "CAN-1" {
		t.Fatalf("failed order's row %+v", sf)
	}

	b := e.order(t, "100506")
	e.shopID(t, b, "CAN-1")
	if res := e.send(t, b); len(res.Shipped) != 1 {
		t.Fatalf("send: %+v", res)
	}
	if sb := e.shipment(t, b.ID); sb.OrderNumber != "CAN-1" {
		t.Fatalf("a cancelled / failed shipment must not hold the number, got %q", sb.OrderNumber)
	}
	if e.fake.dupRefusals != 0 {
		t.Fatalf("THE refused %d times", e.fake.dupRefusals)
	}
}

// A package with a label under the shop's order id that FFM does not know
// (filed on THE by CS, or before FFM recorded it): THE refuses the bare number,
// FFM files ONCE more under "<shop id>-<internal code>" and pays only that.
func TestTHE_DuplicateRefusedRetriesOnceWithSuffix(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100507")
	e.shopID(t, o, "#BW159368-1-S1-C1")
	e.fake.pkgs["900"] = &fakePkg{id: "900", orderNumber: "#BW159368-1-S1-C1", status: "pre-transit", code: "THE000900", charged: true,
		req: theapi.CreatePackageRequest{OrderNumber: "#BW159368-1-S1-C1", Weight: 999}}
	res := e.send(t, o)
	if len(res.Shipped) != 1 {
		t.Fatalf("send: %+v", res)
	}
	sh := e.shipment(t, o.ID)
	if sh.OrderNumber != "#BW159368-1-S1-C1-100507" || sh.Status != models.ShipmentLabeled || sh.Error != "" {
		t.Fatalf("shipment %q %s %q", sh.OrderNumber, sh.Status, sh.Error)
	}
	if e.fake.creates != 2 || e.fake.dupRefusals != 1 || e.fake.delivers != 1 || e.fake.chargedCount() != 2 {
		t.Fatalf("creates %d refusals %d delivers %d charged %d", e.fake.creates, e.fake.dupRefusals, e.fake.delivers, e.fake.chargedCount())
	}
	if got := e.fake.byNumber("#BW159368-1-S1-C1-100507"); len(got) != 1 || !got[0].charged || got[0].id != sh.ExternalID {
		t.Fatalf("FFM's own package: %+v", got)
	}
	if cs := e.fake.pkgs["900"]; cs.status != "pre-transit" || e.fake.cancels != 0 {
		t.Fatalf("the package FFM does not own was touched: %+v cancels %d", cs, e.fake.cancels)
	}
}

// Until THE's exact error shape is settled: a 400/409 whose words say
// "duplicate" / "đã tồn tại" / "already exists" counts too, code or not.
func TestTHE_DuplicateRefusalShapes(t *testing.T) {
	shapes := []*theapi.Error{
		{Status: 400, Code: "duplicate_order_number"},
		{Status: 422, Message: "duplicate_order_number"},
		{Status: 409, Messages: []string{"Order number already exists"}},
		{Status: 400, Message: "Please check input data", Messages: []string{"Mã đơn hàng đã tồn tại"}},
		{Status: 400, Messages: []string{"Duplicate order number #X"}},
	}
	for i, refusal := range shapes {
		e := newTHEEnv(t)
		code := strconv.Itoa(100510 + i)
		o := e.order(t, code)
		e.shopID(t, o, "SHAPE-1")
		e.fake.createErr = refusal
		if res := e.send(t, o); len(res.Shipped) != 1 {
			t.Fatalf("%v: send %+v", refusal, res)
		}
		if sh := e.shipment(t, o.ID); sh.OrderNumber != "SHAPE-1-"+code || e.fake.creates != 2 {
			t.Fatalf("%v: number %q creates %d", refusal, sh.OrderNumber, e.fake.creates)
		}
	}
	// Other refusals are not duplicates: no retry, the order fails as before.
	for _, other := range []*theapi.Error{
		{Status: 400, Messages: []string{"HS code does not exist"}},
		{Status: 403, Messages: []string{"Order number already exists"}},
		{Status: 400, Message: "Route unavailable"},
	} {
		if other.DuplicateOrderNumber() {
			t.Fatalf("%v must not read as a duplicate", other)
		}
	}
}

// THE refuses the suffixed number as well: no third try, nothing paid, the
// row fails with the reason for whoever looks.
func TestTHE_DuplicateRefusedTwiceStops(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100520")
	e.shopID(t, o, "TWICE-1")
	for id, number := range map[string]string{"901": "TWICE-1", "902": "TWICE-1-100520"} {
		e.fake.pkgs[id] = &fakePkg{id: id, orderNumber: number, status: "pre-transit", code: "THE000" + id, charged: true,
			req: theapi.CreatePackageRequest{OrderNumber: number}}
	}
	res := e.send(t, o)
	if len(res.Shipped) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "trùng mã đơn") {
		t.Fatalf("want skipped with the reason, got %+v", res)
	}
	if e.fake.creates != 2 || e.fake.dupRefusals != 2 || e.fake.delivers != 0 || e.fake.chargedCount() != 2 {
		t.Fatalf("creates %d refusals %d delivers %d charged %d", e.fake.creates, e.fake.dupRefusals, e.fake.delivers, e.fake.chargedCount())
	}
	if sh := e.lastShipment(t, o.ID); sh.Status != models.ShipmentFailed || sh.OrderNumber != "TWICE-1-100520" {
		t.Fatalf("row %s %q", sh.Status, sh.OrderNumber)
	}
}

// The first create of the order went unanswered (it did land on THE), and the
// package was paid on THE since. THE now refuses the number — but that package
// may be this very order's: FFM must not file a second one under another
// number (luật sắt: one FFM order never gets two paid THE packages).
func TestTHE_DuplicateAfterUnclearCreateDoesNotRename(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100521")
	e.shopID(t, o, "UNC-1")
	e.fake.createTransportAfterApply = true
	e.send(t, o)
	sh := e.shipment(t, o.ID)
	if sh.Status != models.ShipmentCreating || !strings.HasPrefix(sh.Error, unclearCreatePrefix) {
		t.Fatalf("first send must leave an unclear CREATING row: %+v", sh)
	}
	landed := e.fake.byNumber("UNC-1")
	if len(landed) != 1 {
		t.Fatalf("the lost create must have landed: %+v", landed)
	}
	landed[0].status, landed[0].code, landed[0].charged = "pre-transit", "THE0001UNC", true // paid on THE meanwhile
	e.age(t, o.ID, 3*time.Minute)

	res := e.send(t, o)
	if len(res.Shipped) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "chưa rõ kết quả") {
		t.Fatalf("want stopped for a human, got %+v", res)
	}
	if e.fake.creates != 2 || e.fake.dupRefusals != 1 || e.fake.delivers != 0 || e.fake.chargedCount() != 1 {
		t.Fatalf("creates %d refusals %d delivers %d charged %d", e.fake.creates, e.fake.dupRefusals, e.fake.delivers, e.fake.chargedCount())
	}
	if got := e.fake.byNumber("UNC-1-100521"); len(got) != 0 {
		t.Fatalf("no package may be filed under a second number: %+v", got)
	}
	sh = e.shipment(t, o.ID)
	if sh == nil || sh.Status != models.ShipmentCreating || sh.OrderNumber != "UNC-1" || !strings.HasPrefix(sh.Error, unclearCreatePrefix) {
		t.Fatalf("the row must stay CREATING, unclear, under its number: %+v", sh)
	}
}

// THE stores 60 characters: a shop id with no room for the suffix files under
// the internal code alone; one that fits exactly gets the suffix when taken.
func TestTHE_TakenLongShopOrderIDUsesInternalCode(t *testing.T) {
	long := strings.Repeat("L", 54) // 54 + "-" + 6 digits = 61 > 60
	if got := suffixedOrderNumber(long, "100530"); got != "100530" {
		t.Fatalf("too long: %q", got)
	}
	if got := suffixedOrderNumber(long[:53], "100530"); got != long[:53]+"-100530" {
		t.Fatalf("exactly 60 fits: %q", got)
	}
	if got := suffixedOrderNumber(" #A1 ", "100530"); got != "#A1-100530" {
		t.Fatalf("short: %q", got)
	}

	e := newTHEEnv(t)
	a := e.order(t, "100531")
	b := e.order(t, "100532")
	e.shopID(t, a, long)
	e.shopID(t, b, long)
	e.send(t, a)
	e.send(t, b)
	if na, nb := e.shipment(t, a.ID).OrderNumber, e.shipment(t, b.ID).OrderNumber; na != "100531" || nb != "100532" {
		t.Fatalf("numbers %q %q", na, nb)
	}

	c := e.order(t, "100533")
	d := e.order(t, "100534")
	e.shopID(t, c, long[:53])
	e.shopID(t, d, long[:53])
	e.send(t, c)
	e.send(t, d)
	if nc, nd := e.shipment(t, c.ID).OrderNumber, e.shipment(t, d.ID).OrderNumber; nc != long[:53] || nd != long[:53]+"-100534" {
		t.Fatalf("numbers %q %q", nc, nd)
	}

	// THE refusing the internal code itself: nothing else to try, no retry.
	x := e.order(t, "100535")
	e.shopID(t, x, long)
	e.fake.createErr = &theapi.Error{Status: 400, Code: theapi.DuplicateOrderNumberCode}
	creates := e.fake.creates
	if res := e.send(t, x); len(res.Shipped) != 0 || e.fake.creates != creates+1 {
		t.Fatalf("refused internal code: %+v, creates %d", res, e.fake.creates-creates)
	}
	if sh := e.lastShipment(t, x.ID); sh.Status != models.ShipmentFailed || sh.OrderNumber != "100535" {
		t.Fatalf("row %s %q", sh.Status, sh.OrderNumber)
	}
}
