package services

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// countStatements records every SQL statement issued on db from the moment it is
// called, so a test can assert how many round trips a code path costs.
func countStatements(t *testing.T, db *gorm.DB) *[]string {
	t.Helper()
	stmts := &[]string{}
	record := func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.SQL.String() != "" {
			*stmts = append(*stmts, tx.Statement.SQL.String())
		}
	}
	register := []struct {
		name string
		reg  func(string, func(*gorm.DB)) error
	}{
		{"query", db.Callback().Query().After("gorm:query").Register},
		{"create", db.Callback().Create().After("gorm:create").Register},
		{"update", db.Callback().Update().After("gorm:update").Register},
		{"delete", db.Callback().Delete().After("gorm:delete").Register},
		{"raw", db.Callback().Raw().After("gorm:raw").Register},
		{"row", db.Callback().Row().After("gorm:row").Register},
	}
	for _, cb := range register {
		if err := cb.reg("test:count:"+cb.name, record); err != nil {
			t.Fatalf("register %s callback: %v", cb.name, err)
		}
	}
	return stmts
}

// TestQCPass_StaysWithinRoundTripBudget pins the cost of one QC scan.
//
// Every statement in a request is a separate round trip to the database, and the
// database is not local: a QC PASS measured 27 statements against Supabase's
// ap-southeast-1 pooler at ~200ms per round trip, so the operator waited ~6s per
// product with the scanner in hand. Nothing in the logic is slow — the count is
// the cost. The two habits that produced it were re-reading the item at the end
// of the write (a dozen preload queries to return what the caller already had)
// and looping per production part.
//
// So the budget is asserted, not just the outcome: a change that reintroduces a
// per-part loop or a trailing re-read fails here rather than in the workshop.
// Raise the ceiling only with a measurement that says the extra trips are worth
// what they cost.
func TestQCPass_StaysWithinRoundTripBudget(t *testing.T) {
	// 23 today for this two-material combo. The headroom is one statement, not ten:
	// a reintroduced per-part loop costs three or four per material and trips this
	// immediately, which is the whole point.
	const budget = 24

	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}
	actor := Actor{ID: 1, Role: models.RoleOwner}

	// A combo: 2 materials, 2 batches, 2 parts. The per-part work must NOT scale
	// the statement count — that is the regression this guards.
	wood := &models.Material{Code: "WOOD", Name: "Gỗ"}
	mica := &models.Material{Code: "MICA", Name: "Mica"}
	db.Create(wood)
	db.Create(mica)
	sku := &models.SKU{Code: "COMBO-RT", Name: "Đèn combo", IsCombo: true}
	db.Create(sku)
	db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: wood.ID, QuantityPerUnit: 1})
	db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mica.ID, QuantityPerUnit: 1})

	order := &models.Order{
		InternalCode: "ORD-RT-0001", StoreOrderID: "US-RT", StoreOrderRef: "US-RT", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	db.Create(order)
	item := &models.OrderItem{
		OrderID: order.ID, LineNo: 1, InternalCode: "ORD-RT-0001_1", SKUID: &sku.ID, SKUCode: sku.Code,
		Quantity: 1, MockupURL: "https://example.com/mockup.png", InternalStatus: models.StatusCut,
	}
	db.Create(item)
	for _, m := range []*models.Material{wood, mica} {
		b := &models.Batch{Code: "B-RT-" + m.Code, MaterialID: m.ID, Status: models.StatusCut}
		db.Create(b)
		db.Create(&models.BatchItem{BatchID: b.ID, OrderItemID: item.ID, MaterialID: m.ID, Status: models.StatusCut})
	}

	stmts := countStatements(t, db)

	updated, err := qc.Pass(actor, QCDecisionInput{ScanRef: ScanRef{Code: item.InternalCode}})
	if err != nil {
		t.Fatalf("QC Pass failed: %v", err)
	}

	if n := len(*stmts); n > budget {
		t.Errorf("QC Pass issued %d SQL statements, budget is %d:", n, budget)
		for i, s := range *stmts {
			if len(s) > 120 {
				s = s[:120] + "…"
			}
			t.Errorf("%3d. %s", i+1, s)
		}
	} else {
		t.Logf("QC Pass issued %d SQL statements (budget %d)", n, budget)
	}

	// The response is now built in memory rather than re-read, so assert it still
	// tells the truth — a stale answer would be a silent regression, not a slow one.
	if updated.InternalStatus != models.StatusQCPassed {
		t.Fatalf("returned item: want QC_PASSED, got %s", updated.InternalStatus)
	}
	if len(updated.BatchItems) != 2 {
		t.Fatalf("returned item: want 2 parts, got %d", len(updated.BatchItems))
	}
	for _, bi := range updated.BatchItems {
		if bi.Status != models.StatusQCPassed {
			t.Fatalf("returned part %d: want QC_PASSED, got %s", bi.ID, bi.Status)
		}
	}

	// ...and that it matches what was actually persisted.
	var stored models.OrderItem
	if err := db.Preload("BatchItems").First(&stored, item.ID).Error; err != nil {
		t.Fatalf("reload item: %v", err)
	}
	if stored.InternalStatus != models.StatusQCPassed {
		t.Fatalf("stored item: want QC_PASSED, got %s", stored.InternalStatus)
	}
	for _, bi := range stored.BatchItems {
		if bi.Status != models.StatusQCPassed {
			t.Fatalf("stored part %d: want QC_PASSED, got %s", bi.ID, bi.Status)
		}
	}

	// One QC record per part, still — batching the insert must not lose rows.
	var qcRecords int64
	db.Model(&models.QCRecord{}).Where("order_item_id = ?", item.ID).Count(&qcRecords)
	if qcRecords != 2 {
		t.Fatalf("qc records: want 2 (one per part), got %d", qcRecords)
	}
}
