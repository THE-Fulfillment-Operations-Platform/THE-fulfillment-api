package services

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/secretbox"
	"the-fulfillment/backend/internal/shipping"
	"the-fulfillment/backend/internal/shipping/theapi"
)

// ---------- fake THE ----------
//
// Behaves like THE's customer API as read from its source (see the hồ sơ):
// create never charges and, for an order number that still has a PENDING
// package, answers with THAT package's stored data; delivery charges and turns
// the package pre-transit with a THE code; cancel archives a pending package
// and cancels (refunds) a delivered one.

type fakePkg struct {
	id, orderNumber, status, code string
	req                           theapi.CreatePackageRequest
	charged                       bool
}

type fakeTHE struct {
	mu       sync.Mutex
	nextID   int
	pkgs     map[string]*fakePkg
	creates  int
	delivers int
	cancels  int
	// one-shot failure injection
	createTransportAfterApply  bool  // create lands on THE, the answer is lost
	deliverTransportAfterApply bool  // delivery charges, the answer is lost
	deliverTransport           bool  // delivery never reaches THE
	deliverErr                 error // THE refuses the delivery (e.g. wallet)
	addrExists                 string
	addrZip                    string
	lastMile                   string
}

func newFakeTHE() *fakeTHE {
	return &fakeTHE{pkgs: map[string]*fakePkg{}, addrExists: "Y", lastMile: "9200190000000000000001"}
}

var pngLabel = []byte("\x89PNG\r\n\x1a\n-fake-label")

func (f *fakeTHE) chargedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.pkgs {
		if p.charged {
			n++
		}
	}
	return n
}

func (f *fakeTHE) echo(p *fakePkg) *theapi.Package {
	return &theapi.Package{
		ID: theapi.Flex(p.id), Code: theapi.Flex(p.code), OrderNumber: theapi.Flex(p.orderNumber),
		Status: theapi.Flex(p.status), TotalCost: 4.5,
		Weight: theapi.FlexFloat(p.req.Weight), Length: theapi.FlexFloat(p.req.Length),
		Width: theapi.FlexFloat(p.req.Width), Height: theapi.FlexFloat(p.req.Height),
		Value: theapi.FlexFloat(p.req.Value), Zipcode: theapi.Flex(p.req.Zipcode), Address1: theapi.Flex(p.req.Address1),
		HSCode: theapi.Flex(p.req.HSCode),
	}
}

func (f *fakeTHE) CreatePackage(_ context.Context, in theapi.CreatePackageRequest) (*theapi.Package, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	for _, p := range f.pkgs {
		if p.orderNumber == in.OrderNumber && p.status == "pending" {
			return f.echo(p), nil // THE's de-dupe: the OLD package's data
		}
	}
	f.nextID++
	p := &fakePkg{id: strconv.Itoa(1000 + f.nextID), orderNumber: in.OrderNumber, status: "pending", req: in}
	f.pkgs[p.id] = p
	if f.createTransportAfterApply {
		f.createTransportAfterApply = false
		return nil, fmt.Errorf("%w: timeout", theapi.ErrTransport)
	}
	return f.echo(p), nil
}

func (f *fakeTHE) DeliverPackage(_ context.Context, id string) (*theapi.DeliverResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivers++
	if f.deliverTransport {
		f.deliverTransport = false
		return nil, fmt.Errorf("%w: connection reset", theapi.ErrTransport)
	}
	if f.deliverErr != nil {
		err := f.deliverErr
		f.deliverErr = nil
		return nil, err
	}
	p := f.pkgs[id]
	if p == nil {
		return nil, &theapi.Error{Status: 404, Message: "Not found"}
	}
	if p.status != "pending" {
		return nil, &theapi.Error{Status: 400, Messages: []string{"Can't update order when status different pending"}}
	}
	p.status, p.charged, p.code = "pre-transit", true, "THE00001"+id
	if f.deliverTransportAfterApply {
		f.deliverTransportAfterApply = false
		return nil, fmt.Errorf("%w: timeout", theapi.ErrTransport)
	}
	return &theapi.DeliverResult{Success: true, PackageCode: theapi.Flex(p.code), BillCode: "2100120261005",
		Base64Label: base64.StdEncoding.EncodeToString(pngLabel)}, nil
}

