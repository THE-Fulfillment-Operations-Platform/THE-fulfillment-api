package services

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// newOpenDB is the import fixture (seller id=1 "S1", SKU "TESTSKU" mapped to a
// material) plus the API key table.
func newOpenDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newImportDB(t)
	if err := db.AutoMigrate(&models.APIKey{}); err != nil {
		t.Fatalf("migrate api keys: %v", err)
	}
	return db
}

func openSvc(db *gorm.DB) *OpenAPIService {
	repo := repositories.New(db)
	audit := &AuditService{repo: repo}
	return &OpenAPIService{repo: repo, audit: audit, imports: &ImportService{repo: repo, audit: audit}}
}

func keySvc(db *gorm.DB) *APIKeyService {
	repo := repositories.New(db)
	return NewAPIKeyService(repo, &AuditService{repo: repo})
}

func sellerPrincipal(id uint, code string) *models.APIPrincipal {
	return &models.APIPrincipal{KeyID: id, KeyName: "test", KeyPrefix: "ffm_test0000", SellerID: id, SellerCode: code, SellerName: "Seller " + code}
}

func qty(n int) *FlexInt { q := FlexInt(n); return &q }

// openOrder builds a minimal valid request for the fixture SKU.
func openOrder(orderID string) OpenOrderInput {
	return OpenOrderInput{
		OrderID:  orderID,
		Shipping: OpenShipping{Name: "Jane Doe", Address1: "1 Main St", Country: "US"},
		Items: []OpenItemInput{
			{SKU: "TESTSKU", Quantity: qty(2), MockupURL: "https://example.com/m.png", DesignURL: "https://example.com/d.png"},
		},
	}
}

func wantAppErr(t *testing.T, err error, status int, code string) *apperr.Error {
	t.Helper()
	ae, ok := apperr.As(err)
	if !ok {
		t.Fatalf("want apperr %d %s, got %v", status, code, err)
	}
	if ae.Status != status || ae.Code != code {
		t.Fatalf("want %d %s, got %d %s (%s)", status, code, ae.Status, ae.Code, ae.Message)
	}
	return ae
}

// ---------- API keys ----------

// The plain key exists only in the create response; the database holds a hash.
// A resolved key names its seller; a revoked one stops working at once, even
// though it was just cached.
func TestAPIKey_CreateResolveRevoke(t *testing.T) {
	db := newOpenDB(t)
	svc := keySvc(db)
	admin := Actor{ID: 9, Role: models.RoleOwner}

	created, err := svc.Create(admin, 1, "  Hệ thống ABC  ")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(created.Key, "ffm_") || len(created.Key) != 4+48 {
		t.Fatalf("unexpected key shape %q", created.Key)
	}
	if created.Name != "Hệ thống ABC" || created.Prefix != created.Key[:12] {
		t.Errorf("name/prefix not normalised: %q / %q", created.Name, created.Prefix)
	}
	var stored models.APIKey
	db.First(&stored, created.ID)
	if stored.KeyHash == "" || stored.KeyHash == created.Key || strings.Contains(stored.KeyHash, created.Key[4:]) {
		t.Fatalf("the plain key must not be stored (hash %q)", stored.KeyHash)
	}

	p, err := svc.ResolveAPIKey(created.Key)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.SellerID != 1 || p.SellerCode != "S1" || p.KeyID != created.ID {
		t.Errorf("principal = %+v", p)
	}
	db.First(&stored, created.ID)
	if stored.LastUsedAt == nil {
		t.Error("first use should stamp last_used_at")
	}

	// Wrong keys: right shape but unknown, and something that is not a key at all.
	wantAppErr(t, errOf(svc.ResolveAPIKey("ffm_"+strings.Repeat("0", 48))), http.StatusUnauthorized, "API_KEY_INVALID")
	wantAppErr(t, errOf(svc.ResolveAPIKey("eyJhbGciOiJIUzI1NiJ9.e30.sig")), http.StatusUnauthorized, "API_KEY_INVALID")

	// Another seller's id cannot revoke it.
	other := seedSeller(t, db, "S2")
	wantAppErr(t, errOf(svc.Revoke(admin, other, created.ID)), http.StatusNotFound, "NOT_FOUND")

	revoked, err := svc.Revoke(admin, 1, created.ID)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("revoke: %v (revoked_at %v)", err, revoked)
	}
	wantAppErr(t, errOf(svc.ResolveAPIKey(created.Key)), http.StatusUnauthorized, "API_KEY_INVALID")

	// Revoking again is a no-op that keeps the first timestamp.
	first := *revoked.RevokedAt
	svc.now = func() time.Time { return first.Add(time.Hour) }
	if _, err := svc.Revoke(admin, 1, created.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	db.First(&stored, created.ID)
	if stored.RevokedAt == nil || !stored.RevokedAt.Equal(first) {
		t.Errorf("revoked_at moved: %v, want %v", stored.RevokedAt, first)
	}

	keys, err := svc.List(1)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list: %v, %d keys", err, len(keys))
	}
}

