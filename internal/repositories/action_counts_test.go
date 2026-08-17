package repositories

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// newCountsDB seeds exactly one item of work in each of the three badge queues:
// an order awaiting review, an order asking to be cancelled, and a note flagged
// as needing attention.
func newCountsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Order{}, &models.OrderItem{}, &models.Note{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows := []*models.Order{
		{InternalCode: "100001", StoreOrderID: "E-1", SellerID: 1,
			ReviewStatus: models.ReviewPending, CancellationStatus: models.CancellationNone},
		{InternalCode: "100002", StoreOrderID: "E-2", SellerID: 1,
			ReviewStatus: models.ReviewApproved, CancellationStatus: models.CancellationRequested},
	}
	for _, o := range rows {
		if err := db.Create(o).Error; err != nil {
			t.Fatalf("seed order: %v", err)
		}
	}
	// Two required-attention notes on opposite sides of the CS line: one the
	// workshop must fix, one a customer reported.
	notes := []*models.Note{
		{Title: "Thiếu Mockup URL", ReasonCode: "ART_MISSING", Status: models.NoteOpen,
			Severity: models.SeverityHigh, IsRequiredAttention: true,
			EntityType: models.EntityOrderItem, OwnerRole: models.RoleDesigner},
		{Title: "Khách báo giao sai địa chỉ", Status: models.NoteOpen,
			Severity: models.SeverityHigh, IsRequiredAttention: true,
			EntityType: models.EntityOrder, OwnerRole: models.RoleCS},
	}
	for _, n := range notes {
		if err := db.Create(n).Error; err != nil {
			t.Fatalf("seed note: %v", err)
		}
	}
	return db
}

// TestActionCounts_ScopedToWhatTheRoleCanSee: the sidebar badge answers per role,
// because the sidebar itself is per role. CS carries the notes item but neither
// the review nor the cancellation queue — and used to be refused outright (403 on
// every 30-second poll), which the badge swallowed silently.
//
// CS's notes number counts only the notes owned by CS. Counting the workshop's
// too would point them at jobs they cannot act on and must not touch.
func TestActionCounts_ScopedToWhatTheRoleCanSee(t *testing.T) {
	repo := New(newCountsDB(t))

	ops, err := repo.ActionCounts(models.RoleOps)
	if err != nil {
		t.Fatalf("ops counts: %v", err)
	}
	if ops.Review != 1 || ops.Cancellations != 1 || ops.Notes != 2 {
		t.Fatalf("ops = review %d / cancel %d / notes %d, want 1/1/2",
			ops.Review, ops.Cancellations, ops.Notes)
	}

	cs, err := repo.ActionCounts(models.RoleCS)
	if err != nil {
		t.Fatalf("cs counts: %v", err)
	}
	if cs.Notes != 1 {
		t.Fatalf("cs notes = %d, want 1 — only the customer-reported note is theirs", cs.Notes)
	}
	if cs.Review != 0 || cs.Cancellations != 0 {
		t.Fatalf("cs = review %d / cancel %d, want 0/0 — neither queue is on their sidebar",
			cs.Review, cs.Cancellations)
	}
}
