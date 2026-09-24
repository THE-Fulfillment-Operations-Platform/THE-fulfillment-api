package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// seedAutoItem creates one design-ready item on an approved order for the given
// SKU with an explicit quantity — the custom twin of seedSplit for auto-create
// scenarios that need several materials or quantities.
func seedAutoItem(t *testing.T, db *gorm.DB, orderID uint, sku *models.SKU, code string, qty int) *models.OrderItem {
	t.Helper()
	it := &models.OrderItem{
		OrderID: orderID, InternalCode: code,
		SKUID: &sku.ID, SKUCode: sku.Code, Quantity: qty,
		InternalStatus: models.StatusPending, DesignStatus: models.DesignReady,
	}
	if err := db.Create(it).Error; err != nil {
		t.Fatalf("seed item %s: %v", code, err)
	}
	return it
}

// TestAutoCreateBatches_GroupsByMaterialAndQuota: one click batches the whole
// pool — material A (quota 20, 25 products) becomes 2 flat batches, material B
// (no quota, 5 products) becomes one. Nobody picked a row or typed a batch name.
func TestAutoCreateBatches_GroupsByMaterialAndQuota(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	matA, _ := seedSplit(t, db, 20, 25) // MICA + 25 single-quantity items

	matB := &models.Material{Code: "WOOD", Name: "Wood"}
	if err := db.Create(matB).Error; err != nil {
		t.Fatalf("seed material B: %v", err)
	}
	skuB := &models.SKU{Code: "WOOD-01", Name: "Wood Plate"}
	if err := db.Create(skuB).Error; err != nil {
		t.Fatalf("seed sku B: %v", err)
	}
	if err := db.Create(&models.SKUMaterial{SKUID: skuB.ID, MaterialID: matB.ID, QuantityPerUnit: 1}).Error; err != nil {
		t.Fatalf("seed sku-material B: %v", err)
	}
	orderB := &models.Order{InternalCode: "100002", StoreOrderID: "Etsy-2", SellerID: 1, ReviewStatus: models.ReviewApproved}
	if err := db.Create(orderB).Error; err != nil {
		t.Fatalf("seed order B: %v", err)
	}
	for i := 0; i < 5; i++ {
		seedAutoItem(t, db, orderB.ID, skuB, fmt.Sprintf("100002_%d", i+1), 1)
	}

	res, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("auto create: %v", err)
	}
	if len(res.Created) != 2 {
		t.Fatalf("want batches for 2 materials, got %d (%+v)", len(res.Created), res)
	}
	byMat := map[uint]AutoCreatedBatch{}
	for _, c := range res.Created {
		byMat[c.MaterialID] = c
	}
	a, ok := byMat[matA.ID]
	if !ok {
		t.Fatalf("material A missing from result: %+v", res.Created)
	}
	if len(a.BatchCodes) != 2 {
		t.Fatalf("material A: want 2 flat batches, got %+v", a)
	}
	if a.ItemCount != 25 {
		t.Fatalf("material A item count: got %d, want 25", a.ItemCount)
	}
	b, ok := byMat[matB.ID]
	if !ok {
		t.Fatalf("material B missing from result: %+v", res.Created)
	}
	if len(b.BatchCodes) != 1 || b.ItemCount != 5 {
		t.Fatalf("material B: want one flat batch of 5, got %+v", b)
	}
	// Codes are system-generated and unique across everything created.
	seen := map[string]bool{}
	for _, c := range res.Created {
		codes := append([]string{c.BatchCode}, c.BatchCodes...)
		for _, code := range codes {
			if code == "" {
				t.Fatalf("empty generated code in %+v", c)
			}
			if seen[code] && code != c.BatchCode {
				t.Fatalf("duplicate generated code %q", code)
			}
			seen[code] = true
		}
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("nothing should be skipped, got %+v", res.Skipped)
	}
}

// TestAutoCreateBatches_CoversWholePool: 30 items (above the default 20-row
// page) all land in the batch — auto-create must read the whole pool.
func TestAutoCreateBatches_CoversWholePool(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	seedSplit(t, db, 0, 30)

	res, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("auto create: %v", err)
	}
	if len(res.Created) != 1 || res.Created[0].ItemCount != 30 {
		t.Fatalf("want one batch holding all 30 items, got %+v", res.Created)
	}
	var linked int64
	db.Model(&models.BatchItem{}).Count(&linked)
	if linked != 30 {
		t.Fatalf("want 30 batch_items, got %d", linked)
	}
}

