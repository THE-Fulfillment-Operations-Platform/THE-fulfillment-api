package services

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func reviewSvc(db *gorm.DB) *ReviewService {
	repo := repositories.New(db)
	return &ReviewService{repo: repo, audit: &AuditService{repo: repo}}
}

// TestDailySeq_AssignedAndIncrements verifies "STT trong ngày": every imported
// order gets a per-day sequence assigned atomically at creation, numbers are
// distinct and start at 1, and order_date is stamped.
func TestDailySeq_AssignedAndIncrements(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	prev, err := svc.Preview(actor, 1, "XLSX", "f.xlsx", []ImportRow{
		row("D-1", "A"), row("D-2", "B"), row("D-3", "C"),
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := svc.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	orders := loadOrders(t, db, 1)
	if len(orders) != 3 {
		t.Fatalf("want 3 orders, got %d", len(orders))
	}
	seqs := map[int]bool{}
	for _, o := range orders {
		if o.OrderDate == "" {
			t.Fatalf("order %d has empty order_date", o.ID)
		}
		if o.DailySeq < 1 {
			t.Fatalf("order %d daily_seq not assigned: %d", o.ID, o.DailySeq)
		}
		if seqs[o.DailySeq] {
			t.Fatalf("duplicate daily_seq %d", o.DailySeq)
		}
		seqs[o.DailySeq] = true
	}
	if len(seqs) != 3 {
		t.Fatalf("want 3 distinct daily seqs, got %d", len(seqs))
	}
	// Numbers should be exactly 1,2,3 for a single day.
	for want := 1; want <= 3; want++ {
		if !seqs[want] {
			t.Fatalf("missing daily_seq %d (got %v)", want, seqs)
		}
	}
}

// TestBulkApprove_PartialSuccess verifies bulk approve approves valid orders,
// skips a blocked order (unmapped SKU) and a non-existent id, and reports both.
func TestBulkApprove_PartialSuccess(t *testing.T) {
	db := newImportDB(t)
	imp := importSvc(db)
	rsvc := reviewSvc(db)
	actor := Actor{ID: 1, Role: models.RoleOps}

	prev, err := imp.Preview(actor, 1, "XLSX", "f.xlsx", []ImportRow{row("OK-1", "A"), row("OK-2", "B")})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := imp.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	valid := loadOrders(t, db, 1)
	if len(valid) != 2 {
		t.Fatalf("want 2 valid orders, got %d", len(valid))
	}

	// A blocker order: PENDING_REVIEW with an item whose SKU is unmapped (SKUID nil).
	blk := &models.Order{
		StoreOrderID: "BLK", StoreOrderRef: "BLK", SellerID: 1,
		ReviewStatus: models.ReviewPending, SellerStatus: models.SellerStatusProduction,
		ShippingName: "X", ShippingAddress1: "Y", ShippingCountry: "US",
		OrderDate: "2026-07-22", DailySeq: 99, InternalCode: "199999",
	}
	if err := db.Create(blk).Error; err != nil {
		t.Fatalf("seed blocker order: %v", err)
	}
	if err := db.Create(&models.OrderItem{
		OrderID: blk.ID, InternalCode: "199999_1/1", SKUCode: "NOPE", Quantity: 1,
		InternalStatus: models.StatusPending, DesignStatus: models.DesignMissing,
	}).Error; err != nil {
		t.Fatalf("seed blocker item: %v", err)
	}

	ids := []uint{valid[0].ID, valid[1].ID, blk.ID, 999999}
	res, err := rsvc.BulkApprove(actor, BulkApproveInput{OrderIDs: ids})
	if err != nil {
		t.Fatalf("bulk approve: %v", err)
	}
	if res.ApprovedCount != 2 {
		t.Fatalf("want 2 approved, got %d (%v); skipped=%+v", res.ApprovedCount, res.Approved, res.Skipped)
	}
	if res.SkippedCount != 2 {
		t.Fatalf("want 2 skipped, got %d (%+v)", res.SkippedCount, res.Skipped)
	}
	codes := map[string]bool{}
	for _, s := range res.Skipped {
		codes[s.Code] = true
	}
	if !codes["HAS_BLOCKER"] {
		t.Fatalf("expected a HAS_BLOCKER skip, got %+v", res.Skipped)
	}
	if !codes["NOT_FOUND"] {
		t.Fatalf("expected a NOT_FOUND skip, got %+v", res.Skipped)
	}

	// The two valid orders must now be APPROVED in the DB.
	for _, id := range res.Approved {
		var o models.Order
		if err := db.First(&o, id).Error; err != nil {
			t.Fatalf("reload order %d: %v", id, err)
		}
		if o.ReviewStatus != models.ReviewApproved {
			t.Fatalf("order %d not approved: %s", id, o.ReviewStatus)
		}
	}

	// Re-approving an already-approved order is skipped as NOT_REVIEWABLE.
	res2, err := rsvc.BulkApprove(actor, BulkApproveInput{OrderIDs: []uint{valid[0].ID}})
	if err != nil {
		t.Fatalf("bulk approve 2: %v", err)
	}
	if res2.ApprovedCount != 0 || res2.SkippedCount != 1 || res2.Skipped[0].Code != "NOT_REVIEWABLE" {
		t.Fatalf("re-approve should skip NOT_REVIEWABLE, got %+v", res2)
	}
}
