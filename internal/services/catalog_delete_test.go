package services

import (
	"strconv"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// TestDeleteMaterials_SkipsMaterialsStillInUse covers the guard: a material that a
// SKU maps to, or that a batch was cut from, must survive a bulk delete and come
// back with a reason. Soft-deleting it would leave those rows pointing at a
// material that no longer lists anywhere.
func TestDeleteMaterials_SkipsMaterialsStillInUse(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	free := &models.Material{Code: "FREE", Name: "Không ai dùng"}
	usedBySKU := &models.Material{Code: "BY-SKU", Name: "SKU đang dùng"}
	usedByBatch := &models.Material{Code: "BY-BATCH", Name: "Batch đang dùng"}
	for _, m := range []*models.Material{free, usedBySKU, usedByBatch} {
		if err := db.Create(m).Error; err != nil {
			t.Fatalf("seed material: %v", err)
		}
	}
	sku := &models.SKU{Code: "SKU-1", Name: "SKU"}
	if err := db.Create(sku).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}
	if err := db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: usedBySKU.ID, QuantityPerUnit: 1}).Error; err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	if err := db.Create(&models.Batch{Code: "#100001", MaterialID: usedByBatch.ID, Status: models.StatusPending}).Error; err != nil {
		t.Fatalf("seed batch: %v", err)
	}

	const missingID = 99999
	res, err := svc.DeleteMaterials(owner, []uint{free.ID, usedBySKU.ID, usedByBatch.ID, missingID, free.ID})
	if err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if len(res.DeletedIDs) != 1 || res.DeletedIDs[0] != free.ID {
		t.Fatalf("deleted = %v, want only the unused material (dedup included)", res.DeletedIDs)
	}
	reasons := map[uint]string{}
	for _, s := range res.Skipped {
		reasons[s.ID] = s.Reason
	}
	if len(res.Skipped) != 3 {
		t.Fatalf("skipped = %+v, want the two in-use materials and the missing id", res.Skipped)
	}
	if reasons[usedBySKU.ID] == "" || reasons[usedByBatch.ID] == "" || reasons[missingID] == "" {
		t.Fatalf("every skip needs a reason, got %+v", res.Skipped)
	}

	repo := repositories.New(db)
	left, err := repo.Material.ListByIDs([]uint{free.ID, usedBySKU.ID, usedByBatch.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 2 {
		t.Fatalf("catalog has %d materials left, want the 2 in-use ones", len(left))
	}

	// The single-material endpoint rides on the same guard and must report why.
	if err := svc.DeleteMaterial(owner, usedBySKU.ID); err == nil {
		t.Fatalf("deleting a material a SKU uses must fail with a reason")
	}
}

// TestDeleteSKUs_SkipsSKUsWithOrders is the same guard for SKUs: an order line
// points at its SKU by id, so a SKU that has been ordered must survive the delete
// and say why. Its material mappings go with the ones that do get deleted.
func TestDeleteSKUs_SkipsSKUsWithOrders(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	mat := &models.Material{Code: "MAT", Name: "NVL"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	free := &models.SKU{Code: "FREE-SKU", Name: "Chưa ai đặt"}
	ordered := &models.SKU{Code: "ORDERED-SKU", Name: "Đã có đơn"}
	for _, s := range []*models.SKU{free, ordered} {
		if err := db.Create(s).Error; err != nil {
			t.Fatalf("seed sku: %v", err)
		}
		if err := db.Create(&models.SKUMaterial{SKUID: s.ID, MaterialID: mat.ID, QuantityPerUnit: 1}).Error; err != nil {
			t.Fatalf("seed mapping: %v", err)
		}
	}
	order := &models.Order{InternalCode: "ORD-1", StoreOrderID: "SO-1", SellerID: 1}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if err := db.Create(&models.OrderItem{
		OrderID: order.ID, InternalCode: "IT-1", SKUID: &ordered.ID, SKUCode: ordered.Code, Quantity: 1,
	}).Error; err != nil {
		t.Fatalf("seed order item: %v", err)
	}

	res, err := svc.DeleteSKUs(owner, []uint{free.ID, ordered.ID})
	if err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if len(res.DeletedIDs) != 1 || res.DeletedIDs[0] != free.ID {
		t.Fatalf("deleted = %v, want only the un-ordered SKU", res.DeletedIDs)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].ID != ordered.ID || res.Skipped[0].Reason == "" {
		t.Fatalf("skipped = %+v, want the ordered SKU with a reason", res.Skipped)
	}

	// The deleted SKU's mapping rows go with it; the surviving one keeps its own.
	var mappings []models.SKUMaterial
	if err := db.Find(&mappings).Error; err != nil {
		t.Fatalf("list mappings: %v", err)
	}
	if len(mappings) != 1 || mappings[0].SKUID != ordered.ID {
		t.Fatalf("mappings = %+v, want only the surviving SKU's", mappings)
	}
}

// TestSetSKUsActive_IsOneStatement covers the hide/show bulk action: flipping a
// selection used to be one full SKU save per row.
func TestSetSKUsActive_IsOneStatement(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	ids := make([]uint, 0, 50)
	for i := 0; i < 50; i++ {
		s := &models.SKU{Code: "S" + strconv.Itoa(i), Name: "SKU", IsActive: true}
		if err := db.Create(s).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
		ids = append(ids, s.ID)
	}

	stmts := 0
	count := func(*gorm.DB) { stmts++ }
	for _, reg := range []func(string, func(*gorm.DB)) error{
		db.Callback().Query().After("gorm:query").Register,
		db.Callback().Create().After("gorm:create").Register,
		db.Callback().Update().After("gorm:update").Register,
		db.Callback().Raw().After("gorm:raw").Register,
	} {
		if err := reg("test:count", count); err != nil {
			t.Fatalf("register callback: %v", err)
		}
	}

	n, err := svc.SetSKUsActive(owner, ids, false)
	if err != nil {
		t.Fatalf("bulk set active: %v", err)
	}
	if n != int64(len(ids)) {
		t.Fatalf("updated %d, want %d", n, len(ids))
	}
	if stmts > 2 { // one UPDATE + one audit insert
		t.Fatalf("hiding %d SKUs issued %d statements, want one batched UPDATE", len(ids), stmts)
	}
	var active int64
	db.Model(&models.SKU{}).Where("is_active = ?", true).Count(&active)
	if active != 0 {
		t.Fatalf("%d SKUs still active, want all hidden", active)
	}
}

// TestDeleteMaterials_IsBatched is the round-trip guard. Deleting a selection one
// id at a time cost an HTTP request plus three statements per material — a few
// hundred rows meant minutes. The whole set must cost a handful of statements.
func TestDeleteMaterials_IsBatched(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	const n = 300
	ids := make([]uint, 0, n)
	for i := 0; i < n; i++ {
		m := &models.Material{Code: "M" + strconv.Itoa(i), Name: "NVL " + strconv.Itoa(i)}
		if err := db.Create(m).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
		ids = append(ids, m.ID)
	}

	stmts := 0
	count := func(*gorm.DB) { stmts++ }
	for _, reg := range []func(string, func(*gorm.DB)) error{
		db.Callback().Query().After("gorm:query").Register,
		db.Callback().Create().After("gorm:create").Register,
		db.Callback().Update().After("gorm:update").Register,
		db.Callback().Delete().After("gorm:delete").Register,
		db.Callback().Row().After("gorm:row").Register,
		db.Callback().Raw().After("gorm:raw").Register,
	} {
		if err := reg("test:count", count); err != nil {
			t.Fatalf("register callback: %v", err)
		}
	}

	res, err := svc.DeleteMaterials(owner, ids)
	if err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if len(res.DeletedIDs) != n {
		t.Fatalf("deleted %d, want %d", len(res.DeletedIDs), n)
	}
	// 1 existence read + 3 usage reads + 1 delete + 1 audit insert (chunked at 500).
	if stmts > 8 {
		t.Fatalf("deleting %d materials issued %d statements, want a handful of batched ones", n, stmts)
	}
}
