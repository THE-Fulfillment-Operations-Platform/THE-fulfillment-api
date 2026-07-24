package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// The database lives in another region (Supabase ap-southeast-1) while the API
// talks to it over the pooler, so wall-clock cost of a request is dominated by
// the NUMBER of statements it sends, not by how long any one of them runs —
// every statement pays a full network round trip (~40-60ms measured).
//
// These tests pin that count for the batch-status path, which is the hottest
// button in the workshop ("Đã in" / "Đã cắt"). They fail loudly if someone
// reintroduces a per-item query or an extra full reload.

// countingLogger tallies every statement GORM sends (including BEGIN/COMMIT,
// which are round trips too).
type countingLogger struct {
	gormlogger.Interface
	n *int
}

func (l countingLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	*l.n++
}

func countQueries(db *gorm.DB, fn func(*gorm.DB)) int {
	n := 0
	fn(db.Session(&gorm.Session{Logger: countingLogger{Interface: gormlogger.Discard, n: &n}}))
	return n
}

// seedStatusBatch builds a flat batch with both production links and `n` items.
func seedStatusBatch(t *testing.T, db *gorm.DB, n int) *models.Batch {
	t.Helper()
	mat := &models.Material{Code: "MICA", Name: "Mica"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	order := &models.Order{
		InternalCode: "100002", StoreOrderID: "Etsy-2", SellerID: 1,
		SellerStatus: models.SellerStatusProduction, ReviewStatus: models.ReviewApproved,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	batch := &models.Batch{Code: "#101002", MaterialID: mat.ID, Status: models.StatusPending}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	if err := db.Create(&[]models.BatchLink{
		{BatchID: batch.ID, Kind: models.BatchLinkPrint, URL: "https://x/print", LinkUpdatedAt: time.Now()},
		{BatchID: batch.ID, Kind: models.BatchLinkCut, URL: "https://x/cut", LinkUpdatedAt: time.Now()},
	}).Error; err != nil {
		t.Fatalf("seed links: %v", err)
	}
	items := make([]models.OrderItem, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, models.OrderItem{
			OrderID: order.ID, InternalCode: fmt.Sprintf("100002_%d/%d", i+1, n), SKUCode: "BR-A-1.6-KEP",
			LineNo: i + 1, Quantity: 1, InternalStatus: models.StatusPending,
		})
	}
	if err := db.Create(&items).Error; err != nil {
		t.Fatalf("seed order items: %v", err)
	}
	parts := make([]models.BatchItem, 0, n)
	for _, oi := range items {
		parts = append(parts, models.BatchItem{
			BatchID: batch.ID, OrderItemID: oi.ID, MaterialID: mat.ID, Status: models.StatusPending,
		})
	}
	if err := db.Create(&parts).Error; err != nil {
		t.Fatalf("seed batch items: %v", err)
	}
	return batch
}

// TestUpdateStatus_RoundTripBudget: advancing a single-item batch must stay
// within a fixed statement budget. It used to send 34 (~3s against a remote DB).
func TestUpdateStatus_RoundTripBudget(t *testing.T) {
	db := newSplitDB(t)
	batch := seedStatusBatch(t, db, 1)
	actor := Actor{ID: 1, Role: models.RoleProduction}

	const budget = 20
	n := countQueries(db, func(tx *gorm.DB) {
		svc := newBatchService(tx)
		if _, err := svc.UpdateStatus(actor, batch.ID, UpdateStatusInput{Status: string(models.StatusPrinted)}); err != nil {
			t.Fatalf("update status: %v", err)
		}
	})
	t.Logf("UpdateStatus (1 item) sent %d statements", n)
	if n > budget {
		t.Errorf("UpdateStatus sent %d statements, budget is %d — each one costs a network round trip", n, budget)
	}
}

// TestUpdateStatus_ScalesWithBatchSize: a 40-item batch must not cost 40 extra
// round trips. The per-item UPDATE loop is the thing this guards against.
func TestUpdateStatus_ScalesWithBatchSize(t *testing.T) {
	db := newSplitDB(t)
	small := seedStatusBatch(t, db, 1)
	actor := Actor{ID: 1, Role: models.RoleProduction}
	base := countQueries(db, func(tx *gorm.DB) {
		svc := newBatchService(tx)
		if _, err := svc.UpdateStatus(actor, small.ID, UpdateStatusInput{Status: string(models.StatusPrinted)}); err != nil {
			t.Fatalf("update small: %v", err)
		}
	})

	db2 := newSplitDB(t)
	big := seedStatusBatch(t, db2, 40)
	large := countQueries(db2, func(tx *gorm.DB) {
		svc := newBatchService(tx)
		if _, err := svc.UpdateStatus(actor, big.ID, UpdateStatusInput{Status: string(models.StatusPrinted)}); err != nil {
			t.Fatalf("update large: %v", err)
		}
	})
	t.Logf("UpdateStatus: 1 item = %d statements, 40 items = %d", base, large)
	// A few extra statements for the bigger payload are fine; one per item is not.
	if large > base+4 {
		t.Errorf("40-item batch sent %d statements vs %d for one item — the cascade is still per-item", large, base)
	}
}

// TestFindByID_RoundTripBudget: the batch detail payload (also what the status
// PATCH returns) must not fan out into a query per association.
func TestFindByID_RoundTripBudget(t *testing.T) {
	db := newSplitDB(t)
	batch := seedStatusBatch(t, db, 3)

	const budget = 5
	var got *models.Batch
	n := countQueries(db, func(tx *gorm.DB) {
		var err error
		got, err = repositories.New(tx).Batch.FindByID(batch.ID)
		if err != nil {
			t.Fatalf("find batch: %v", err)
		}
	})
	t.Logf("FindByID (flat batch) sent %d statements", n)
	if n > budget {
		t.Errorf("FindByID sent %d statements, budget is %d", n, budget)
	}

	// Folding associations into joins must not quietly stop populating them —
	// the batch detail screen renders every one of these.
	if got.Material.Name != "Mica" {
		t.Errorf("Material not loaded: %+v", got.Material)
	}
	if len(got.Links) != 2 {
		t.Errorf("expected 2 links, got %d", len(got.Links))
	}
	if len(got.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(got.Items))
	}
	for _, it := range got.Items {
		if it.OrderItem == nil || it.OrderItem.SKUCode != "BR-A-1.6-KEP" {
			t.Errorf("batch item %d: OrderItem not loaded: %+v", it.ID, it.OrderItem)
		}
		if it.OrderItem != nil && (it.OrderItem.Order == nil || it.OrderItem.Order.StoreOrderID != "Etsy-2") {
			t.Errorf("batch item %d: OrderItem.Order not loaded", it.ID)
		}
		if it.Material == nil || it.Material.Name != "Mica" {
			t.Errorf("batch item %d: Material not loaded", it.ID)
		}
	}
}
