package services

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/shipping"
)

func newShipDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.User{}, &models.Seller{}, &models.Order{}, &models.OrderItem{},
		&models.Package{}, &models.PackageItem{}, &models.Handoff{},
		&models.StatusHistory{}, &models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedHandedOff(t *testing.T, db *gorm.DB) (*models.Order, *models.Handoff) {
	t.Helper()
	order := &models.Order{
		InternalCode: "ORD-000001", StoreOrderID: "US-1", StoreOrderRef: "US-1", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusHandedOff,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	handoff := &models.Handoff{
		Code: "THE-HO-000001", OrderID: &order.ID, Carrier: "THE", Status: models.HandoffHandedOff,
	}
	if err := db.Create(handoff).Error; err != nil {
		t.Fatalf("seed handoff: %v", err)
	}
	return order, handoff
}

// TestMarkShipped_EndToEnd drives the real "Đánh dấu đã gửi" flow: a HANDED_OFF
// handoff gets a carrier + tracking number, flips to SHIPPED, and its order
// advances to seller status SHIPPED (the visible end state).
func TestMarkShipped_EndToEnd(t *testing.T) {
	db := newShipDB(t)
	repo := repositories.New(db)
	svc := &PackingService{repo: repo, audit: &AuditService{repo: repo}, carrier: shipping.NewNoopCarrier("THE")}
	order, handoff := seedHandedOff(t, db)

	got, err := svc.MarkShipped(Actor{ID: 1, Role: models.RoleShipping}, handoff.ID, MarkShippedInput{
		Carrier: "GHN", TrackingNumber: "GHN123456789", LabelURL: "https://labels.example.com/x.pdf",
	})
	if err != nil {
		t.Fatalf("MarkShipped failed: %v", err)
	}
	if got.Status != models.HandoffShipped {
		t.Fatalf("handoff: want SHIPPED, got %s", got.Status)
	}
	if got.Carrier != "GHN" || got.TrackingNumber != "GHN123456789" {
		t.Fatalf("handoff carrier/tracking not persisted: %+v", got)
	}

	// Order rolled up to SHIPPED.
	fresh, err := repo.Order.FindByID(order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if fresh.SellerStatus != models.SellerStatusShipped {
		t.Fatalf("order: want seller status SHIPPED, got %s", fresh.SellerStatus)
	}
}

// TestMarkShipped_Guards covers the validation + idempotency rails.
func TestMarkShipped_Guards(t *testing.T) {
	db := newShipDB(t)
	repo := repositories.New(db)
	svc := &PackingService{repo: repo, audit: &AuditService{repo: repo}, carrier: shipping.NewNoopCarrier("THE")}
	_, handoff := seedHandedOff(t, db)
	actor := Actor{ID: 1, Role: models.RoleShipping}

	// Missing tracking number → 400.
	if _, err := svc.MarkShipped(actor, handoff.ID, MarkShippedInput{Carrier: "GHN"}); err == nil {
		t.Fatal("expected error for missing tracking number")
	} else if ae, ok := apperr.As(err); !ok || ae.Status != 400 {
		t.Fatalf("want 400 bad request, got %v", err)
	}

	// First real ship succeeds.
	if _, err := svc.MarkShipped(actor, handoff.ID, MarkShippedInput{TrackingNumber: "T-1"}); err != nil {
		t.Fatalf("first ship failed: %v", err)
	}
	// Second ship is a conflict (already shipped).
	if _, err := svc.MarkShipped(actor, handoff.ID, MarkShippedInput{TrackingNumber: "T-2"}); err == nil {
		t.Fatal("expected conflict on re-ship")
	} else if ae, ok := apperr.As(err); !ok || ae.Status != 409 {
		t.Fatalf("want 409 conflict, got %v", err)
	}

	// Unknown handoff → 404.
	if _, err := svc.MarkShipped(actor, 9999, MarkShippedInput{TrackingNumber: "T-3"}); err == nil {
		t.Fatal("expected not found for unknown handoff")
	} else if ae, ok := apperr.As(err); !ok || ae.Status != 404 {
		t.Fatalf("want 404 not found, got %v", err)
	}
}