func (f *fakeTHE) GetPackage(_ context.Context, id string) (*theapi.PackageDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pkgs[id]
	if p == nil {
		return nil, &theapi.Error{Status: 404, Message: "Not found"}
	}
	d := &theapi.PackageDetail{Package: *f.echo(p)}
	if p.charged && f.lastMile != "" {
		d.Tracking = &struct {
			TrackingNumber  theapi.Flex `json:"tracking_number"`
			LastMileCarrier theapi.Flex `json:"last_mile_carrier"`
			LabelURL        theapi.Flex `json:"label_url"`
		}{TrackingNumber: theapi.Flex(f.lastMile), LastMileCarrier: "USPS"}
	}
	return d, nil
}

func (f *fakeTHE) CancelPackages(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	for _, id := range ids {
		p := f.pkgs[id]
		if p == nil {
			continue
		}
		switch p.status {
		case "pending":
			p.status = "archived"
		case "pre-transit":
			p.status, p.charged = "canceled", false // refunded
		default:
			return &theapi.Error{Status: 400, Messages: []string{"Package status is invalid"}}
		}
	}
	return nil
}

func (f *fakeTHE) ValidateAddress(_ context.Context, in theapi.AddressCheckRequest) (*theapi.AddressCheck, error) {
	zip := f.addrZip
	if zip == "" {
		zip = in.Zipcode
	}
	return &theapi.AddressCheck{Line1: theapi.Flex(strings.ToUpper(in.Address1)), City: theapi.Flex(in.City),
		State: theapi.Flex(in.StateCode), Zip5: theapi.Flex(zip), AddressExists: theapi.Flex(f.addrExists)}, nil
}

func (f *fakeTHE) FetchLabel(context.Context, string) ([]byte, string, error) {
	return pngLabel, "image/png", nil
}

func (f *fakeTHE) ListServices(context.Context) ([]theapi.Service, error) {
	return []theapi.Service{{ID: "3", Code: "EXPRESS", Name: "Express"}}, nil
}

func (f *fakeTHE) GetBalance(context.Context) (*theapi.Balance, error) {
	return &theapi.Balance{Balance: 120}, nil
}

// ---------- environment ----------

type theEnv struct {
	db      *gorm.DB
	fake    *fakeTHE
	carrier *CarrierService
	packing *PackingService
	sku     *models.SKU
}

func newTHEEnv(t *testing.T) *theEnv {
	t.Helper()
	db := newHandoffDB(t)
	if err := db.AutoMigrate(&models.CarrierConfig{}, &models.CarrierShipment{}); err != nil {
		t.Fatal(err)
	}
	// The production guard (migrate.go): one live shipment per order.
	if err := db.Exec(`CREATE UNIQUE INDEX uniq_carrier_shipments_live_order ON carrier_shipments (order_id) WHERE status NOT IN ('CANCELLED', 'FAILED') AND deleted_at IS NULL`).Error; err != nil {
		t.Fatal(err)
	}
	repo := repositories.New(db)
	audit := &AuditService{repo: repo}
	box, _ := secretbox.New(strings.Repeat("k", 40), "carrier-token")
	fake := newFakeTHE()
	carrier := &CarrierService{repo: repo, audit: audit,
		tracking: NewTrackingSyncService(repo, audit, nil, "", false),
		opts:     CarrierOptions{Box: box, NewClient: func(string, string) theAPI { return fake }}}
	packing := &PackingService{repo: repo, audit: audit, carrier: shipping.NewNoopCarrier("THE"),
		tracking: carrier.tracking, the: carrier}

	on, tok, svc := true, "dGVzdEB0ZXN0LmNvbTp1dWlkLTEyMzQ1Njc4OQ==", "EXPRESS"
	if _, err := carrier.UpdateConfig(Actor{ID: 1, Role: models.RoleOwner},
		CarrierConfigInput{Token: &tok, ServiceCode: &svc, Enabled: &on}); err != nil {
		t.Fatalf("config: %v", err)
	}
	sku := &models.SKU{Code: "SKU-1", Name: "Bảng gỗ", ShipWeightG: fptr(150), ShipLengthCM: fptr(20),
		ShipWidthCM: fptr(15), ShipHeightCM: fptr(2), DeclaredValue: fptr(6), HSCode: "44209000"}
	if err := db.Create(sku).Error; err != nil {
		t.Fatal(err)
	}
	return &theEnv{db: db, fake: fake, carrier: carrier, packing: packing, sku: sku}
}

