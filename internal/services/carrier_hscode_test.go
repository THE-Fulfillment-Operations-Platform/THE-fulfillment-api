package services

import (
	"context"
	"strings"
	"testing"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// #6312 (Nghĩa, 19/11/2026): an order whose SKU has no HS code — neither its
// own nor its parent's — must not go to THE. The connection's "Mã HS mặc định"
// (the postcard code) used to fill the gap, so unclassified goods shipped as
// postcards; THE now buys the label on its own for exactly that code.

// hsEnv is a THE env whose connection still carries the old postcard default,
// to prove it is no longer used to fill a SKU.
func hsEnv(t *testing.T) *theEnv {
	t.Helper()
	e := newTHEEnv(t)
	hs := "4909002000347"
	if _, err := e.carrier.UpdateConfig(Actor{ID: 1, Role: models.RoleOwner}, CarrierConfigInput{DefaultHSCode: &hs}); err != nil {
		t.Fatal(err)
	}
	return e
}

// skuNoHS creates a SKU with weight, box and value but no HS code.
func (e *theEnv) skuNoHS(t *testing.T, code string, parentID *uint) *models.SKU {
	t.Helper()
	s := &models.SKU{Code: code, Name: code, ParentID: parentID, ShipWeightG: fptr(100),
		ShipLengthCM: fptr(10), ShipWidthCM: fptr(10), ShipHeightCM: fptr(1), DeclaredValue: fptr(5)}
	if err := e.db.Create(s).Error; err != nil {
		t.Fatal(err)
	}
	return s
}

// orderWith is e.order with one line per SKU code.
func (e *theEnv) orderWith(t *testing.T, code string, skus ...string) *models.Order {
	t.Helper()
	qty := make([]int, len(skus))
	for i := range qty {
		qty[i] = 1
	}
	o := e.order(t, code, qty...)
	for i, s := range skus {
		e.db.Model(&models.OrderItem{}).Where("order_id = ? AND line_no = ?", o.ID, i+1).Update("sku_code", s)
	}
	return o
}

func (e *theEnv) sentHS(orderNumber string) string {
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	for _, p := range e.fake.pkgs {
		if p.orderNumber == orderNumber {
			return p.req.HSCode
		}
	}
	return ""
}

// The SKU declares its own HS code: the order goes with it (not the default).
func TestHSCode_OwnCodeShips(t *testing.T) {
	e := hsEnv(t)
	o := e.order(t, "100601") // SKU-1, HS 44209000
	res := e.send(t, o)
	if len(res.Shipped) != 1 || len(res.Skipped) != 0 {
		t.Fatalf("want shipped, got %+v", res)
	}
	if hs := e.sentHS("SO-100601"); hs != "44209000" {
		t.Fatalf("THE must get the SKU's HS code, got %q", hs)
	}
}

// Only the parent declares the HS code: the child ships with the parent's.
func TestHSCode_ParentCodeShips(t *testing.T) {
	e := hsEnv(t)
	parent := &models.SKU{Code: "FAM-HS", Name: "Họ", HSCode: "39264000"}
	e.db.Create(parent)
	e.skuNoHS(t, "FAM-HS-L", &parent.ID)
	o := e.orderWith(t, "100602", "FAM-HS-L")
	res := e.send(t, o)
	if len(res.Shipped) != 1 {
		t.Fatalf("want shipped with the parent's HS code, got %+v", res)
	}
	if hs := e.sentHS("SO-100602"); hs != "39264000" {
		t.Fatalf("THE must get the parent's HS code, got %q", hs)
	}
}

// Neither the SKU nor its parent has an HS code: refused with a message naming
// every such SKU, THE is never called and no shipment row is claimed — on every
// path (bulk send, scan, preflight).
func TestHSCode_MissingBlocksEveryPathBeforeTHE(t *testing.T) {
	e := hsEnv(t)
	parent := &models.SKU{Code: "FAM-NOHS", Name: "Họ không mã"}
	e.db.Create(parent)
	e.skuNoHS(t, "FAM-NOHS-L", &parent.ID)
	e.skuNoHS(t, "LOOSE", nil)
	// SKU-1 has its code; the other two lines (one twice) have none.
	o := e.orderWith(t, "100603", "SKU-1", "FAM-NOHS-L", "LOOSE", "FAM-NOHS-L")

	want := "Đơn #100603 chưa có mã HS cho SKU FAM-NOHS-L, LOOSE — khai mã HS ở Master Data → SKU"

	// Bulk / single send by id.
	res := e.send(t, o)
	if len(res.Shipped) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("want skipped, got %+v", res)
	}
	sk := res.Skipped[0]
	if sk.Code != ShipErrData || !strings.Contains(sk.Reason, want) {
		t.Fatalf("reason must name the SKUs without HS code: %+v", sk)
	}
	if strings.Contains(sk.Reason, "SKU-1") {
		t.Fatalf("a SKU with its HS code must not be named: %q", sk.Reason)
	}

	// Scan "Gửi cho THE": 409 with the same sentence.
	_, err := e.packing.ShipScannedOrderWith(context.Background(), opsActor(), "100603", ShipSendOptions{})
	ae, ok := apperr.As(err)
	if err == nil || !ok || ae.Status != 409 || !strings.Contains(ae.Message, want) {
		t.Fatalf("scan must be refused with the HS message, got %v", err)
	}

	// Preflight (the confirm dialog) shows it blocked with the reason.
	pf, err := e.carrier.PreflightTHE(context.Background(), opsActor(), []uint{o.ID})
	if err != nil {
		t.Fatal(err)
	}
	if pf.Ready != 0 || pf.Blocked != 1 || !strings.Contains(pf.Orders[0].Reason, want) {
		t.Fatalf("preflight must hold the order back: %+v", pf)
	}

	e.fake.mu.Lock()
	creates := e.fake.creates
	e.fake.mu.Unlock()
	if creates != 0 {
		t.Fatalf("THE must not be called, creates=%d", creates)
	}
	var n int64
	e.db.Model(&models.CarrierShipment{}).Where("order_id = ?", o.ID).Count(&n)
	if n != 0 {
		t.Fatalf("no shipment may be claimed for a refused order, got %d rows", n)
	}

	// Declared on the parent: the order now goes (LOOSE still missing → still
	// held), then LOOSE declared too → it ships.
	e.db.Model(&models.SKU{}).Where("id = ?", parent.ID).Update("hs_code", "39264000")
	if res := e.send(t, o); len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "cho SKU LOOSE —") {
		t.Fatalf("only LOOSE must be left: %+v", res)
	}
	e.db.Model(&models.SKU{}).Where("code = ?", "LOOSE").Update("hs_code", "44209000")
	if res := e.send(t, o); len(res.Shipped) != 1 {
		t.Fatalf("all declared → must ship: %+v", res)
	}
}

