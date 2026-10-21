package services

import (
	"errors"
	"testing"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// Đơn test của kỹ thuật viên thường đã chạy hết luồng (batch, QC, bàn giao) nên
// "đã vào sản xuất" không được chặn chính người đang dọn đơn test. Admin / OPS vẫn
// bị chặn; OWNER xoá từng đơn được, xoá hàng loạt thì vẫn không.

// Every table an order can leave rows in — what a purge has to reach.
var purgeModels = []any{
	&models.Batch{}, &models.BatchItem{}, &models.BatchLink{}, &models.CarrierShipment{}, &models.QCRecord{},
	&models.ItemAsset{}, &models.Package{}, &models.PackageItem{}, &models.Handoff{}, &models.Note{},
	&models.StatusHistory{}, &models.OrderTrackingEvent{}, &models.Basket{}, &models.User{},
}

func TestDeleteInProduction_TechnicianMayAdminMayNot(t *testing.T) {
	db := newBulkReadyDB(t)
	if err := db.AutoMigrate(purgeModels...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := newOrderService(db)
	mk := func(code string) *models.OrderItem {
		it := seedReadyCandidate(t, db, code, models.DesignReady, "https://x/m.png", true)
		if err := db.Model(it).Update("internal_status", models.StatusPrinted).Error; err != nil {
			t.Fatal(err)
		}
		return it
	}
	conflict := func(err error) bool {
		var ae *apperr.Error
		return errors.As(err, &ae) && ae.Status == 409
	}

	// Xoá từng đơn từ chi tiết.
	a, b, c := mk("A"), mk("B"), mk("C")
	if err := svc.DeleteOrder(Actor{ID: 2, Role: models.RoleAdmin}, a.OrderID); !conflict(err) {
		t.Fatalf("admin xoá đơn đang sản xuất phải bị chặn (409), got %v", err)
	}
	if err := svc.DeleteOrder(Actor{ID: 3, Role: models.RoleTechnician}, b.OrderID); err != nil {
		t.Fatalf("kỹ thuật viên phải xoá được đơn test đang sản xuất: %v", err)
	}
	if err := svc.DeleteOrder(Actor{ID: 1, Role: models.RoleOwner}, c.OrderID); err != nil {
		t.Fatalf("owner vẫn xoá được: %v", err)
	}

	// Tick-and-delete: chỉ kỹ thuật viên xoá được đơn đang sản xuất.
	d, e, f := mk("D"), mk("E"), mk("F")
	res, err := svc.DeleteOrders(Actor{ID: 2, Role: models.RoleAdmin}, []uint{d.OrderID})
	if err != nil || len(res.DeletedIDs) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("admin hàng loạt phải bỏ qua đơn đang sản xuất: %+v err=%v", res, err)
	}
	res, err = svc.DeleteOrders(Actor{ID: 1, Role: models.RoleOwner}, []uint{e.OrderID})
	if err != nil || len(res.DeletedIDs) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("owner hàng loạt vẫn bỏ qua đơn đang sản xuất: %+v err=%v", res, err)
	}
	res, err = svc.DeleteOrders(Actor{ID: 3, Role: models.RoleTechnician}, []uint{d.OrderID, e.OrderID, f.OrderID})
	if err != nil || len(res.DeletedIDs) != 3 || len(res.Skipped) != 0 {
		t.Fatalf("kỹ thuật viên hàng loạt phải xoá được cả 3: %+v err=%v", res, err)
	}
}

// Đơn đã chốt trên THE (label, đã trừ tiền) thì không xoá được dù là ai: xoá đơn là
// kiện vẫn sống trên THE và tiền đã mất. Phải huỷ đơn THE trước.
func TestDeleteOrder_RefusesWhilePaidTHEShipmentIsLive(t *testing.T) {
	db := newBulkReadyDB(t)
	if err := db.AutoMigrate(purgeModels...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := newOrderService(db)
	it := seedReadyCandidate(t, db, "PAID", models.DesignReady, "https://x/m.png", true)
	if err := db.Model(it).Update("internal_status", models.StatusQCPassed).Error; err != nil {
		t.Fatal(err)
	}
	sh := &models.CarrierShipment{OrderID: it.OrderID, Provider: models.CarrierProviderTHE, Status: models.ShipmentLabeled}
	if err := db.Create(sh).Error; err != nil {
		t.Fatalf("seed shipment: %v", err)
	}
	tech := Actor{ID: 3, Role: models.RoleTechnician}

	if err := svc.DeleteOrder(tech, it.OrderID); err == nil {
		t.Fatalf("đơn đã chốt THE không được xoá")
	}
	res, err := svc.DeleteOrders(tech, []uint{it.OrderID})
	if err != nil || len(res.DeletedIDs) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("hàng loạt phải bỏ qua đơn đã chốt THE: %+v err=%v", res, err)
	}

	// Huỷ đơn THE xong thì xoá được.
	db.Model(sh).Update("status", models.ShipmentCancelled)
	if err := svc.DeleteOrder(tech, it.OrderID); err != nil {
		t.Fatalf("sau khi huỷ đơn THE phải xoá được: %v", err)
	}
}