// order: approved, every item QC-passed, US address.
func (e *theEnv) order(t *testing.T, code string, qty ...int) *models.Order {
	t.Helper()
	if len(qty) == 0 {
		qty = []int{1}
	}
	statuses := make([]models.InternalStatus, len(qty))
	for i := range qty {
		statuses[i] = models.StatusQCPassed
	}
	o := seedOrderForShipping(t, e.db, code, statuses...)
	e.db.Model(&models.Order{}).Where("id = ?", o.ID).Updates(map[string]any{
		"shipping_name": "Jane Doe", "shipping_address1": "1425 Harrison Street", "shipping_city": "Oakland",
		"shipping_province": "CA", "shipping_zip": "94612", "shipping_country": "United States",
	})
	for i, q := range qty {
		e.db.Model(&models.OrderItem{}).Where("order_id = ? AND line_no = ?", o.ID, i+1).Update("quantity", q)
	}
	return o
}

func (e *theEnv) send(t *testing.T, o *models.Order, opts ...ShipSendOptions) *ShipToCarrierResult {
	t.Helper()
	var opt ShipSendOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	res, err := e.packing.ShipOrdersToCarrierWith(context.Background(), opsActor(), []uint{o.ID}, opt)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return res
}

func (e *theEnv) shipment(t *testing.T, orderID uint) *models.CarrierShipment {
	t.Helper()
	sh, err := e.carrier.repo.Carrier.LiveShipment(orderID)
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

// age pretends the shipment row was last touched `d` ago.
func (e *theEnv) age(t *testing.T, orderID uint, d time.Duration) {
	t.Helper()
	if err := e.db.Model(&models.CarrierShipment{}).Where("order_id = ?", orderID).
		UpdateColumn("updated_at", time.Now().Add(-d)).Error; err != nil {
		t.Fatal(err)
	}
}

func (e *theEnv) sellerStatus(t *testing.T, id uint) models.SellerStatus {
	var o models.Order
	e.db.First(&o, id)
	return o.SellerStatus
}

// ---------- tests ----------

// The headline: one click creates the THE shipment, pays it, keeps the label,
// writes the handoff and puts the USPS number on the order.
func TestTHE_SendCreatesPaysAndHandsOff(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100001")

	res := e.send(t, o)
	if len(res.Shipped) != 1 || len(res.Skipped) != 0 {
		t.Fatalf("want shipped, got %+v", res)
	}
	got := res.Shipped[0].THE
	if got == nil || !strings.HasPrefix(got.TrackingCode, "THE") || !got.HasLabel {
		t.Fatalf("THE outcome missing: %+v", got)
	}
	if e.fake.creates != 1 || e.fake.delivers != 1 || e.fake.chargedCount() != 1 {
		t.Fatalf("creates=%d delivers=%d charged=%d", e.fake.creates, e.fake.delivers, e.fake.chargedCount())
	}
	sh := e.shipment(t, o.ID)
	if sh.Status != models.ShipmentLabeled || string(sh.Label) != string(pngLabel) || sh.LabelType != "image/png" {
		t.Fatalf("shipment not labeled with its label: %+v", sh.Status)
	}
	if e.sellerStatus(t, o.ID) != models.SellerStatusHandedOff {
		t.Fatal("order must be handed off after the THE shipment")
	}
	var fresh models.Order
	e.db.First(&fresh, o.ID)
	if fresh.TrackingNumber != e.fake.lastMile {
		t.Fatalf("the last-mile number must land on the order, got %q", fresh.TrackingNumber)
	}
	// The request THE got: internal code as order number, the SKU's data.
	for _, p := range e.fake.pkgs {
		r := p.req
		if r.OrderNumber != "100001" || r.Weight != 150 || r.HSCode != "44209000" || r.StateCode != "CA" ||
			r.CountryCode != "US" || r.ServiceCode != "EXPRESS" || !r.IgnoreAddressCheck {
			t.Fatalf("request sent to THE: %+v", r)
		}
	}
}

// Calling again for an order that already has a paid shipment never reaches
// THE: same outcome back, still one delivery.
func TestTHE_SecondSendReusesPaidShipment(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100002")
	e.send(t, o)
	c, cli, err := e.carrier.activeTHE()
	if err != nil {
		t.Fatal(err)
	}
	full, _ := e.carrier.repo.Order.FindByID(o.ID)
	out, err := e.carrier.ShipOrderOnTHE(context.Background(), opsActor(), c, cli, full, ShipOptions{})
	if err != nil || !out.Reused {
		t.Fatalf("want reused outcome, got %+v %v", out, err)
	}
	if e.fake.delivers != 1 || e.fake.creates != 1 {
		t.Fatalf("a second send must not call THE: creates=%d delivers=%d", e.fake.creates, e.fake.delivers)
	}
}

// The money case: THE charged but the answer was lost. An immediate resend
// must WAIT (not deliver again); after the wait, FFM asks THE, sees the code
// and adopts the paid shipment — still exactly one charge.
func TestTHE_LostDeliveryAnswerNeverPaysTwice(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100003")
	e.fake.deliverTransportAfterApply = true

	res := e.send(t, o)
	if len(res.Skipped) != 1 || res.Skipped[0].Code != ShipErrUnclear {
		t.Fatalf("want UNCLEAR skip, got %+v", res)
	}
	if e.sellerStatus(t, o.ID) == models.SellerStatusHandedOff {
		t.Fatal("unclear delivery must not hand the order off")
	}

	res = e.send(t, o)
	if len(res.Skipped) != 1 || res.Skipped[0].Code != ShipErrWait || e.fake.delivers != 1 {
		t.Fatalf("immediate resend must wait without delivering: %+v delivers=%d", res, e.fake.delivers)
	}

	e.age(t, o.ID, unclearDeliveryWait+time.Minute)
	res = e.send(t, o)
	if len(res.Shipped) != 1 {
		t.Fatalf("after the wait the paid package must be adopted: %+v", res)
	}
	if e.fake.delivers != 1 || e.fake.chargedCount() != 1 {
		t.Fatalf("paid twice: delivers=%d charged=%d", e.fake.delivers, e.fake.chargedCount())
	}
	if sh := e.shipment(t, o.ID); sh.Status != models.ShipmentLabeled || len(sh.Label) == 0 {
		t.Fatalf("adopted shipment must be labeled with its label: %+v", sh.Status)
	}
}

// The delivery never reached THE: after the wait FFM sees "pending" and
// delivers once more — one charge in the end.
func TestTHE_DeliveryThatNeverLandedIsRetried(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100004")
	e.fake.deliverTransport = true
	e.send(t, o)
	e.age(t, o.ID, unclearDeliveryWait+time.Minute)
	res := e.send(t, o)
	if len(res.Shipped) != 1 || e.fake.chargedCount() != 1 || e.fake.delivers != 2 {
		t.Fatalf("want one retry and one charge: %+v delivers=%d charged=%d", res, e.fake.delivers, e.fake.chargedCount())
	}
}

// Create landed but its answer was lost: the row stays CREATING; a resend
// after the grace period re-creates, THE answers with the same pending
// package, and only one package ever exists.
func TestTHE_LostCreateAnswerDoesNotDuplicate(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100005")
	e.fake.createTransportAfterApply = true
	res := e.send(t, o)
	if len(res.Skipped) != 1 || res.Skipped[0].Code != ShipErrUnclear {
		t.Fatalf("want UNCLEAR, got %+v", res)
	}
	if res := e.send(t, o); len(res.Skipped) != 1 || res.Skipped[0].Code != ShipErrWait {
		t.Fatalf("a fresh CREATING row must wait: %+v", res)
	}
	e.age(t, o.ID, creatingStaleAfter+time.Minute)
	res = e.send(t, o)
	if len(res.Shipped) != 1 || len(e.fake.pkgs) != 1 || e.fake.chargedCount() != 1 {
		t.Fatalf("want one package, one charge: %+v pkgs=%d", res, len(e.fake.pkgs))
	}
}

// THE refused the paid step (wallet empty); meanwhile the SKU weight was
// corrected. The resend must NOT pay for the pending package holding the old
// weight: it is archived and a new one created.
func TestTHE_ChangedDataReplacesUnpaidPackage(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100006")
	e.fake.deliverErr = &theapi.Error{Status: 400, Messages: []string{"The balance in the wallet is not enough. Please top up"}}
	res := e.send(t, o)
	if len(res.Skipped) != 1 || res.Skipped[0].Code != ShipErrTHE || !strings.Contains(res.Skipped[0].Reason, "balance") {
		t.Fatalf("wallet refusal must be shown: %+v", res)
	}
	e.db.Model(&models.SKU{}).Where("id = ?", e.sku.ID).Update("ship_weight_g", 210)

	res = e.send(t, o)
	if len(res.Shipped) != 1 {
		t.Fatalf("resend after top-up: %+v", res)
	}
	var paid, archived int
	for _, p := range e.fake.pkgs {
		switch {
		case p.charged:
			paid++
			if p.req.Weight != 210 {
				t.Fatalf("paid package carries the old weight %v", p.req.Weight)
			}
		case p.status == "archived":
			archived++
		}
	}
	if paid != 1 || archived != 1 {
		t.Fatalf("want 1 paid (new data) + 1 archived (old data), got paid=%d archived=%d", paid, archived)
	}
}

// THE's de-dupe hands back a leftover pending package with other data (e.g. a
// hidden one from THE's HS-code bug): it is archived, a fresh one is made.
func TestTHE_StaleDedupeAnswerIsReplaced(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100007")
	e.fake.pkgs["999"] = &fakePkg{id: "999", orderNumber: "100007", status: "pending",
		req: theapi.CreatePackageRequest{OrderNumber: "100007", Weight: 5, Length: 1, Width: 1, Height: 1, Value: 1, Zipcode: "94612", Address1: "1425 Harrison Street", HSCode: "44209000"}}
	res := e.send(t, o)
	if len(res.Shipped) != 1 {
		t.Fatalf("want shipped, got %+v", res)
	}
	if e.fake.pkgs["999"].status != "archived" || e.fake.pkgs["999"].charged {
		t.Fatal("the stale pending package must be archived, never paid")
	}
	if e.fake.chargedCount() != 1 {
		t.Fatalf("charged=%d", e.fake.chargedCount())
	}
}

// Missing SKU shipping data stops the order before THE is called at all.
func TestTHE_MissingSKUDataBlocksBeforeTHE(t *testing.T) {
	e := newTHEEnv(t)
	e.db.Model(&models.SKU{}).Where("id = ?", e.sku.ID).Updates(map[string]any{"ship_weight_g": nil, "hs_code": ""})
	o := e.order(t, "100008")
	res := e.send(t, o)
	if len(res.Skipped) != 1 || res.Skipped[0].Code != ShipErrData ||
		!strings.Contains(res.Skipped[0].Reason, "cân nặng") || !strings.Contains(res.Skipped[0].Reason, "mã HS") {
		t.Fatalf("want DATA naming what is missing, got %+v", res)
	}
	if e.fake.creates != 0 {
		t.Fatal("THE must not be called for an order FFM knows is incomplete")
	}
}

// USPS does not know the address → refused with the ADDRESS code (the screen
// offers "send without the check"); sending that way goes through.
func TestTHE_AddressCheckAndOverride(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100009")
	e.fake.addrExists = "N"
	res := e.send(t, o)
	if len(res.Skipped) != 1 || res.Skipped[0].Code != ShipErrAddress || e.fake.creates != 0 {
		t.Fatalf("want ADDRESS before any create, got %+v creates=%d", res, e.fake.creates)
	}
	res = e.send(t, o, ShipSendOptions{ShipOptions: ShipOptions{SkipAddressCheck: true}})
	if len(res.Shipped) != 1 {
		t.Fatalf("override must send: %+v", res)
	}

	// A zip that differs from USPS names the suggestion.
	o2 := e.order(t, "100010")
	e.fake.addrExists, e.fake.addrZip = "Y", "94607"
	res = e.send(t, o2)
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "94607") {
		t.Fatalf("zip mismatch must show USPS's zip: %+v", res)
	}
}

