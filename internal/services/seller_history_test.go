package services

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newHistoryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Seller{}, &models.User{}, &models.Order{}, &models.OrderItem{},
		&models.StatusHistory{}, &models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func historyFixture(t *testing.T) (*gorm.DB, *OrderService) {
	t.Helper()
	db := newHistoryDB(t)
	repo := repositories.New(db)
	return db, &OrderService{repo: repo, audit: &AuditService{repo: repo}}
}

func addHistory(t *testing.T, db *gorm.DB, orderID uint, from, to string, by *uint, note string) {
	t.Helper()
	row := &models.StatusHistory{
		EntityType: models.EntityOrder, EntityID: orderID,
		FromStatus: from, ToStatus: to, ChangedByID: by, Note: note,
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("seed history: %v", err)
	}
}

// The whole point of the endpoint: the seller reads the order's own trail, with
// the review vocabulary and the production vocabulary each tagged so the UI can
// label them, and the actor reduced to a side rather than a person.
func TestSellerOrderHistory_ReturnsTaggedTrail(t *testing.T) {
	db, svc := historyFixture(t)
	sellerUser := &models.User{Email: "s@x.test", FullName: "S", Role: models.RoleSeller, SellerID: ptrUint(1), PasswordHash: "x"}
	opsUser := &models.User{Email: "o@x.test", FullName: "O", Role: models.RoleOps, PasswordHash: "x"}
	if err := db.Create(sellerUser).Error; err != nil {
		t.Fatalf("seed seller user: %v", err)
	}
	if err := db.Create(opsUser).Error; err != nil {
		t.Fatalf("seed ops user: %v", err)
	}
	o := &models.Order{InternalCode: "100001", StoreOrderID: "SO-1", SellerID: 1}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}

	addHistory(t, db, o.ID, "PENDING_REVIEW", "APPROVED", &opsUser.ID, "duyệt")
	addHistory(t, db, o.ID, "PRODUCTION", "HANDED_OFF", nil, "handed off")
	addHistory(t, db, o.ID, "APPROVED", "CANCELLED", &sellerUser.ID, "seller cancelled")

	events, err := svc.SellerOrderHistory(1, o.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d", len(events))
	}

	// Oldest first, matching the repository's id-ascending read.
	if events[0].Kind != "review" || events[0].ToStatus != "APPROVED" {
		t.Errorf("event0 = %+v, want review/APPROVED", events[0])
	}
	if events[0].Actor != "ops" {
		t.Errorf("ops decision attributed to %q, want ops", events[0].Actor)
	}
	// HANDED_OFF is a SellerStatus, not a review status — it must not be labelled
	// with the review vocabulary or the UI prints the wrong Vietnamese word.
	if events[1].Kind != "production" {
		t.Errorf("HANDED_OFF tagged %q, want production", events[1].Kind)
	}
	if events[1].Actor != "system" {
		t.Errorf("unattributed row = %q, want system", events[1].Actor)
	}
	// The seller's own action reads as theirs, so the timeline does not look like
	// the factory cancelled an order the seller cancelled themselves.
	if events[2].Actor != "seller" {
		t.Errorf("seller's own action = %q, want seller", events[2].Actor)
	}
	if events[2].Kind != "review" {
		t.Errorf("CANCELLED tagged %q, want review", events[2].Kind)
	}
}

// Ownership is the security boundary: a cancellation reason and the statuses
// alone tell another seller what happened to an order that is not theirs.
func TestSellerOrderHistory_RejectsOtherSellersOrder(t *testing.T) {
	db, svc := historyFixture(t)
	o := &models.Order{InternalCode: "100002", StoreOrderID: "SO-2", SellerID: 7}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	addHistory(t, db, o.ID, "PENDING_REVIEW", "APPROVED", nil, "duyệt")

	if _, err := svc.SellerOrderHistory(1, o.ID); err == nil {
		t.Fatal("seller 1 read seller 7's history")
	} else if ae, ok := apperr.As(err); !ok || ae.Status != 403 {
		t.Fatalf("want 403, got %v", err)
	}
}

func TestSellerOrderHistory_MissingOrderIsNotFound(t *testing.T) {
	_, svc := historyFixture(t)
	if _, err := svc.SellerOrderHistory(1, 4242); err == nil {
		t.Fatal("want error for missing order")
	} else if ae, ok := apperr.As(err); !ok || ae.Status != 404 {
		t.Fatalf("want 404, got %v", err)
	}
}

// The hand-off note is written internally as "handed off to <carrier>". Serving
// it raw would name the transport partner in the one place the rest of the
// seller view works hard to keep him out of.
func TestSellerOrderHistory_RedactsPartnerFromNotes(t *testing.T) {
	withPartnerConfig(t, "THE", "Acme Express", "ACME")
	db, svc := historyFixture(t)
	o := &models.Order{InternalCode: "100003", StoreOrderID: "SO-3", SellerID: 1}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	addHistory(t, db, o.ID, "PACKED", "HANDED_OFF", nil, "handed off to Acme Express")

	events, err := svc.SellerOrderHistory(1, o.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if events[0].Note != "handed off to THE" {
		t.Errorf("note = %q, partner name survived redaction", events[0].Note)
	}
}

// Item / batch rows carry the print-cut-QC pipeline that SellerStatus exists to
// hide. They live in the same table, so the entity filter is load-bearing.
func TestSellerOrderHistory_ExcludesInternalPipelineRows(t *testing.T) {
	db, svc := historyFixture(t)
	o := &models.Order{InternalCode: "100004", StoreOrderID: "SO-4", SellerID: 1}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	addHistory(t, db, o.ID, "PENDING_REVIEW", "APPROVED", nil, "duyệt")
	// Same entity id, different entity type — an unfiltered read would return it.
	if err := db.Create(&models.StatusHistory{
		EntityType: models.EntityOrderItem, EntityID: o.ID,
		FromStatus: "PRINTED", ToStatus: "CUT", Note: "derived from batch parts",
	}).Error; err != nil {
		t.Fatalf("seed item history: %v", err)
	}

	events, err := svc.SellerOrderHistory(1, o.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 order-level event, got %d — internal pipeline leaked", len(events))
	}
	for _, ev := range events {
		if ev.ToStatus == "CUT" || ev.FromStatus == "PRINTED" {
			t.Errorf("internal status leaked to seller: %+v", ev)
		}
	}
}

func ptrUint(v uint) *uint { return &v }