// TestAutoCreateBatches_ComboSplitsPerMaterialWithoutMixing: a combo item whose
// SKU uses two materials must appear in BOTH materials' batches (one part each),
// and no batch may hold a part of a foreign material.
func TestAutoCreateBatches_ComboSplitsPerMaterialWithoutMixing(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	matA, _ := seedSplit(t, db, 0, 1)
	matB := &models.Material{Code: "WOOD", Name: "Wood"}
	if err := db.Create(matB).Error; err != nil {
		t.Fatalf("seed material B: %v", err)
	}
	comboSKU := &models.SKU{Code: "COMBO-01", Name: "Combo"}
	if err := db.Create(comboSKU).Error; err != nil {
		t.Fatalf("seed combo sku: %v", err)
	}
	for _, m := range []uint{matA.ID, matB.ID} {
		if err := db.Create(&models.SKUMaterial{SKUID: comboSKU.ID, MaterialID: m, QuantityPerUnit: 1}).Error; err != nil {
			t.Fatalf("seed combo sku-material: %v", err)
		}
	}
	order := &models.Order{InternalCode: "100020", StoreOrderID: "C-1", SellerID: 1, ReviewStatus: models.ReviewApproved}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	combo := seedAutoItem(t, db, order.ID, comboSKU, "100020_1", 1)

	res, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("auto create: %v", err)
	}
	if len(res.Created) != 2 {
		t.Fatalf("want batches for both materials, got %+v", res.Created)
	}
	// The combo item has exactly one live part per material — never lost, never doubled.
	var parts []models.BatchItem
	if err := db.Where("order_item_id = ?", combo.ID).Find(&parts).Error; err != nil {
		t.Fatalf("load combo parts: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("combo item: want 2 parts (one per material), got %d", len(parts))
	}
	mats := map[uint]bool{}
	for _, p := range parts {
		mats[p.MaterialID] = true
	}
	if !mats[matA.ID] || !mats[matB.ID] {
		t.Fatalf("combo parts landed on wrong materials: %+v", parts)
	}
	// No batch mixes materials: every part's material matches its batch's material.
	var all []models.BatchItem
	if err := db.Find(&all).Error; err != nil {
		t.Fatalf("load parts: %v", err)
	}
	for _, p := range all {
		var b models.Batch
		if err := db.First(&b, p.BatchID).Error; err != nil {
			t.Fatalf("load batch: %v", err)
		}
		if b.MaterialID != p.MaterialID {
			t.Fatalf("batch %s (material %d) holds a part of material %d", b.Code, b.MaterialID, p.MaterialID)
		}
	}
}

// TestAutoCreateBatches_ReworkIncludedAndSecondRunIsNoOp: a part scrapped at QC
// puts its product back into the pool, so the next auto-create picks it up
// (attempt 2). Running auto-create again with an empty pool creates nothing —
// products already batched are never batched twice.
func TestAutoCreateBatches_ReworkIncludedAndSecondRunIsNoOp(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 2)

	first, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(first.Created) != 1 || first.Created[0].ItemCount != 2 {
		t.Fatalf("first run: want one batch of 2, got %+v", first.Created)
	}

	// QC scraps the first product's part → it is design-ready again for rework.
	now := time.Now()
	if err := db.Model(&models.BatchItem{}).Where("order_item_id = ?", ids[0]).
		Updates(map[string]any{"scrapped_at": now, "scrap_reason": "vỡ"}).Error; err != nil {
		t.Fatalf("scrap part: %v", err)
	}

	second, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(second.Created) != 1 || second.Created[0].ItemCount != 1 {
		t.Fatalf("second run: want one rework batch of 1, got %+v", second.Created)
	}
	var attempts []models.BatchItem
	if err := db.Where("order_item_id = ? AND scrapped_at IS NULL", ids[0]).Find(&attempts).Error; err != nil {
		t.Fatalf("load rework part: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Attempt != 2 {
		t.Fatalf("rework part: want one live part at attempt 2, got %+v", attempts)
	}
	if int64(1) != func() int64 {
		var n int64
		db.Model(&models.BatchItem{}).Where("order_item_id = ? AND scrapped_at IS NULL", ids[1]).Count(&n)
		return n
	}() {
		t.Fatalf("untouched product must keep exactly one live part")
	}

	// Third run: pool is empty → nothing new, no duplicates.
	third, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if len(third.Created) != 0 {
		t.Fatalf("empty pool must create nothing, got %+v", third.Created)
	}
	var batches int64
	db.Model(&models.Batch{}).Count(&batches)
	if batches != 2 {
		t.Fatalf("want the 2 batches from runs 1-2 only, got %d", batches)
	}
}