// Integration off: the old behaviour, untouched — handoff only, THE silent.
func TestTHE_OffMeansPlainHandoff(t *testing.T) {
	e := newTHEEnv(t)
	off := false
	if _, err := e.carrier.UpdateConfig(Actor{ID: 1, Role: models.RoleOwner}, CarrierConfigInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	o := e.order(t, "100011")
	res := e.send(t, o)
	if len(res.Shipped) != 1 || res.Shipped[0].THE != nil || e.fake.creates != 0 {
		t.Fatalf("off must be a plain handoff: %+v creates=%d", res, e.fake.creates)
	}
	// Manual handoff while ON: same, by choice.
	on := true
	e.carrier.UpdateConfig(Actor{ID: 1, Role: models.RoleOwner}, CarrierConfigInput{Enabled: &on})
	o2 := e.order(t, "100012")
	res = e.send(t, o2, ShipSendOptions{ManualHandoff: true})
	if len(res.Shipped) != 1 || e.fake.creates != 0 {
		t.Fatalf("manual handoff must not create on THE: %+v", res)
	}
}

// Two stations send the same order at the same instant: one pays, the other
// is told to wait. The unique index is what decides.
func TestTHE_ConcurrentSendsPayOnce(t *testing.T) {
	e := newTHEEnv(t)
	sqlDB, _ := e.db.DB()
	sqlDB.SetMaxOpenConns(1)
	o := e.order(t, "100013")
	c, cli, _ := e.carrier.activeTHE()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			full, _ := e.carrier.repo.Order.FindByID(o.ID)
			_, errs[i] = e.carrier.ShipOrderOnTHE(context.Background(), opsActor(), c, cli, full, ShipOptions{})
		}(i)
	}
	wg.Wait()
	if e.fake.chargedCount() != 1 {
		t.Fatalf("concurrent sends paid %d times", e.fake.chargedCount())
	}
	ok := 0
	for _, err := range errs {
		var se *ShipError
		if err == nil {
			ok++
		} else if !errors.As(err, &se) || se.Code != ShipErrWait {
			t.Fatalf("the loser must be told to wait, got %v", err)
		}
	}
	if ok < 1 {
		t.Fatal("one send must succeed")
	}
}

