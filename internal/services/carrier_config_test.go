package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"the-fulfillment/backend/internal/models"
)

// The THE token is the factory's wallet: stored sealed, never echoed, only
// owner/admin touch it, and the integration cannot be switched on half-done.
func TestCarrierConfig_TokenNeverLeaves(t *testing.T) {
	e := newTHEEnv(t)
	owner := Actor{ID: 1, Role: models.RoleTechnician}

	var row models.CarrierConfig
	e.db.Where("provider = ?", models.CarrierProviderTHE).First(&row)
	if row.TokenSealed == "" || strings.Contains(row.TokenSealed, "dGVzdEB0ZXN0") {
		t.Fatal("token must be stored sealed, not as typed")
	}
	v, err := e.carrier.GetConfig(owner)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "dGVzdEB0ZXN0") || !v.HasToken || v.TokenHint != "…OQ==" || !v.Ready {
		t.Fatalf("view leaks or is wrong: %s", raw)
	}
	if _, err := e.carrier.GetConfig(Actor{ID: 2, Role: models.RoleOps}); err == nil {
		t.Fatal("OPS must not read the THE connection")
	}

	// Cannot enable without a token.
	clear, on := true, true
	if _, err := e.carrier.UpdateConfig(owner, CarrierConfigInput{ClearToken: clear, Enabled: &on}); err == nil {
		t.Fatal("enabling without a token must be refused")
	}

	// The check is read-only: balance + services, no package created.
	chk, err := e.carrier.CheckConnection(context.Background(), owner)
	if err != nil || !chk.OK || chk.Balance != 120 || len(chk.Services) != 1 || chk.Services[0].Code != "EXPRESS" {
		t.Fatalf("check: %+v %v", chk, err)
	}
	if e.fake.creates != 0 || e.fake.delivers != 0 {
		t.Fatal("the connection check must not create anything on THE")
	}
}

// Preflight tells the operator, before any money moves, which orders are ready
// and what each would declare — and reads the wallet.
func TestPreflight(t *testing.T) {
	e := newTHEEnv(t)
	ready := e.order(t, "100101")
	e.db.Create(&models.SKU{Code: "BARE", Name: "Chưa khai"})
	bare := e.order(t, "100102")
	e.db.Model(&models.OrderItem{}).Where("order_id = ?", bare.ID).Update("sku_code", "BARE")

	pf, err := e.carrier.PreflightTHE(context.Background(), opsActor(), []uint{ready.ID, bare.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !pf.Enabled || pf.Ready != 1 || pf.Blocked != 1 || pf.Balance == nil || *pf.Balance != 120 {
		t.Fatalf("preflight: %+v", pf)
	}
	for _, o := range pf.Orders {
		if o.OrderID == bare.ID && !strings.Contains(o.Reason, "BARE") {
			t.Fatalf("blocked order must name the SKU: %q", o.Reason)
		}
		if o.OrderID == ready.ID && (o.Parcel == nil || o.Parcel.WeightG != 150) {
			t.Fatalf("ready order must show its parcel: %+v", o.Parcel)
		}
	}
	if e.fake.creates != 0 {
		t.Fatal("preflight must not create anything")
	}
}

// Only the technician reads or changes the THE connection (Owner and Admin
// included); shipping still reads the same config inside the server for the
// roles that ship, whoever they are.
func TestCarrierConfig_TechnicianOnly(t *testing.T) {
	e := newTHEEnv(t)
	phone := "0900000000"
	for _, r := range []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleShipping, models.RolePacking, models.RoleCS} {
		a := Actor{ID: 9, Role: r}
		if _, err := e.carrier.GetConfig(a); err == nil {
			t.Errorf("%s must not read the connection", r)
		}
		if _, err := e.carrier.UpdateConfig(a, CarrierConfigInput{DefaultPhone: &phone}); err == nil {
			t.Errorf("%s must not save the connection", r)
		}
		if _, err := e.carrier.CheckConnection(context.Background(), a); err == nil {
			t.Errorf("%s must not check the connection", r)
		}
		if _, err := e.carrier.PreviewServiceRules(a, nil, ""); err == nil {
			t.Errorf("%s must not preview the service rules", r)
		}
	}
	tech := Actor{ID: 1, Role: models.RoleTechnician}
	if _, err := e.carrier.GetConfig(tech); err != nil {
		t.Fatalf("technician read: %v", err)
	}
	if _, err := e.carrier.UpdateConfig(tech, CarrierConfigInput{DefaultPhone: &phone}); err != nil {
		t.Fatalf("technician save: %v", err)
	}
	if _, err := e.carrier.CheckConnection(context.Background(), tech); err != nil {
		t.Fatalf("technician check: %v", err)
	}
	if _, err := e.carrier.PreviewServiceRules(tech, nil, ""); err != nil {
		t.Fatalf("technician preview: %v", err)
	}
	// The label flow of a non-technician still works off the stored config.
	o := e.order(t, "100777")
	pf, err := e.carrier.PreflightTHE(context.Background(), Actor{ID: 5, Role: models.RoleShipping}, []uint{o.ID})
	if err != nil || !pf.Enabled || pf.Ready != 1 {
		t.Fatalf("shipping role preflight must still work: %+v %v", pf, err)
	}
}
