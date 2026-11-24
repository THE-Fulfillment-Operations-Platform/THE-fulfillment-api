package services

import (
	"context"
	"strings"
	"testing"

	"the-fulfillment/backend/internal/models"
)

func (e *theEnv) withExternalLabel(t *testing.T, o *models.Order) {
	t.Helper()
	if err := e.db.Model(&models.Order{}).Where("id = ?", o.ID).
		Update("external_label_url", "https://files.example.com/gke/"+o.InternalCode+".pdf").Error; err != nil {
		t.Fatal(err)
	}
}

func (e *theEnv) orderSvc() *OrderService {
	return &OrderService{repo: e.carrier.repo, audit: e.carrier.audit}
}

// An order that already has a label bought outside must not get a second, paid
// THE label — neither from the bulk button nor from the scan box.
func TestTHE_ExternalLabelBlocksTHELabel(t *testing.T) {
	e := newTHEEnv(t)
	a := e.order(t, "100061")
	e.withExternalLabel(t, a)

	res := e.send(t, a)
	if len(res.Shipped) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "label mua ngoài") {
		t.Fatalf("bulk must skip with the reason: %+v", res)
	}

	pf, err := e.carrier.PreflightTHE(context.Background(), Actor{ID: 1, Role: models.RoleOwner}, []uint{a.ID})
	if err != nil || pf.Ready != 0 || pf.Blocked != 1 || !strings.Contains(pf.Orders[0].Reason, "label mua ngoài") {
		t.Fatalf("preflight must flag it too: %+v %v", pf, err)
	}

	_, err = e.packing.ShipScannedOrderWith(context.Background(), opsActor(), a.InternalCode, ShipSendOptions{})
	if err == nil || !strings.Contains(err.Error(), "label mua ngoài") {
		t.Fatalf("scan must refuse with the reason, got %v", err)
	}
	if e.fake.creates != 0 || e.fake.chargedCount() != 0 {
		t.Fatalf("THE must not be touched: creates=%d charged=%d", e.fake.creates, e.fake.chargedCount())
	}
}

// Xuất kho is not a label: it stays open for orders with an outside label.
func TestTHE_ExternalLabelStillAllowsExport(t *testing.T) {
	e := newTHEEnv(t)
	a := e.order(t, "100062")
	e.withExternalLabel(t, a)
	res := e.send(t, a, ShipSendOptions{ExportOnly: true})
	if len(res.Shipped) != 1 || e.sellerStatus(t, a.ID) != models.SellerStatusHandedOff {
		t.Fatalf("export must still work: %+v", res)
	}
}

// Clearing the link reopens the THE label and writes an audit entry.
func TestTHE_ClearExternalLabelThenTHELabel(t *testing.T) {
	e := newTHEEnv(t)
	a := e.order(t, "100063")
	e.withExternalLabel(t, a)
	svc := e.orderSvc()

	if err := svc.ClearExternalLabel(Actor{ID: 2, Role: models.RoleOps, Perms: map[string]bool{}}, a.ID); err == nil {
		t.Fatal("an actor without orders.manage must be refused")
	}
	if err := svc.ClearExternalLabel(Actor{ID: 1, Role: models.RoleOwner}, a.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	var got models.Order
	e.db.First(&got, a.ID)
	if got.ExternalLabelURL != "" {
		t.Fatalf("link not cleared: %q", got.ExternalLabelURL)
	}
	if err := svc.ClearExternalLabel(Actor{ID: 1, Role: models.RoleOwner}, a.ID); err == nil {
		t.Fatal("clearing an order without a link must say so")
	}
	svc.audit.Drain(context.Background())
	var n int64
	e.db.Model(&models.AuditLog{}).Where("action = ? AND entity_id = ?", "ORDER_EXTERNAL_LABEL_SET", a.ID).Count(&n)
	if n != 1 {
		t.Fatalf("want 1 audit entry, got %d", n)
	}
	if res := e.send(t, a); len(res.Shipped) != 1 || res.Shipped[0].THE == nil {
		t.Fatalf("THE label must work after clearing: %+v", res)
	}
}