// Đơn test xoá là XOÁ HẲN: không còn dòng nào (kể cả dòng đã soft-delete) của đơn,
// sản phẩm, batch part, QC, ghi chú, lịch sử, bàn giao, giỏ chờ. Batch chỉ chứa đơn
// test bị xoá luôn; batch còn chứa đơn khác thì giữ và vẫn còn phần của đơn kia.
func TestTechnicianDelete_PurgesEverything(t *testing.T) {
	db := newBulkReadyDB(t)
	if err := db.AutoMigrate(purgeModels...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := newOrderService(db)
	tech := Actor{ID: 3, Role: models.RoleTechnician}

	test := seedReadyCandidate(t, db, "TESTORD", models.DesignReady, "https://x/m.png", true)
	real := seedReadyCandidate(t, db, "REALORD", models.DesignReady, "https://x/m.png", true)
	db.Model(test).Update("internal_status", models.StatusQCPassed)

	onlyTest := &models.Batch{Code: "B-ONLY", MaterialID: 1, Status: models.StatusQCPassed}
	shared := &models.Batch{Code: "B-SHARED", MaterialID: 1, Status: models.StatusCut}
	db.Create(onlyTest)
	db.Create(shared)
	db.Create(&models.BatchLink{BatchID: onlyTest.ID, Kind: "PRINT", URL: "https://x/p"})
	bi1 := &models.BatchItem{BatchID: onlyTest.ID, OrderItemID: test.ID, MaterialID: 1, Status: models.StatusQCPassed}
	bi2 := &models.BatchItem{BatchID: shared.ID, OrderItemID: test.ID, MaterialID: 2, Status: models.StatusQCPassed}
	bi3 := &models.BatchItem{BatchID: shared.ID, OrderItemID: real.ID, MaterialID: 1, Status: models.StatusCut}
	db.Create(bi1)
	db.Create(bi2)
	db.Create(bi3)
	db.Create(&models.QCRecord{OrderItemID: test.ID, Result: models.QCPass})
	db.Create(&models.StatusHistory{EntityType: models.EntityOrderItem, EntityID: test.ID, ToStatus: "QC_PASSED"})
	db.Create(&models.StatusHistory{EntityType: models.EntityBatchItem, EntityID: bi1.ID, ToStatus: "QC_PASSED"})
	eid := test.ID
	db.Create(&models.Note{Title: "n", EntityType: models.EntityOrderItem, EntityID: &eid})
	db.Create(&models.Handoff{Code: "H1", OrderID: &test.OrderID})
	db.Create(&models.Basket{Number: 3, OrderID: test.OrderID})

	if err := svc.DeleteOrder(tech, test.OrderID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	count := func(model any, cond string, args ...any) int64 {
		var n int64
		db.Unscoped().Model(model).Where(cond, args...).Count(&n)
		return n
	}
	gone := map[string]int64{
		"orders":         count(&models.Order{}, "id = ?", test.OrderID),
		"order_items":    count(&models.OrderItem{}, "order_id = ?", test.OrderID),
		"batch_items":    count(&models.BatchItem{}, "order_item_id = ?", test.ID),
		"qc_records":     count(&models.QCRecord{}, "order_item_id = ?", test.ID),
		"notes":          count(&models.Note{}, "entity_id = ?", test.ID),
		"status_history": count(&models.StatusHistory{}, "entity_id IN ?", []uint{test.ID, bi1.ID}),
		"handoffs":       count(&models.Handoff{}, "order_id = ?", test.OrderID),
		"baskets":        count(&models.Basket{}, "order_id = ?", test.OrderID),
		"batch B-ONLY":   count(&models.Batch{}, "id = ?", onlyTest.ID),
		"its links":      count(&models.BatchLink{}, "batch_id = ?", onlyTest.ID),
	}
	for name, n := range gone {
		if n != 0 {
			t.Errorf("%s: còn %d dòng sau khi xoá hẳn đơn test", name, n)
		}
	}
	if count(&models.Order{}, "id = ?", real.OrderID) != 1 || count(&models.BatchItem{}, "id = ?", bi3.ID) != 1 {
		t.Fatalf("đơn thật và phần của nó trong batch chung phải còn nguyên")
	}
	if count(&models.Batch{}, "id = ?", shared.ID) != 1 {
		t.Fatalf("batch còn phần của đơn khác phải được giữ")
	}
	var after models.Batch
	db.First(&after, shared.ID)
	if after.Status != models.StatusCut {
		t.Fatalf("batch chung phải tính lại trạng thái theo phần còn lại (CUT), got %s", after.Status)
	}

	// Owner xoá vẫn là xoá mềm (có thể khôi phục).
	owner := Actor{ID: 1, Role: models.RoleOwner}
	other := seedReadyCandidate(t, db, "OWNERDEL", models.DesignPending, "", false)
	if err := svc.DeleteOrder(owner, other.OrderID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if n := count(&models.Order{}, "id = ?", other.OrderID); n != 1 {
		t.Fatalf("owner xoá phải là xoá mềm (còn dòng đã đánh dấu xoá), got %d", n)
	}
}
