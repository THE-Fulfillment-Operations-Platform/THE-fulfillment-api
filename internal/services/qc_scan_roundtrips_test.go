package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// TestQCScan_StaysWithinRoundTripBudget pins the cost of the OTHER half of the QC
// loop. Its sibling (TestQCPass_StaysWithinRoundTripBudget) guards the write; this
// guards the read, and the read is the one the operator actually waits on — PASS
// is fired in the background by the station, a scan is not.
//
// The cost here is entirely association preloads: resolving an item loads its
// order, seller, SKU materials, batch parts and assets so the station can show
// mockup vs product without a second call. That is the right trade against a
// same-region database (a handful of milliseconds each) and a bad one against a
// remote one, which is why the number is pinned rather than left to drift.
func TestQCScan_StaysWithinRoundTripBudget(t *testing.T) {
	// One statement of headroom, same as the PASS budget: enough to absorb a
	// harmless reorder, not enough to hide a new preload chain.
	const budget = 11

	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}
	actor := Actor{ID: 1, Role: models.RoleQC}

	wood := &models.Material{Code: "WOOD-SC", Name: "Gỗ"}
	mica := &models.Material{Code: "MICA-SC", Name: "Mica"}
	db.Create(wood)
	db.Create(mica)
	sku := &models.SKU{Code: "COMBO-SC", Name: "Đèn combo", IsCombo: true}
	db.Create(sku)
	db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: wood.ID, QuantityPerUnit: 1})
	db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mica.ID, QuantityPerUnit: 1})

	order := &models.Order{
		InternalCode: "ORD-SC-0001", StoreOrderID: "US-SC", StoreOrderRef: "US-SC", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	db.Create(order)
	item := &models.OrderItem{
		OrderID: order.ID, LineNo: 1, InternalCode: "ORD-SC-0001_1", SKUID: &sku.ID, SKUCode: sku.Code,
		Quantity: 1, MockupURL: "https://example.com/mockup.png", InternalStatus: models.StatusCut,
	}
	db.Create(item)
	// Two materials, two batches: like the PASS budget, the count must not scale
	// with the number of production parts.
	for _, m := range []*models.Material{wood, mica} {
		b := &models.Batch{Code: "B-SC-" + m.Code, MaterialID: m.ID, Status: models.StatusCut}
		db.Create(b)
		db.Create(&models.BatchItem{BatchID: b.ID, OrderItemID: item.ID, MaterialID: m.ID, Status: models.StatusCut})
	}

	stmts := countStatements(t, db)

	res, err := qc.Scan(actor, ScanRef{Code: item.InternalCode})
	if err != nil {
		t.Fatalf("QC Scan failed: %v", err)
	}

	if n := len(*stmts); n > budget {
		t.Errorf("QC Scan issued %d SQL statements, budget is %d:", n, budget)
		for i, s := range *stmts {
			if len(s) > 120 {
				s = s[:120] + "…"
			}
			t.Errorf("%3d. %s", i+1, s)
		}
	} else {
		t.Logf("QC Scan issued %d SQL statements (budget %d)", n, budget)
	}

	if res.ItemCode != item.InternalCode {
		t.Fatalf("scan answered for %q, want %q", res.ItemCode, item.InternalCode)
	}
	if len(res.Batches) != 2 {
		t.Fatalf("scan: want 2 production parts, got %d", len(res.Batches))
	}
}
