package services

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// seedCSOrder creates an order with the shipping details a seller uploads — the
// exact fields customer support searches on and reads back to a customer.
// There is no email: the seller template dropped the column, so orders no longer
// carry one and CS no longer searches by it.
func seedCSOrder(t *testing.T, db *gorm.DB, code, storeOrder, name, phone, tracking string) *models.Order {
	t.Helper()
	o := &models.Order{
		InternalCode: code, StoreOrderID: storeOrder, SellerID: 1,
		ShippingName: name, ShippingPhone: phone,
		ShippingAddress1: "1 Main St", ShippingCity: "Austin", ShippingCountry: "US",
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
		TrackingNumber: tracking,
	}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return o
}

// csFixture returns the DB plus the OrderService the CS screen actually calls.
// Going through the service (not the repository) is deliberate: it is what
// normalises pagination, and a filter that works on the repo but returns an
// empty page through the service would be a bug the UI sees and the test does not.
func csFixture(t *testing.T) (*gorm.DB, *OrderService) {
	t.Helper()
	db := newTrackingDB(t)
	repo := repositories.New(db)
	return db, &OrderService{repo: repo, audit: &AuditService{repo: repo}}
}

func codesOf(orders []models.Order) map[string]bool {
	out := map[string]bool{}
	for _, o := range orders {
		out[o.InternalCode] = true
	}
	return out
}

// The CS screen has ONE search box. Whatever the customer quotes — the store's
// order id, our internal code, a tracking number, their name or phone — has to
// land on the right order.
func TestCSSearch_MatchesEveryIdentifierACustomerCanQuote(t *testing.T) {
	db, svc := csFixture(t)
	seedCSOrder(t, db, "100001", "ETSY-5541", "Jennifer Widmer", "4153616469", "9400111899223456789012")
	seedCSOrder(t, db, "100002", "ETSY-9002", "Brett Martino", "4152975031", "1ZA2R2010328060414")

	cases := map[string]string{
		"store order id":  "ETSY-5541",
		"internal code":   "100001",
		"tracking number": "9400111899223456789012",
		"recipient name":  "Jennifer Widmer",
		"phone":           "4153616469",
	}
	for what, term := range cases {
		t.Run(what, func(t *testing.T) {
			rows, total, err := svc.ListOrders(repositories.OrderFilter{Search: term})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if total != 1 || len(rows) != 1 {
				t.Fatalf("search %q returned total=%d rows=%d, want exactly 1", term, total, len(rows))
			}
			if rows[0].InternalCode != "100001" {
				t.Fatalf("search %q found order %s, want 100001", term, rows[0].InternalCode)
			}
		})
	}
}

// A CS agent types what they hear. Case and partial names must still find the
// order, otherwise they fall back to scrolling the list by hand.
func TestCSSearch_IsCaseInsensitiveAndPartial(t *testing.T) {
	db, svc := csFixture(t)
	seedCSOrder(t, db, "100001", "ETSY-5541", "Jennifer Widmer", "4153616469", "")

	for _, term := range []string{"jennifer", "JENNIFER", "widmer", "etsy-55", "ETSY", "wid"} {
		rows, _, err := svc.ListOrders(repositories.OrderFilter{Search: term})
		if err != nil {
			t.Fatalf("List(%q): %v", term, err)
		}
		if len(rows) != 1 {
			t.Fatalf("search %q returned %d orders, want 1", term, len(rows))
		}
	}
}

