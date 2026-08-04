package services

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/tracking24h"
)

// Replacing a tracking number is routine in logistics — a mistyped 20-character
// code, a parcel returned and re-sent, a carrier re-label. These tests pin what
// must happen when it does, because the failure mode is silent: the screen keeps
// showing the OLD parcel's journey and status, which looks authoritative.

func newReplaceFixture(t *testing.T, f *fakeProvider) (*gorm.DB, *OrderService, *models.Order) {
	t.Helper()
	db := newTrackingDB(t)
	repo := repositories.New(db)
	sync := NewTrackingSyncService(repo, &AuditService{repo: repo}, f.start(t), "FFM", true)
	svc := &OrderService{repo: repo, audit: &AuditService{repo: repo}, tracking: sync}

	order := &models.Order{
		InternalCode: "100001", StoreOrderID: "SO-1", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusHandedOff,
		TrackingStatus: models.TrackingNone,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return db, svc, order
}

func setTracking(t *testing.T, svc *OrderService, orderID uint, number string) {
	t.Helper()
	if _, err := svc.UpdateTracking(Actor{ID: 1, Role: models.RoleCS}, orderID,
		UpdateTrackingInput{TrackingNumber: &number}); err != nil {
		t.Fatalf("UpdateTracking(%s): %v", number, err)
	}
}

// The reported bug: attach a number, get its journey, then correct the number —
// and the old parcel's scans were still on screen.
func TestReplaceTrackingNumber_JourneyFollowsTheNewParcel(t *testing.T) {
	f := newFakeProvider()
	f.parcels["OLD111"] = &fakeParcel{Carrier: "USPS", Status: "In Transit", Location: "JAMAICA, NY"}
	f.events["OLD111"] = []tracking24h.Event{
		{EventDate: "July 20, 2026 9:00 AM", Description: "Shipment information received", Location: "JAMAICA, NY"},
	}
	f.parcels["NEW222"] = &fakeParcel{Carrier: "ACME", Status: "Delivered", Location: "AUSTIN, TX"}
	f.events["NEW222"] = []tracking24h.Event{
		{EventDate: "July 29, 2026 3:00 PM", Description: "Delivered, Front Door", Location: "AUSTIN, TX"},
	}
	db, svc, order := newReplaceFixture(t, f)

	setTracking(t, svc, order.ID, "OLD111")
	waitFor(t, 3*time.Second, func() bool {
		var o models.Order
		db.First(&o, order.ID)
		return o.TrackingStatus == models.TrackingInTransit
	}, "first number never synced")

	setTracking(t, svc, order.ID, "NEW222")
	waitFor(t, 3*time.Second, func() bool {
		var o models.Order
		db.First(&o, order.ID)
		return o.TrackingStatus == models.TrackingDelivered
	}, "replacement number never synced")

	events, err := svc.tracking.Timeline(order.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("timeline shows %d scans, want only the new parcel's 1", len(events))
	}
	if events[0].Description != "Delivered, Front Door" {
		t.Fatalf("timeline still shows the old parcel: %q", events[0].Description)
	}
	if events[0].TrackingNumber != "NEW222" {
		t.Fatalf("scan belongs to %q, want NEW222", events[0].TrackingNumber)
	}

	// The old scans are kept — they are the evidence for a carrier claim — just
	// not shown next to the parcel currently in flight.
	var kept int64
	db.Model(&models.OrderTrackingEvent{}).
		Where("order_id = ? AND tracking_number = ?", order.ID, "OLD111").Count(&kept)
	if kept != 1 {
		t.Fatalf("old parcel's history was destroyed (%d rows left)", kept)
	}
}

// Between the edit and the first sync of the new parcel there is a window. The
// order must not keep advertising the old parcel's carrier state during it.
func TestReplaceTrackingNumber_ClearsStaleCarrierState(t *testing.T) {
	f := newFakeProvider()
	f.parcels["OLD111"] = &fakeParcel{
		Carrier: "USPS", Status: "Delivered",
		Detail: "Delivered, In/At Mailbox", Location: "HAMPTON BAYS, NY",
	}
	db, svc, order := newReplaceFixture(t, f)

	setTracking(t, svc, order.ID, "OLD111")
	waitFor(t, 3*time.Second, func() bool {
		var o models.Order
		db.First(&o, order.ID)
		return o.TrackingStatus == models.TrackingDelivered
	}, "first number never synced")

	// The replacement is unknown to the provider, so nothing will overwrite the
	// mirrored columns — exactly the case where stale data would linger.
	updated, err := svc.UpdateTracking(Actor{ID: 1, Role: models.RoleCS}, order.ID,
		UpdateTrackingInput{TrackingNumber: strPtr("NEW222")})
	if err != nil {
		t.Fatalf("UpdateTracking: %v", err)
	}
	if updated.TrackingStatus == models.TrackingDelivered {
		t.Error("order still reports the old parcel as Delivered")
	}
	if updated.TrackingDetail != "" || updated.TrackingLocation != "" {
		t.Errorf("stale scan left behind: detail=%q location=%q", updated.TrackingDetail, updated.TrackingLocation)
	}
	if updated.TrackingSyncedAt != nil {
		t.Error("order looks freshly synced when the new parcel has never been checked")
	}
	if updated.TrackingDeliveredAt != nil {
		t.Error("old delivery timestamp survived onto the new parcel")
	}
}

// Two parcels tagged with one store order id is the ambiguity the reverse lookup
// refuses to guess through, so the abandoned number must give the tag back.
func TestReplaceTrackingNumber_ReleasesTheOldParcelTag(t *testing.T) {
	f := newFakeProvider()
	f.parcels["OLD111"] = &fakeParcel{Carrier: "USPS", Status: "In Transit"}
	f.parcels["NEW222"] = &fakeParcel{Carrier: "USPS", Status: "In Transit"}
	_, svc, order := newReplaceFixture(t, f)

	setTracking(t, svc, order.ID, "OLD111")
	waitFor(t, 3*time.Second, func() bool { return f.parcels["OLD111"].Description == "FFM:SO-1" },
		"first number was never tagged")

	setTracking(t, svc, order.ID, "NEW222")
	waitFor(t, 3*time.Second, func() bool { return f.parcels["NEW222"].Description == "FFM:SO-1" },
		"replacement number was never tagged")
	waitFor(t, 3*time.Second, func() bool { return f.parcels["OLD111"].Description == "" },
		"old parcel still answers to this store order id")
}

// CS pastes a number belonging to ANOTHER order, then fixes the typo.
//
// The damage is done at paste time: registering already overwrote that parcel's
// tag. What the fix must guarantee is that the mistyped parcel stops answering
// to THIS order — otherwise the reverse lookup would later pull a stranger's
// parcel onto it, which is far worse than a missing tag. The other order loses
// nothing that matters: it still holds the number, and syncing follows the
// number, not the tag (the tag only serves orders that have no number yet).
func TestReplaceTrackingNumber_MistypedParcelStopsAnsweringToThisOrder(t *testing.T) {
	f := newFakeProvider()
	f.parcels["OTHER99"] = &fakeParcel{Carrier: "USPS", Status: "In Transit", Description: "FFM:SO-999"}
	f.parcels["MINE11"] = &fakeParcel{Carrier: "USPS", Status: "In Transit"}
	_, svc, order := newReplaceFixture(t, f)

	setTracking(t, svc, order.ID, "OTHER99") // typo: someone else's parcel
	setTracking(t, svc, order.ID, "MINE11")  // corrected

	waitFor(t, 3*time.Second, func() bool { return f.parcels["MINE11"].Description == "FFM:SO-1" },
		"corrected number was never tagged")
	waitFor(t, 3*time.Second, func() bool { return f.parcels["OTHER99"].Description != "FFM:SO-1" },
		"the mistyped parcel still answers to this order")
}

// And the release must not touch a parcel we never claimed: an order that never
// tagged the number it is abandoning has no business blanking someone else's.
func TestReleaseNumber_SkipsParcelTaggedForSomeoneElse(t *testing.T) {
	f := newFakeProvider()
	f.parcels["THEIRS"] = &fakeParcel{Carrier: "USPS", Status: "In Transit", Description: "FFM:SO-999"}
	db := newTrackingDB(t)
	repo := repositories.New(db)
	sync := NewTrackingSyncService(repo, &AuditService{repo: repo}, f.start(t), "FFM", true)

	order := &models.Order{
		InternalCode: "100001", StoreOrderID: "SO-1", SellerID: 1,
		SellerStatus: models.SellerStatusHandedOff,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}

	sync.ReleaseNumberAsync(order, "THEIRS")
	time.Sleep(300 * time.Millisecond)

	if got := f.parcels["THEIRS"].Description; got != "FFM:SO-999" {
		t.Fatalf("another order's tag was disturbed: %q", got)
	}
}

func strPtr(s string) *string { return &s }