// Cancelling a paid shipment (owner) refunds it on THE and puts the order back
// in the ship queue, dropping the number that belonged to the dead parcel.
func TestTHE_CancelPaidShipmentReturnsOrderToQueue(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100014")
	e.send(t, o)
	if _, err := e.carrier.CancelShipment(context.Background(), Actor{ID: 1, Role: models.RoleOps}, o.ID, "x"); err == nil {
		t.Fatal("OPS must not cancel a paid THE shipment")
	}
	if _, err := e.carrier.CancelShipment(context.Background(), Actor{ID: 1, Role: models.RoleOwner}, o.ID, "nhầm đơn"); err != nil {
		t.Fatal(err)
	}
	if e.fake.chargedCount() != 0 {
		t.Fatal("THE must have refunded the cancelled package")
	}
	var fresh models.Order
	e.db.First(&fresh, o.ID)
	if fresh.SellerStatus != models.SellerStatusProduction || fresh.TrackingNumber != "" || fresh.HandedOverAt != nil {
		t.Fatalf("order must be back in the queue: status=%s tracking=%q", fresh.SellerStatus, fresh.TrackingNumber)
	}
	// And it can be sent again — a new, separate THE shipment.
	res := e.send(t, o)
	if len(res.Shipped) != 1 || e.fake.chargedCount() != 1 {
		t.Fatalf("resend after cancel: %+v", res)
	}
}