// TestAutoCreateBatches_SkipsIneligiblePool: only approved, design-ready,
// not-cancelled, not-yet-batched items are picked up.
func TestAutoCreateBatches_SkipsIneligiblePool(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 1)
	sku := &models.SKU{}
	if err := db.First(sku, "code = ?", "MICA-01").Error; err != nil {
		t.Fatalf("load sku: %v", err)
	}

	pending := &models.Order{InternalCode: "100010", StoreOrderID: "P-1", SellerID: 1, ReviewStatus: models.ReviewPending}
	if err := db.Create(pending).Error; err != nil {
		t.Fatalf("seed pending order: %v", err)
	}
	seedAutoItem(t, db, pending.ID, sku, "100010_1", 1) // order not approved

	approved := &models.Order{InternalCode: "100011", StoreOrderID: "A-1", SellerID: 1, ReviewStatus: models.ReviewApproved}
	if err := db.Create(approved).Error; err != nil {
		t.Fatalf("seed approved order: %v", err)
	}
	cancelled := seedAutoItem(t, db, approved.ID, sku, "100011_1", 1)
	if err := db.Model(cancelled).Update("cancellation_status", models.CancellationApproved).Error; err != nil {
		t.Fatalf("cancel item: %v", err)
	}
	notReady := seedAutoItem(t, db, approved.ID, sku, "100011_2", 1)
	if err := db.Model(notReady).Update("design_status", models.DesignPending).Error; err != nil {
		t.Fatalf("unready item: %v", err)
	}

	res, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("auto create: %v", err)
	}
	if len(res.Created) != 1 || res.Created[0].ItemCount != 1 {
		t.Fatalf("want one batch holding only the eligible item, got %+v", res.Created)
	}
	var part models.BatchItem
	if err := db.First(&part, "order_item_id = ?", ids[0]).Error; err != nil {
		t.Fatalf("eligible item was not batched: %v", err)
	}
	var wrong int64
	db.Model(&models.BatchItem{}).Where("order_item_id <> ?", ids[0]).Count(&wrong)
	if wrong != 0 {
		t.Fatalf("ineligible items must not be batched, found %d parts", wrong)
	}
}

// TestAutoCreateBatches_FailedMaterialLeavesNoHalfBatch: when creating one
// material's batch blows up mid-transaction, that material rolls back to
// nothing (no header without items, no dangling parts) while the other
// material's batch still lands — and the failure is reported, not swallowed.
func TestAutoCreateBatches_FailedMaterialLeavesNoHalfBatch(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	matA, _ := seedSplit(t, db, 0, 2)
	matB := &models.Material{Code: "WOOD", Name: "Wood"}
	if err := db.Create(matB).Error; err != nil {
		t.Fatalf("seed material B: %v", err)
	}
	skuB := &models.SKU{Code: "WOOD-01", Name: "Wood Plate"}
	if err := db.Create(skuB).Error; err != nil {
		t.Fatalf("seed sku B: %v", err)
	}
	if err := db.Create(&models.SKUMaterial{SKUID: skuB.ID, MaterialID: matB.ID, QuantityPerUnit: 1}).Error; err != nil {
		t.Fatalf("seed sku-material B: %v", err)
	}
	orderB := &models.Order{InternalCode: "100002", StoreOrderID: "Etsy-2", SellerID: 1, ReviewStatus: models.ReviewApproved}
	if err := db.Create(orderB).Error; err != nil {
		t.Fatalf("seed order B: %v", err)
	}
	seedAutoItem(t, db, orderB.ID, skuB, "100002_1", 1)

	// Sabotage material B: every batch_items insert for it aborts.
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_mat_b BEFORE INSERT ON batch_items
		WHEN NEW.material_id = %d BEGIN SELECT RAISE(ABORT, 'material B blocked'); END`, matB.ID)).Error; err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	res, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("auto create should degrade per material, not fail whole: %v", err)
	}
	if len(res.Created) != 1 || res.Created[0].MaterialID != matA.ID {
		t.Fatalf("material A must still be batched, got %+v", res.Created)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].MaterialCode != "WOOD" || res.Skipped[0].Reason == "" {
		t.Fatalf("material B's failure must be reported, got %+v", res.Skipped)
	}
	var bBatches int64
	db.Model(&models.Batch{}).Where("material_id = ?", matB.ID).Count(&bBatches)
	if bBatches != 0 {
		t.Fatalf("failed material must leave no half-created batch, found %d", bBatches)
	}
}

// TestAutoCreateBatches_EmptyPool_MarshalsArrays: an empty pool is a valid,
// empty result whose lists marshal as [] — never null.
func TestAutoCreateBatches_EmptyPool_MarshalsArrays(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)

	res, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("auto create on empty pool: %v", err)
	}
	if len(res.Created) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("want empty result, got %+v", res)
	}
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"created", "skipped"} {
		if bytes.Contains(blob, []byte(`"`+field+`":null`)) {
			t.Fatalf("%s marshalled as null — the client reads it as an array: %s", field, blob)
		}
	}
}