func errOf[T any](_ T, err error) error { return err }

// A paused seller's key is refused with its own code, and works again on the
// next request after the seller is re-activated.
func TestAPIKey_PausedSellerIsRefused(t *testing.T) {
	db := newOpenDB(t)
	svc := keySvc(db)
	created, err := svc.Create(Actor{ID: 9}, 1, "k")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	db.Model(&models.Seller{}).Where("id = ?", 1).Update("status", "paused")
	wantAppErr(t, errOf(svc.ResolveAPIKey(created.Key)), http.StatusForbidden, "SELLER_PAUSED")

	db.Model(&models.Seller{}).Where("id = ?", 1).Update("status", "active")
	if _, err := svc.ResolveAPIKey(created.Key); err != nil {
		t.Fatalf("re-activated seller should resolve: %v", err)
	}

	// A deleted seller takes its keys with it (once the cached entry expires).
	db.Delete(&models.Seller{}, 1)
	svc.now = func() time.Time { return time.Now().Add(2 * apiKeyTTL) }
	wantAppErr(t, errOf(svc.ResolveAPIKey(created.Key)), http.StatusUnauthorized, "API_KEY_INVALID")
}

func TestAPIKey_CapsActiveKeysAndNeedsAName(t *testing.T) {
	db := newOpenDB(t)
	svc := keySvc(db)
	admin := Actor{ID: 9}

	wantAppErr(t, errOf(svc.Create(admin, 1, "   ")), http.StatusUnprocessableEntity, "UNPROCESSABLE")
	wantAppErr(t, errOf(svc.Create(admin, 999, "k")), http.StatusNotFound, "NOT_FOUND")

	var last *CreatedAPIKey
	for i := 0; i < MaxActiveAPIKeys; i++ {
		k, err := svc.Create(admin, 1, "k")
		if err != nil {
			t.Fatalf("create #%d: %v", i+1, err)
		}
		last = k
	}
	wantAppErr(t, errOf(svc.Create(admin, 1, "one too many")), http.StatusConflict, "CONFLICT")
	// Revoking one frees a slot.
	if _, err := svc.Revoke(admin, 1, last.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.Create(admin, 1, "replacement"); err != nil {
		t.Fatalf("create after revoke: %v", err)
	}
}

// ---------- Create order ----------

// The order lands exactly where an imported one would: pending review, items
// with tem codes, assets recorded, a required-attention note for the line that
// came without a mockup — and it remembers the caller's id as its api_ref.
func TestOpenCreate_CreatesAReviewableOrder(t *testing.T) {
	db := newOpenDB(t)
	svc := openSvc(db)

	in := openOrder("  ETSY-1001  ")
	in.OrderDate = "2026-09-30"
	in.Items = append(in.Items, OpenItemInput{SKU: "testsku", EngraveText: "Anna"}) // no quantity, no mockup
	res, err := svc.CreateOrder(sellerPrincipal(1, "S1"), "203.0.113.7", in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !res.Created || len(res.Warnings) != 0 {
		t.Fatalf("created=%v warnings=%v", res.Created, res.Warnings)
	}
	o := res.Order
	if o.OrderID != "ETSY-1001" || o.Status != OpenStatusPendingReview || o.StatusText == "" || o.OrderDate != "2026-09-30" {
		t.Errorf("order = %+v", o)
	}
	if o.Tracking != nil {
		t.Errorf("a new order has no tracking, got %+v", o.Tracking)
	}
	if len(o.Items) != 2 || o.Items[0].Quantity != 2 || o.Items[1].Quantity != 1 ||
		o.Items[1].SKU != "TESTSKU" || o.Items[0].ProductName != "Test SKU" || o.Items[1].Line != 2 {
		t.Errorf("items = %+v", o.Items)
	}

	var order models.Order
	if err := db.Preload("Items").Where("internal_code = ?", o.Code).First(&order).Error; err != nil {
		t.Fatalf("load order %s: %v", o.Code, err)
	}
	if order.APIRef == nil || *order.APIRef != "ETSY-1001" || order.SellerID != 1 ||
		order.ReviewStatus != models.ReviewPending || order.DailySeq != 1 || order.CreatedByID != nil {
		t.Errorf("stored order = %+v (api_ref %v)", order, order.APIRef)
	}
	if strings.HasPrefix(order.InternalCode, "TMP") {
		t.Errorf("placeholder code survived: %s", order.InternalCode)
	}
	if order.Items[0].InternalCode != order.InternalCode+"_1/2" || order.Items[0].SKUID == nil {
		t.Errorf("item code/sku = %q / %v", order.Items[0].InternalCode, order.Items[0].SKUID)
	}
	if order.Items[0].DesignStatus != models.DesignPending || order.Items[1].DesignStatus != models.DesignMissing {
		t.Errorf("design statuses = %s, %s", order.Items[0].DesignStatus, order.Items[1].DesignStatus)
	}
	var assets, notes int64
	db.Model(&models.ItemAsset{}).Count(&assets)
	db.Model(&models.Note{}).Where("reason_code = ?", "ART_MISSING").Count(&notes)
	if assets != 2 || notes != 1 {
		t.Errorf("assets=%d notes=%d, want 2 and 1", assets, notes)
	}
	var audit models.AuditLog
	if err := db.Where("action = ?", "ORDER_CREATE_API").First(&audit).Error; err != nil {
		t.Fatalf("audit entry: %v", err)
	}
	if audit.ActorID != nil || audit.ActorEmail != "api-key:ffm_test0000" || audit.IP != "203.0.113.7" {
		t.Errorf("audit = %+v", audit)
	}
}

// The whole point of the channel: a retry is the same order, whatever the retry
// carries — and it stays answerable even if the SKU has since disappeared.
func TestOpenCreate_ResendingAnOrderIDReturnsTheSameOrder(t *testing.T) {
	db := newOpenDB(t)
	svc := openSvc(db)
	p := sellerPrincipal(1, "S1")

	first, err := svc.CreateOrder(p, "", openOrder("ETSY-1001"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	replay := openOrder("ETSY-1001")
	replay.Items[0].SKU = "NO-SUCH-SKU"
	replay.Shipping.Name = "Someone Else"
	second, err := svc.CreateOrder(p, "", replay)
	if err != nil {
		t.Fatalf("replay must not fail: %v", err)
	}
	if second.Created || second.Order.Code != first.Order.Code || second.Order.Shipping.Name != "Jane Doe" {
		t.Errorf("replay = created %v, code %s, name %q", second.Created, second.Order.Code, second.Order.Shipping.Name)
	}
	var n int64
	db.Model(&models.Order{}).Count(&n)
	if n != 1 {
		t.Fatalf("orders = %d, want 1", n)
	}

	// The same id from ANOTHER seller is that seller's own order.
	s2 := seedSeller(t, db, "S2")
	other, err := svc.CreateOrder(sellerPrincipal(s2, "S2"), "", openOrder("ETSY-1001"))
	if err != nil || !other.Created || other.Order.Code == first.Order.Code {
		t.Fatalf("other seller: %v, %+v", err, other)
	}

	// A deleted order frees its id: the factory removed it, a resend is a new order.
	db.Where("internal_code = ?", first.Order.Code).Delete(&models.Order{})
	again, err := svc.CreateOrder(p, "", openOrder("ETSY-1001"))
	if err != nil || !again.Created || again.Order.Code == first.Order.Code {
		t.Fatalf("after delete: %v, %+v", err, again)
	}
}

// An order that came in by file is not what an API retry means: the file import
// never set api_ref, so the API creates its own order (the list flags the
// repeated store order id, as it does for a repeated import).
func TestOpenCreate_DoesNotAdoptImportedOrders(t *testing.T) {
	db := newOpenDB(t)
	svc := openSvc(db)
	imported := &models.Order{InternalCode: "100900", StoreOrderID: "ETSY-1001", SellerID: 1,
		SellerStatus: models.SellerStatusProduction, ReviewStatus: models.ReviewApproved}
	if err := db.Create(imported).Error; err != nil {
		t.Fatalf("seed imported: %v", err)
	}
	res, err := svc.CreateOrder(sellerPrincipal(1, "S1"), "", openOrder("ETSY-1001"))
	if err != nil || !res.Created || res.Order.Code == "100900" {
		t.Fatalf("create: %v, %+v", err, res)
	}
}

// Everything wrong with a request comes back in one answer, each problem pinned
// to the field the caller sent — and nothing is created.
func TestOpenCreate_ReportsEveryProblemAndCreatesNothing(t *testing.T) {
	db := newOpenDB(t)
	svc := openSvc(db)
	unmapped := &models.SKU{Code: "NOMAT", Name: "No material"}
	if err := db.Create(unmapped).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}

	in := OpenOrderInput{
		OrderDate: "30/09/2026",
		Shipping:  OpenShipping{Name: "Jane", Zip: strings.Repeat("9", 41)},
		Items: []OpenItemInput{
			{SKU: "NO-SUCH-SKU"},
			{SKU: "TESTSKU", Quantity: qty(0)},
			{SKU: "NOMAT"},
			{SKU: ""},
			{SKU: "TESTSKU", MockupURL: "ftp://x/y.png"},
			{SKU: "TESTSKU", DesignURL: "https://e.com/a.png", BackDesignURL: "https://e.com/a.png"},
			{SKU: "TESTSKU", DesignURL: "design-a"}, // a bare design code is fine
		},
	}
	_, err := svc.CreateOrder(sellerPrincipal(1, "S1"), "", in)
	ae := wantAppErr(t, err, http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	details, ok := ae.Details.([]OpenFieldError)
	if !ok {
		t.Fatalf("details type %T", ae.Details)
	}
	got := map[string]string{}
	for _, d := range details {
		got[d.Field] = d.Code
	}
	want := map[string]string{
		"order_id":                 "REQUIRED",
		"order_date":               "DATE_INVALID",
		"shipping.address1":        "REQUIRED",
		"shipping.country":         "REQUIRED",
		"shipping.zip":             "TOO_LONG",
		"items[0].sku":             "SKU_NOT_FOUND",
		"items[1].quantity":        "QUANTITY_INVALID",
		"items[2].sku":             "SKU_NOT_READY",
		"items[3].sku":             "REQUIRED",
		"items[4].mockup_url":      "URL_INVALID",
		"items[5].back_design_url": "DESIGN_SIDE_DUPLICATE",
	}
	for field, code := range want {
		if got[field] != code {
			t.Errorf("%s: got %q, want %q", field, got[field], code)
		}
	}
	if len(details) != len(want) {
		t.Errorf("got %d errors, want %d: %+v", len(details), len(want), details)
	}
	var n int64
	db.Model(&models.Order{}).Count(&n)
	if n != 0 {
		t.Fatalf("a rejected request created %d orders", n)
	}

	// No items, and too many items.
	empty := openOrder("E-1")
	empty.Items = nil
	wantAppErr(t, errOf(svc.CreateOrder(sellerPrincipal(1, "S1"), "", empty)), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
	big := openOrder("E-2")
	for len(big.Items) <= MaxOpenOrderItems {
		big.Items = append(big.Items, big.Items[0])
	}
	wantAppErr(t, errOf(svc.CreateOrder(sellerPrincipal(1, "S1"), "", big)), http.StatusUnprocessableEntity, "VALIDATION_ERROR")
}

func TestOpenCreate_WarnsWhenZipAndStateLookSwapped(t *testing.T) {
	db := newOpenDB(t)
	in := openOrder("E-1")
	in.Shipping.Zip, in.Shipping.Province = "MN", "55112"
	res, err := openSvc(db).CreateOrder(sellerPrincipal(1, "S1"), "", in)
	if err != nil || !res.Created {
		t.Fatalf("a warning must not block: %v", err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != "ZIP_STATE_SWAPPED" {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

// ---------- Read ----------

// A seller reads its own orders by its order id or our code — and gets "not
// found", never "forbidden", for anything belonging to someone else.
func TestOpenGet_IsScopedToTheKeysSeller(t *testing.T) {
	db := newOpenDB(t)
	svc := openSvc(db)
	s2 := seedSeller(t, db, "S2")
	mine, err := svc.CreateOrder(sellerPrincipal(1, "S1"), "", openOrder("ETSY-1001"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, ref := range []string{"ETSY-1001", mine.Order.Code, " ETSY-1001 "} {
		got, err := svc.GetOrder(1, ref)
		if err != nil || got.Code != mine.Order.Code || got.Items[0].ProductName != "Test SKU" {
			t.Errorf("get %q: %v, %+v", ref, err, got)
		}
	}
	for _, ref := range []string{"ETSY-1001", mine.Order.Code} {
		wantAppErr(t, errOf(svc.GetOrder(s2, ref)), http.StatusNotFound, "ORDER_NOT_FOUND")
	}
	wantAppErr(t, errOf(svc.GetOrder(1, "NOPE")), http.StatusNotFound, "ORDER_NOT_FOUND")
	wantAppErr(t, errOf(svc.GetOrder(1, "  ")), http.StatusNotFound, "ORDER_NOT_FOUND")

	// An order that came in by file is readable by its store order id too.
	db.Create(&models.Order{InternalCode: "100900", StoreOrderID: "FILE-7", SellerID: 1,
		SellerStatus: models.SellerStatusProduction, ReviewStatus: models.ReviewApproved})
	if got, err := svc.GetOrder(1, "FILE-7"); err != nil || got.Code != "100900" || got.Status != OpenStatusInProduction {
		t.Errorf("file order: %v, %+v", err, got)
	}
}

func TestOpenList_FiltersAndPages(t *testing.T) {
	db := newOpenDB(t)
	svc := openSvc(db)
	p := sellerPrincipal(1, "S1")
	s2 := seedSeller(t, db, "S2")
	for _, id := range []string{"A-1", "A-2", "A-3"} {
		if _, err := svc.CreateOrder(p, "", openOrder(id)); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if _, err := svc.CreateOrder(sellerPrincipal(s2, "S2"), "", openOrder("B-1")); err != nil {
		t.Fatalf("create B-1: %v", err)
	}
	// A-2 approved and shipped with a tracking number; A-3 cancelled.
	now := time.Now()
	db.Model(&models.Order{}).Where("store_order_id = ?", "A-2").Updates(map[string]interface{}{
		"review_status": models.ReviewApproved, "seller_status": models.SellerStatusShipped,
		"tracking_number": "TRK1", "tracking_status": models.TrackingInTransit, "tracking_updated_at": now,
	})
	db.Model(&models.Order{}).Where("store_order_id = ?", "A-3").Updates(map[string]interface{}{
		"review_status": models.ReviewCancelled, "cancellation_status": models.CancellationApproved,
		"cancellation_reason": "khách đổi ý",
	})

	rows, page, total, err := svc.ListOrders(1, OpenOrderQuery{})
	if err != nil || total != 3 || len(rows) != 3 || rows[0].OrderID != "A-3" || page.PageSize != 20 {
		t.Fatalf("all: %v total=%d rows=%d page=%+v", err, total, len(rows), page)
	}
	if rows[0].Status != OpenStatusCancelled || rows[0].CancellationReason != "khách đổi ý" {
		t.Errorf("cancelled row = %+v", rows[0])
	}

	rows, _, total, err = svc.ListOrders(1, OpenOrderQuery{Status: "shipped"})
	if err != nil || total != 1 || rows[0].OrderID != "A-2" {
		t.Fatalf("shipped: %v total=%d", err, total)
	}
	if tr := rows[0].Tracking; tr == nil || tr.Number != "TRK1" || tr.Status != "IN_TRANSIT" {
		t.Errorf("tracking = %+v", rows[0].Tracking)
	}
	// Still pending review, not "in production": the production phase only speaks
	// once the order is approved.
	if _, _, total, _ = svc.ListOrders(1, OpenOrderQuery{Status: OpenStatusInProduction}); total != 0 {
		t.Errorf("in production = %d, want 0", total)
	}
	if _, _, total, _ = svc.ListOrders(1, OpenOrderQuery{Status: OpenStatusPendingReview}); total != 1 {
		t.Errorf("pending review = %d, want 1", total)
	}
	_, _, _, err = svc.ListOrders(1, OpenOrderQuery{Status: "WHATEVER"})
	wantAppErr(t, err, http.StatusBadRequest, "STATUS_INVALID")

	// order_id narrows to exactly that order; another seller's id is an empty list.
	rows, _, total, err = svc.ListOrders(1, OpenOrderQuery{OrderID: "A-1"})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].OrderID != "A-1" {
		t.Fatalf("order_id: %v total=%d", err, total)
	}
	rows, _, total, err = svc.ListOrders(1, OpenOrderQuery{OrderID: "B-1"})
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("foreign order_id: %v total=%d", err, total)
	}

	// Page size: capped at the maximum, "everything" (-1) is not on offer.
	for _, size := range []int{-1, 0, 5000} {
		_, page, _, _ := svc.ListOrders(1, OpenOrderQuery{Page: repositories.Page{PageSize: size}})
		if page.PageSize < 1 || page.PageSize > MaxOpenPageSize {
			t.Errorf("page_size %d → %d", size, page.PageSize)
		}
	}
	rows, _, total, _ = svc.ListOrders(1, OpenOrderQuery{Page: repositories.Page{Page: 2, PageSize: 2}})
	if total != 3 || len(rows) != 1 || rows[0].OrderID != "A-1" {
		t.Errorf("page 2: total=%d rows=%d", total, len(rows))
	}
}

// One status out, whatever combination of review and production state is in.
func TestOpenStatus(t *testing.T) {
	cases := []struct {
		review models.ReviewStatus
		cancel models.CancellationStatus
		seller models.SellerStatus
		want   string
	}{
		{models.ReviewPending, models.CancellationNone, models.SellerStatusProduction, OpenStatusPendingReview},
		{models.ReviewNeedsFix, models.CancellationNone, models.SellerStatusProduction, OpenStatusNeedsCorrection},
		{models.ReviewRejected, models.CancellationNone, models.SellerStatusProduction, OpenStatusRejected},
		{models.ReviewCancelled, models.CancellationSeller, models.SellerStatusProduction, OpenStatusCancelled},
		{models.ReviewApproved, models.CancellationApproved, models.SellerStatusProduction, OpenStatusCancelled},
		{models.ReviewApproved, models.CancellationRequested, models.SellerStatusProduction, OpenStatusInProduction},
		{models.ReviewApproved, models.CancellationRejected, models.SellerStatusPacked, OpenStatusPacked},
		{models.ReviewApproved, models.CancellationNone, models.SellerStatusHandedOff, OpenStatusHandedOff},
		{models.ReviewApproved, models.CancellationNone, models.SellerStatusShipped, OpenStatusShipped},
		{models.ReviewApproved, models.CancellationNone, models.SellerStatusDelivered, OpenStatusDelivered},
	}
	for _, c := range cases {
		o := &models.Order{ReviewStatus: c.review, CancellationStatus: c.cancel, SellerStatus: c.seller}
		if got := openStatus(o); got != c.want {
			t.Errorf("review=%s cancel=%s seller=%s → %s, want %s", c.review, c.cancel, c.seller, got, c.want)
		}
		if openStatusText[c.want] == "" {
			t.Errorf("status %s has no text", c.want)
		}
		// Every status the API reports can be asked for in the list filter.
		if !openStatusFilter(c.want, &repositories.OrderFilter{}) {
			t.Errorf("status %s is not filterable", c.want)
		}
	}
}

// Two identical requests in flight: both pass the "already sent?" lookup, and the
// unique index lets only one insert through. The loser must come back as a plain
// replay of the winner's order — not as a 500 the caller would retry forever.
//
// The race is staged with a query hook that slips the rival's order in right
// after this request's lookup found nothing.
func TestOpenCreate_LosingARaceIsAReplay(t *testing.T) {
	db := newOpenDB(t)
	// The index production gets from ensurePerformanceIndexes.
	if err := db.Exec(`CREATE UNIQUE INDEX uniq_orders_seller_api_ref ON orders (seller_id, api_ref) WHERE api_ref IS NOT NULL AND deleted_at IS NULL`).Error; err != nil {
		t.Fatalf("create index: %v", err)
	}
	staged := false
	err := db.Callback().Query().After("gorm:query").Register("test:rival_order", func(tx *gorm.DB) {
		if staged || !strings.Contains(tx.Statement.SQL.String(), "api_ref") {
			return
		}
		staged = true
		ref := "ETSY-1001"
		rival := &models.Order{InternalCode: "100777", StoreOrderID: ref, StoreOrderRef: ref, APIRef: &ref, SellerID: 1,
			ShippingName: "Rival", SellerStatus: models.SellerStatusProduction, ReviewStatus: models.ReviewPending}
		if err := tx.Session(&gorm.Session{NewDB: true}).Create(rival).Error; err != nil {
			t.Errorf("stage rival: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("register hook: %v", err)
	}

	res, err := openSvc(db).CreateOrder(sellerPrincipal(1, "S1"), "", openOrder("ETSY-1001"))
	if err != nil {
		t.Fatalf("the loser of the race must get the winner's order, got %v", err)
	}
	if !staged {
		t.Fatal("the rival was never staged — the test did not exercise the race")
	}
	if res.Created || res.Order.Code != "100777" || res.Order.Shipping.Name != "Rival" {
		t.Errorf("result = created %v, code %s", res.Created, res.Order.Code)
	}
	var orders, items int64
	db.Model(&models.Order{}).Count(&orders)
	db.Model(&models.OrderItem{}).Count(&items)
	if orders != 1 || items != 0 {
		t.Errorf("orders=%d items=%d — the losing transaction must leave nothing behind", orders, items)
	}
}

// The SKU list is exactly what the create call accepts: a SKU without a
// material is refused at create time, so it must not be offered here either.
func TestOpenListSKUs_MatchesWhatCreateAccepts(t *testing.T) {
	db := newOpenDB(t)
	svc := openSvc(db)
	db.Create(&models.SKU{Code: "NOMAT", Name: "No material"})

	rows, err := svc.ListSKUs()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Code != "TESTSKU" || rows[0].ProductName != "Test SKU" {
		t.Fatalf("skus = %+v", rows)
	}
	in := openOrder("SKU-1")
	in.Items[0].SKU = rows[0].Code
	if _, err := svc.CreateOrder(sellerPrincipal(1, "S1"), "", in); err != nil {
		t.Errorf("a listed SKU must be accepted: %v", err)
	}
}

// created_to=<a day> covers that whole day in the factory's timezone; a bare
// midnight bound would drop every order created on it.
func TestParseOpenTimeBound(t *testing.T) {
	loc := AppLocation()
	from, err := ParseOpenTimeBound("2026-10-01", "created_from", false)
	if err != nil || !from.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, loc)) {
		t.Errorf("from = %v, %v", from, err)
	}
	to, err := ParseOpenTimeBound("2026-10-01", "created_to", true)
	if err != nil || !to.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, loc).Add(-time.Nanosecond)) {
		t.Errorf("to = %v, %v", to, err)
	}
	exact, err := ParseOpenTimeBound("2026-10-01T08:30:00Z", "created_to", true)
	if err != nil || !exact.Equal(time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)) {
		t.Errorf("rfc3339 = %v, %v", exact, err)
	}
	if b, err := ParseOpenTimeBound("  ", "created_to", true); b != nil || err != nil {
		t.Errorf("empty = %v, %v", b, err)
	}
	_, err = ParseOpenTimeBound("01/10/2026", "created_from", false)
	wantAppErr(t, err, http.StatusBadRequest, "DATE_INVALID")

	// End to end: an order created today is found by created_to=today.
	db := newOpenDB(t)
	svc := openSvc(db)
	if _, err := svc.CreateOrder(sellerPrincipal(1, "S1"), "", openOrder("D-1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	today := time.Now().In(loc).Format("2006-01-02")
	lo, _ := ParseOpenTimeBound(today, "created_from", false)
	hi, _ := ParseOpenTimeBound(today, "created_to", true)
	if _, _, total, _ := svc.ListOrders(1, OpenOrderQuery{CreatedFrom: lo, CreatedTo: hi}); total != 1 {
		t.Errorf("created_from=created_to=today found %d orders, want 1", total)
	}
}
