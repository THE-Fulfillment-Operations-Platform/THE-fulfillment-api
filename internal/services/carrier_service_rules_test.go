package services

import (
	"context"
	"strings"
	"testing"

	"the-fulfillment/backend/internal/models"
)

// #7783 — "hàng thiệp thì đi line Postcard US, hàng mica và gỗ thì đi line
// Standard Plus US": FFM picks the THE service per parcel from its goods.

// goodsSKU makes a SKU fully declared for shipping, made of one material.
func (e *theEnv) goodsSKU(t *testing.T, code, matCode, matName, hs string) *models.SKU {
	t.Helper()
	var mat models.Material
	e.db.Where("code = ?", matCode).Limit(1).Find(&mat)
	if mat.ID == 0 {
		mat = models.Material{Code: matCode, Name: matName}
		if err := e.db.Create(&mat).Error; err != nil {
			t.Fatal(err)
		}
	}
	sku := &models.SKU{Code: code, Name: code, ShipWeightG: fptr(100), ShipLengthCM: fptr(20),
		ShipWidthCM: fptr(15), ShipHeightCM: fptr(1), DeclaredValue: fptr(5), HSCode: hs}
	if err := e.db.Create(sku).Error; err != nil {
		t.Fatal(err)
	}
	if err := e.db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mat.ID, QuantityPerUnit: 1}).Error; err != nil {
		t.Fatal(err)
	}
	return sku
}

// goodsOrder: one line per SKU code given.
func (e *theEnv) goodsOrder(t *testing.T, code string, skus ...string) *models.Order {
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

func (e *theEnv) catalog(t *testing.T) {
	e.goodsSKU(t, "CARD-1", "GIAY-IN-ANH", "GIẤY IN ẢNH", "4909002000347")
	e.goodsSKU(t, "MICA-1", "MICA-3-LY", "Mica 3 ly", "3926400090256")
	e.goodsSKU(t, "WOOD-1", "BASSWOOD-3-LY", "Basswood 3 ly", "9503000090301")
	e.goodsSKU(t, "BASE-1", "DE-GO", "Đế gỗ", "9503000090301")
	e.goodsSKU(t, "MUG-1", "CERAMIC", "Ceramic", "6913902000300")
}

func (e *theEnv) sentService(t *testing.T, orderNumber string) string {
	t.Helper()
	for _, p := range e.fake.pkgs {
		if p.orderNumber == orderNumber {
			return p.req.ServiceCode
		}
	}
	t.Fatalf("no package sent for %s", orderNumber)
	return ""
}

// The headline: what is created on THE carries the service of the goods, and
// the shipment row remembers it.
func TestTHEServiceRules_SendPicksServicePerGoods(t *testing.T) {
	e := newTHEEnv(t)
	e.catalog(t)
	cases := []struct {
		code  string
		skus  []string
		want  string
		goods string
	}{
		{"200001", []string{"CARD-1"}, "PCU", "Thiệp"},
		{"200002", []string{"MICA-1"}, "SPU", "Mica"},
		{"200003", []string{"WOOD-1", "BASE-1"}, "SPU", "Gỗ"},
		{"200004", []string{"CARD-1", "WOOD-1"}, "SPU", "Gỗ + Thiệp"},
		{"200005", []string{"MICA-1", "CARD-1", "WOOD-1"}, "SPU", "Mica + Gỗ + Thiệp"},
		// Not thiệp, mica nor gỗ: the connection's own service (THE keeps it
		// pending for its staff when the HS code is not on the account's list).
		{"200006", []string{"MUG-1"}, "EXPRESS", ""},
	}
	for _, c := range cases {
		o := e.goodsOrder(t, c.code, c.skus...)

		pf, err := e.carrier.PreflightTHE(context.Background(), opsActor(), []uint{o.ID})
		if err != nil || len(pf.Orders) != 1 || !pf.Orders[0].Ready || pf.Orders[0].Parcel == nil {
			t.Fatalf("%s preflight: %+v %v", c.code, pf, err)
		}
		svc := pf.Orders[0].Parcel.Service
		if svc.ServiceCode != c.want || svc.Goods != c.goods || svc.Reason == "" {
			t.Fatalf("%s preflight service: %+v, want %s / %q", c.code, svc, c.want, c.goods)
		}

		res := e.send(t, o)
		if len(res.Shipped) != 1 {
			t.Fatalf("%s not shipped: %+v", c.code, res.Skipped)
		}
		if got := e.sentService(t, "SO-"+c.code); got != c.want {
			t.Fatalf("%s sent to THE with %s, want %s", c.code, got, c.want)
		}
		if sh := e.shipment(t, o.ID); sh.ServiceCode != c.want || res.Shipped[0].THE.ServiceCode != c.want {
			t.Fatalf("%s shipment row / outcome service: %q / %q", c.code, sh.ServiceCode, res.Shipped[0].THE.ServiceCode)
		}
	}
}

// "Chặn" for goods of no kind: the order is held with a sentence naming the SKU
// and its material; known goods still go.
func TestTHEServiceRules_BlockUnclassified(t *testing.T) {
	e := newTHEEnv(t)
	e.catalog(t)
	rules := models.DefaultTHEServiceRules()
	rules.Unclassified = models.THEUnclassifiedBlock
	if _, err := e.carrier.UpdateConfig(Actor{ID: 1, Role: models.RoleOwner}, CarrierConfigInput{ServiceRules: &rules}); err != nil {
		t.Fatal(err)
	}
	mug := e.goodsOrder(t, "200011", "MUG-1", "MICA-1")
	res := e.send(t, mug)
	if len(res.Shipped) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("ceramic must be held: %+v", res)
	}
	if r := res.Skipped[0].Reason; !strings.Contains(r, "MUG-1") || !strings.Contains(r, "NVL Ceramic") || res.Skipped[0].Code != ShipErrData {
		t.Fatalf("reason: %+v", res.Skipped[0])
	}
	if e.fake.creates != 0 {
		t.Fatalf("nothing may reach THE, creates=%d", e.fake.creates)
	}
	card := e.goodsOrder(t, "200012", "CARD-1")
	if res := e.send(t, card); len(res.Shipped) != 1 || e.sentService(t, "SO-200012") != "PCU" {
		t.Fatalf("postcard must still go on PCU: %+v", res)
	}
}