// A bulk send mixing complete and incomplete orders: the complete ones go, the
// incomplete one is skipped with its reason, THE sees only the complete ones.
func TestHSCode_BulkMixedSendsOnlyComplete(t *testing.T) {
	e := hsEnv(t)
	e.skuNoHS(t, "NOHS", nil)
	a := e.order(t, "100611")
	b := e.orderWith(t, "100612", "NOHS")
	c := e.order(t, "100613")

	res, err := e.packing.ShipOrdersToCarrierWith(context.Background(), opsActor(), []uint{a.ID, b.ID, c.ID}, ShipSendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Shipped) != 2 || len(res.Skipped) != 1 {
		t.Fatalf("want 2 shipped + 1 skipped, got %+v", res)
	}
	sk := res.Skipped[0]
	if sk.OrderID != b.ID || sk.InternalCode != "100612" || sk.Code != ShipErrData ||
		!strings.Contains(sk.Reason, "Đơn #100612 chưa có mã HS cho SKU NOHS") {
		t.Fatalf("skipped row: %+v", sk)
	}
	if e.fake.creates != 2 || e.fake.chargedCount() != 2 {
		t.Fatalf("THE must see only the 2 complete orders: creates=%d charged=%d", e.fake.creates, e.fake.chargedCount())
	}
	if hs := e.sentHS("SO-100612"); hs != "" {
		t.Fatalf("the incomplete order must not reach THE, got HS %q", hs)
	}
}

// The ship queue flags, before anyone presses send, the orders that would be
// refused for a missing HS code — and not the orders that already have their
// label or whose SKUs all have one (own or parent's).
func TestHSCode_ShipQueueFlagsMissing(t *testing.T) {
	e := hsEnv(t)
	if err := e.db.AutoMigrate(&models.User{}, &models.QCRecord{}); err != nil {
		t.Fatal(err)
	}
	parent := &models.SKU{Code: "FAM-Q", Name: "Họ", HSCode: "39264000"}
	e.db.Create(parent)
	e.skuNoHS(t, "FAM-Q-L", &parent.ID)
	e.skuNoHS(t, "NOHS-Q", nil)

	ok := e.orderWith(t, "100621", "SKU-1", "FAM-Q-L")
	bad := e.orderWith(t, "100622", "SKU-1", "NOHS-Q", "NOHS-Q")
	unknown := e.orderWith(t, "100623", "NOT-IN-MASTER")

	qc := &QCService{repo: e.carrier.repo, audit: e.carrier.audit}
	rows, err := qc.buildResultOrders([]uint{ok.ID, bad.ID, unknown.ID})
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint][]string{}
	for _, r := range rows {
		got[r.OrderID] = r.MissingHS
	}
	if len(got[ok.ID]) != 0 {
		t.Fatalf("own / parent HS code: no flag, got %v", got[ok.ID])
	}
	if len(got[bad.ID]) != 1 || got[bad.ID][0] != "NOHS-Q" {
		t.Fatalf("want [NOHS-Q] once, got %v", got[bad.ID])
	}
	if len(got[unknown.ID]) != 0 {
		t.Fatalf("a SKU unknown to Master Data is refused for that, not flagged as missing HS: %v", got[unknown.ID])
	}
}