// The search ORs across five columns. If that group leaked into the surrounding
// AND chain, every other filter would silently widen — a seller-scoped search
// would start returning other sellers' customers.
func TestCSSearch_DoesNotWidenOtherFilters(t *testing.T) {
	db, svc := csFixture(t)
	mine := seedCSOrder(t, db, "100001", "ETSY-1", "Same Name", "111", "")
	other := seedCSOrder(t, db, "100002", "ETSY-2", "Same Name", "222", "")
	db.Model(other).Update("seller_id", 2)

	sellerID := mine.SellerID
	rows, total, err := svc.ListOrders(repositories.OrderFilter{Search: "Same Name", SellerID: &sellerID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].InternalCode != "100001" {
		t.Fatalf("seller-scoped search leaked: total=%d rows=%v", total, codesOf(rows))
	}
}

// The CS work queue: which orders are still waiting for a tracking number.
func TestCSFilter_HasTrackingSplitsTheQueue(t *testing.T) {
	db, svc := csFixture(t)
	seedCSOrder(t, db, "100001", "ETSY-1", "A", "1", "9400111899223456789012")
	seedCSOrder(t, db, "100002", "ETSY-2", "B", "2", "")
	seedCSOrder(t, db, "100003", "ETSY-3", "C", "3", "")

	no := false
	waiting, total, err := svc.ListOrders(repositories.OrderFilter{HasTracking: &no})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 {
		t.Fatalf("has_tracking=false returned %d orders, want 2 (%v)", total, codesOf(waiting))
	}
	yes := true
	done, total, err := svc.ListOrders(repositories.OrderFilter{HasTracking: &yes})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || !codesOf(done)["100001"] {
		t.Fatalf("has_tracking=true returned %v, want just 100001", codesOf(done))
	}
}

// Narrow fields exist for when the operator knows WHICH identifier they hold —
// a name search must not be satisfied by a store order id that happens to match.
func TestCSFilter_NarrowFieldsStayNarrow(t *testing.T) {
	db, svc := csFixture(t)
	seedCSOrder(t, db, "100001", "SMITH-1", "Jane Doe", "111", "")
	seedCSOrder(t, db, "100002", "ETSY-2", "John Smith", "222", "")

	rows, _, err := svc.ListOrders(repositories.OrderFilter{ShippingName: "smith"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].InternalCode != "100002" {
		t.Fatalf("shipping_name=smith matched %v, want only 100002", codesOf(rows))
	}
}

// CS is allowed to attach a tracking number — that is the job. Roles that have
// no business touching shipments still must not.
func TestCanEditTracking_IncludesCSOnly(t *testing.T) {
	allowed := []models.Role{
		models.RoleOwner, models.RoleAdmin, models.RoleOps,
		models.RolePacking, models.RoleShipping, models.RoleCS,
	}
	denied := []models.Role{
		models.RoleDesigner, models.RoleProduction, models.RoleQC, models.RoleSeller,
	}
	for _, r := range allowed {
		if !canEditTracking(r) {
			t.Errorf("%s should be allowed to edit tracking", r)
		}
	}
	for _, r := range denied {
		if canEditTracking(r) {
			t.Errorf("%s must NOT be allowed to edit tracking", r)
		}
	}
}

// A seller must never read another seller's journey: the events name the
// destination city, so an unchecked order id would leak customer locations.
func TestSellerTimeline_EnforcesOwnership(t *testing.T) {
	db := newTrackingDB(t)
	repo := repositories.New(db)
	mine := seedCSOrder(t, db, "100001", "ETSY-1", "A", "1", "940011")
	sync := NewTrackingSyncService(repo, &AuditService{repo: repo}, nil, "FFM", true)

	if err := repo.Tracking.SaveEvents([]models.OrderTrackingEvent{{
		OrderID: mine.ID, TrackingNumber: "940011", Description: "Delivered",
		Location: "AUSTIN, TX", Fingerprint: models.EventFingerprint("940011", "d", "Delivered", "AUSTIN, TX"),
	}}); err != nil {
		t.Fatalf("seed events: %v", err)
	}

	if _, err := sync.SellerTimeline(mine.SellerID, mine.ID); err != nil {
		t.Fatalf("owner could not read own journey: %v", err)
	}
	if _, err := sync.SellerTimeline(mine.SellerID+1, mine.ID); err == nil {
		t.Fatal("a different seller was able to read this order's journey")
	}
}

// A seller sees where the parcel is, but none of the provider bookkeeping.
func TestSellerView_CarriesTrackingWithoutInternals(t *testing.T) {
	o := models.Order{
		InternalCode: "100001", StoreOrderID: "ETSY-1",
		TrackingNumber: "940011", TrackingStatus: models.TrackingInTransit,
		TrackingDetail: "In Transit to Next Facility",
		TrackingLocation: "JAMAICA, NY", TrackingSyncError: "provider blew up",
	}
	v := toSellerView(o, false, false)
	if v.TrackingNumber != "940011" || v.TrackingStatus != models.TrackingInTransit {
		t.Fatalf("seller view lost the tracking state: %+v", v)
	}
	if v.TrackingLocation != "JAMAICA, NY" || v.TrackingDetail == "" {
		t.Fatalf("seller view lost the latest scan: %+v", v)
	}

	// NONE is our placeholder, not a shipment state: it must not reach the seller
	// as a status badge, and no stale location may ride along with it.
	empty := toSellerView(models.Order{TrackingStatus: models.TrackingNone, TrackingLocation: "JAMAICA, NY"}, false, false)
	if empty.TrackingStatus != "" {
		t.Fatalf("NONE leaked to the seller as %q", empty.TrackingStatus)
	}
	if empty.TrackingLocation != "" {
		t.Fatalf("a location leaked without a status: %q", empty.TrackingLocation)
	}
}