// CS attached a number by hand before the label existed: THE's number never
// overwrites a human's entry.
func TestTHE_LastMileDoesNotOverwriteCS(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100015")
	e.db.Model(&models.Order{}).Where("id = ?", o.ID).Update("tracking_number", "CS-TYPED-1")
	e.send(t, o)
	var fresh models.Order
	e.db.First(&fresh, o.ID)
	if fresh.TrackingNumber != "CS-TYPED-1" {
		t.Fatalf("CS's number overwritten with %q", fresh.TrackingNumber)
	}
}

// Parcel maths: units stack in height, weights add up with the packaging,
// the HS code of the most valuable line speaks for the parcel, a child SKU
// inherits from its parent, and Excel-mangled zips / state names are fixed.
func TestTHE_BuildParcel(t *testing.T) {
	e := newTHEEnv(t)
	parent := &models.SKU{Code: "FAM", Name: "Họ", ShipLengthCM: fptr(30), ShipWidthCM: fptr(20), ShipHeightCM: fptr(1),
		DeclaredValue: fptr(10), HSCode: "39264000"}
	e.db.Create(parent)
	child := &models.SKU{Code: "FAM-L", Name: "Con", ParentID: &parent.ID, ShipWeightG: fptr(300)}
	e.db.Create(child)
	w := 50.0
	e.carrier.UpdateConfig(Actor{ID: 1, Role: models.RoleOwner}, CarrierConfigInput{PackagingWeightG: &w})

	o := e.order(t, "100016", 2, 1) // SKU-1 ×2, then a second line
	e.db.Model(&models.OrderItem{}).Where("order_id = ? AND line_no = 2", o.ID).Update("sku_code", "FAM-L")
	e.db.Model(&models.Order{}).Where("id = ?", o.ID).Updates(map[string]any{"shipping_zip": "2134", "shipping_province": "Massachusetts"})
	full, _ := e.carrier.repo.Order.FindByID(o.ID)
	c, _, _ := e.carrier.activeTHE()
	p, problems := e.carrier.BuildParcel(c, full)
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	// 2×150 + 300 + 50 packaging
	if p.WeightG != 650 || p.LengthCM != 30 || p.WidthCM != 20 || p.HeightCM != 5 || p.Value != 22 {
		t.Fatalf("parcel: %+v", p)
	}
	if p.HSCode != "44209000" { // SKU-1 line = 2×6=12 USD > FAM-L 10 USD
		t.Fatalf("HS of the most valuable line expected, got %s", p.HSCode)
	}
	if p.Request.Zipcode != "02134" || p.Request.StateCode != "MA" {
		t.Fatalf("zip/state normalisation: %q %q", p.Request.Zipcode, p.Request.StateCode)
	}
}