// Switching the automatic pick off sends everything with the connection's own
// service again (the kill switch), and a pending package made under the other
// service is replaced, not paid for.
func TestTHEServiceRules_AutoOffAndServiceChangeReplacesPending(t *testing.T) {
	e := newTHEEnv(t)
	e.catalog(t)
	// THE holds non-postcards for its staff (the account's api-allowed-hs-codes).
	e.fake.onlyHS = "4909"
	o := e.goodsOrder(t, "200021", "MICA-1")
	res := e.send(t, o)
	if len(res.Skipped) != 1 || res.Skipped[0].Code != ShipErrHeld {
		t.Fatalf("mica must be held by THE: %+v", res)
	}
	first := e.shipment(t, o.ID)
	if first.ServiceCode != "SPU" || first.Status != models.ShipmentCreated {
		t.Fatalf("held shipment: %+v", first)
	}

	rules := models.DefaultTHEServiceRules()
	rules.Auto = false
	if _, err := e.carrier.UpdateConfig(Actor{ID: 1, Role: models.RoleOwner}, CarrierConfigInput{ServiceRules: &rules}); err != nil {
		t.Fatal(err)
	}
	e.send(t, o)
	sh := e.shipment(t, o.ID)
	if sh == nil || sh.ID == first.ID || sh.ServiceCode != "EXPRESS" {
		t.Fatalf("the SPU package must be replaced by an EXPRESS one: %+v (first %d)", sh, first.ID)
	}
	if p := e.fake.pkgs[first.ExternalID]; p == nil || p.status != "archived" || p.charged {
		t.Fatalf("the old pending package must be archived, never paid: %+v", p)
	}
	if e.fake.chargedCount() != 0 {
		t.Fatalf("nothing may be charged while THE holds non-postcards")
	}
}

