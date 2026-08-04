package services

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/shipping"
	"the-fulfillment/backend/internal/tracking24h"
)

// newHandoffDB carries the packing tables too — CreateHandoff walks the package
// before it will hand anything over.
func newHandoffDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Batch/material tables come along because FindByID preloads an item's batch
	// parts — the handoff path loads the whole order, not a slim projection.
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Order{}, &models.OrderItem{},
		&models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Batch{}, &models.BatchItem{},
		&models.Package{}, &models.PackageItem{}, &models.Handoff{},
		&models.StatusHistory{}, &models.OrderTrackingEvent{}, &models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedPackedOrder builds an approved order whose package is fully scanned — the
// state the packing station is in right before someone presses "Bàn giao cho THE".
func seedPackedOrder(t *testing.T, db *gorm.DB, trackingNumber string) *models.Order {
	t.Helper()
	order := &models.Order{
		InternalCode: "100001", StoreOrderID: "SO-1", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusPacked,
		TrackingNumber: trackingNumber, TrackingStatus: models.TrackingNone,
	}
	if trackingNumber != "" {
		order.TrackingStatus = models.TrackingPending
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	item := &models.OrderItem{
		OrderID: order.ID, LineNo: 1, InternalCode: "100001_1/1",
		SKUCode: "SKU-1", Quantity: 1, InternalStatus: models.StatusQCPassed,
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	pkg := &models.Package{Code: "PKG-1", OrderID: order.ID, Status: models.PackageOpen}
	if err := db.Create(pkg).Error; err != nil {
		t.Fatalf("seed package: %v", err)
	}
	// Fully scanned: expected == scanned, which is what unblocks the handoff.
	if err := db.Create(&models.PackageItem{
		PackageID: pkg.ID, OrderItemID: item.ID, ExpectedQty: 1, ScannedQty: 1,
	}).Error; err != nil {
		t.Fatalf("seed package item: %v", err)
	}
	return order
}

func newPackingService(db *gorm.DB, tracking *TrackingSyncService) *PackingService {
	repo := repositories.New(db)
	return &PackingService{
		repo: repo, audit: &AuditService{repo: repo},
		carrier: shipping.NewNoopCarrier("THE"), tracking: tracking,
	}
}

// The hinge of the whole design: pressing "Bàn giao cho THE" is what starts the
// shipping half of the order's life. Before it, nothing reaches the provider;
// the handoff itself is what kicks tracking off for a number CS attached earlier.
func TestCreateHandoff_StartsTracking(t *testing.T) {
	db := newHandoffDB(t)
	f := newFakeProvider()
	f.parcels["940011"] = &fakeParcel{Carrier: "USPS", Status: "In Transit", Location: "JAMAICA, NY"}
	f.events["940011"] = []tracking24h.Event{
		{EventDate: "July 27, 2026 9:53 PM", Description: "In Transit", Location: "JAMAICA, NY"},
	}

	sync := newSync(t, db, f)
	svc := newPackingService(db, sync)
	order := seedPackedOrder(t, db, "940011")

	// Nothing has been asked of the provider while the order sat packed.
	if f.calls["register"]+f.calls["detail"] != 0 {
		t.Fatalf("provider contacted before handover (register=%d detail=%d)", f.calls["register"], f.calls["detail"])
	}

	handoff, err := svc.CreateHandoff(Actor{ID: 1, Role: models.RoleOps}, HandoffInput{OrderID: &order.ID})
	if err != nil {
		t.Fatalf("CreateHandoff: %v", err)
	}
	if handoff.Status != models.HandoffHandedOff {
		t.Fatalf("handoff status = %s, want HANDED_OFF", handoff.Status)
	}

	// The push is fire-and-forget so the packing station never waits on a third
	// party; poll briefly for the effect instead of sleeping a fixed amount.
	waitFor(t, 3*time.Second, func() bool {
		var got models.Order
		db.First(&got, order.ID)
		return got.TrackingStatus == models.TrackingInTransit
	}, "tracking never started after handover")

	if f.calls["register"] == 0 {
		t.Error("parcel was never registered with the provider")
	}
	if got := f.parcels["940011"].Description; got != "FFM:SO-1" {
		t.Errorf("parcel description = %q, want FFM:SO-1", got)
	}
	var got models.Order
	db.First(&got, order.ID)
	// The provider answered "In Transit" on that first sync, so the order is no
	// longer merely handed over — the parcel is demonstrably moving. This used to
	// assert HANDED_OFF, which is precisely the bug it was pinning in place: an
	// order frozen at the last state a human clicked, however far the carrier had
	// already carried it.
	if got.SellerStatus != models.SellerStatusShipped {
		t.Errorf("seller_status = %s, want SHIPPED (carrier reported In Transit)", got.SellerStatus)
	}
	if got.TrackingLocation != "JAMAICA, NY" {
		t.Errorf("latest scan not mirrored: %+v", got.TrackingLocation)
	}
	var events int64
	db.Model(&models.OrderTrackingEvent{}).Where("order_id = ?", order.ID).Count(&events)
	if events == 0 {
		t.Error("journey was not collected after handover")
	}
}

// An order handed over WITHOUT a tracking number must not be pushed anywhere —
// there is no number to register. The periodic reverse lookup covers it instead.
func TestCreateHandoff_NoTrackingNumberIsQuiet(t *testing.T) {
	db := newHandoffDB(t)
	f := newFakeProvider()
	sync := newSync(t, db, f)
	svc := newPackingService(db, sync)
	order := seedPackedOrder(t, db, "")

	if _, err := svc.CreateHandoff(Actor{ID: 1, Role: models.RoleOps}, HandoffInput{OrderID: &order.ID}); err != nil {
		t.Fatalf("CreateHandoff: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if f.calls["register"] != 0 {
		t.Fatalf("provider was called %d time(s) for an order with no tracking number", f.calls["register"])
	}
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, limit time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}