// Customs data are the carrier side's: a SKU with only weight + box ships with
// the connection's default HS code / value, and a SKU's own values win.
func TestTHE_CustomsDefaultsFromConnection(t *testing.T) {
	e := newTHEEnv(t)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	plain := &models.SKU{Code: "PLAIN", Name: "Chỉ có cân + hộp", ShipWeightG: fptr(100),
		ShipLengthCM: fptr(10), ShipWidthCM: fptr(10), ShipHeightCM: fptr(1)}
	e.db.Create(plain)
	o := e.order(t, "100201")
	e.db.Model(&models.OrderItem{}).Where("order_id = ?", o.ID).Update("sku_code", "PLAIN")

	// No defaults yet: blocked, pointing at the THE settings, before THE is called.
	res := e.send(t, o)
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "Kết nối THE") || e.fake.creates != 0 {
		t.Fatalf("want a block pointing at the THE settings: %+v", res)
	}

	hs, val := "3926.40.00", 7.0
	if _, err := e.carrier.UpdateConfig(owner, CarrierConfigInput{DefaultHSCode: &hs, DefaultDeclaredValue: &val}); err != nil {
		t.Fatal(err)
	}
	res = e.send(t, o)
	if len(res.Shipped) != 1 {
		t.Fatalf("with defaults the order must ship: %+v", res)
	}
	for _, p := range e.fake.pkgs {
		if p.orderNumber == "100201" && (p.req.HSCode != "39264000" || p.req.Value != 7) {
			t.Fatalf("defaults not applied: hs=%s value=%v", p.req.HSCode, p.req.Value)
		}
	}

	// SKU-1 declares its own HS 44209000 / 6 USD: those win over the defaults.
	o2 := e.order(t, "100202")
	e.send(t, o2)
	for _, p := range e.fake.pkgs {
		if p.orderNumber == "100202" && (p.req.HSCode != "44209000" || p.req.Value != 6) {
			t.Fatalf("SKU's own customs data must win: hs=%s value=%v", p.req.HSCode, p.req.Value)
		}
	}
}

// THE refused an unknown HS code but (its handler does not stop) stored a
// hidden pending package without one. After the code is fixed, the resend gets
// THAT package back from THE's de-dupe — same weight, size, address. It must be
// archived, never paid: the HS code is part of the consistency check.
func TestTHE_HiddenPackageWithoutHSIsNeverPaid(t *testing.T) {
	e := newTHEEnv(t)
	o := e.order(t, "100301")
	parcel, _ := e.carrier.BuildParcel(func() *models.CarrierConfig { c, _, _ := e.carrier.activeTHE(); return c }(),
		func() *models.Order { full, _ := e.carrier.repo.Order.FindByID(o.ID); return full }())
	hidden := parcel.Request
	hidden.HSCode = "" // what THE stored after "Hs Code does not exist"
	e.fake.pkgs["777"] = &fakePkg{id: "777", orderNumber: "100301", status: "pending", req: hidden}

	res := e.send(t, o)
	if len(res.Shipped) != 1 {
		t.Fatalf("want shipped on a fresh package, got %+v", res)
	}
	if e.fake.pkgs["777"].charged || e.fake.pkgs["777"].status != "archived" {
		t.Fatal("the hidden package without an HS code must be archived, never paid")
	}
	for _, p := range e.fake.pkgs {
		if p.charged && p.req.HSCode != "44209000" {
			t.Fatalf("paid package must carry the HS code, got %q", p.req.HSCode)
		}
	}
}