func TestTHEServiceRules_ConfigViewSaveReset(t *testing.T) {
	e := newTHEEnv(t)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	v, err := e.carrier.GetConfig(owner)
	if err != nil || v.ServiceRulesSaved || len(v.ServiceRules.Groups) != 3 || !v.ServiceRules.Auto {
		t.Fatalf("defaults must show until saved: %+v %v", v, err)
	}
	bad := models.THEServiceRules{Auto: true, Groups: []models.THEGoodsGroup{{Name: "Mica"}}}
	if _, err := e.carrier.UpdateConfig(owner, CarrierConfigInput{ServiceRules: &bad}); err == nil {
		t.Fatal("a kind matching nothing must be refused")
	}
	one := models.THEServiceRules{Auto: true, Groups: []models.THEGoodsGroup{{Name: "Mica", ServiceCode: "SPU", Materials: []string{"mica"}}}}
	v, err = e.carrier.UpdateConfig(owner, CarrierConfigInput{ServiceRules: &one})
	if err != nil || !v.ServiceRulesSaved || len(v.ServiceRules.Groups) != 1 || v.ServiceRules.Groups[0].Materials[0] != "MICA" {
		t.Fatalf("saved: %+v %v", v, err)
	}
	// Saving other fields keeps the table.
	phone := "0900000000"
	if v, _ = e.carrier.UpdateConfig(owner, CarrierConfigInput{DefaultPhone: &phone}); len(v.ServiceRules.Groups) != 1 {
		t.Fatalf("table lost on an unrelated save: %+v", v.ServiceRules)
	}
	v, _ = e.carrier.UpdateConfig(owner, CarrierConfigInput{ResetServiceRules: true})
	if v.ServiceRulesSaved || len(v.ServiceRules.Groups) != 3 {
		t.Fatalf("reset must bring the defaults back: %+v", v)
	}
	if _, err := e.carrier.GetConfig(opsActor()); err == nil {
		t.Fatal("only owner/admin see the connection")
	}
}

func TestTHEServiceRules_Preview(t *testing.T) {
	e := newTHEEnv(t)
	e.catalog(t)
	// A parent with a child: the parent is not counted, the child takes the
	// parent's material when it has none.
	parent := e.goodsSKU(t, "FAM-MICA", "MICA-3-LY", "Mica 3 ly", "")
	child := &models.SKU{Code: "FAM-MICA-S", Name: "s", ParentID: &parent.ID}
	if err := e.db.Create(child).Error; err != nil {
		t.Fatal(err)
	}
	pv, err := e.carrier.PreviewServiceRules(Actor{ID: 1, Role: models.RoleOwner}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, g := range pv.Groups {
		counts[g.Name+"/"+g.ServiceCode] = g.SKUs
	}
	// SKU-1 (env's own: no material, HS 4420) is wood by HS.
	if counts["Mica/SPU"] != 2 || counts["Gỗ/SPU"] != 3 || counts["Thiệp/PCU"] != 1 ||
		pv.Unclassified.SKUs != 1 || pv.Unclassified.ServiceCode != "EXPRESS" || pv.SKUs != 7 {
		t.Fatalf("preview counts: %+v unclassified %+v total %d", counts, pv.Unclassified, pv.SKUs)
	}
	var ceramic *THEMaterialPreview
	for i := range pv.Materials {
		if pv.Materials[i].Code == "CERAMIC" {
			ceramic = &pv.Materials[i]
		}
	}
	if ceramic == nil || ceramic.Group != "" || ceramic.SKUs != 1 {
		t.Fatalf("ceramic row: %+v", ceramic)
	}

	// Previewing an edited table does not save it.
	edited := models.DefaultTHEServiceRules()
	edited.Groups = append(edited.Groups, models.THEGoodsGroup{Name: "Gốm", ServiceCode: "SPU", Materials: []string{"CERAMIC"}})
	pv, err = e.carrier.PreviewServiceRules(Actor{ID: 1, Role: models.RoleOwner}, &edited, "")
	if err != nil || pv.Unclassified.SKUs != 0 || pv.Groups[3].SKUs != 1 {
		t.Fatalf("edited preview: %+v %v", pv, err)
	}
	if v, _ := e.carrier.GetConfig(Actor{ID: 1, Role: models.RoleOwner}); v.ServiceRulesSaved {
		t.Fatal("preview must not save")
	}
}

// The ship queue shows the service before anyone presses "Tạo label THE".
func TestTHEServiceRules_QueueFlagsService(t *testing.T) {
	e := newTHEEnv(t)
	e.catalog(t)
	a := e.goodsOrder(t, "200031", "CARD-1")
	b := e.goodsOrder(t, "200032", "CARD-1", "MICA-1")
	out := []QCResultOrder{{OrderID: a.ID}, {OrderID: b.ID}, {OrderID: 999, THETracking: "THE1"}}
	lines := []models.OrderItem{
		{OrderID: a.ID, SKUCode: "CARD-1"},
		{OrderID: b.ID, SKUCode: "CARD-1"}, {OrderID: b.ID, SKUCode: "MICA-1"},
		{OrderID: 999, SKUCode: "MICA-1"},
	}
	flagTHEService(e.carrier.repo, out, lines)
	if out[0].THEService != "PCU" || out[1].THEService != "SPU" || !strings.Contains(out[1].THEServiceReason, "Đơn trộn") || out[2].THEService != "" {
		t.Fatalf("queue flags: %+v", out)
	}
}
