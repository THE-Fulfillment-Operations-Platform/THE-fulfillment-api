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

func TestDeleteInProduction_TechnicianMayAdminMayNot(t *testing.T) {
	db := newBulkReadyDB(t)
	if err := db.AutoMigrate(&models.Batch{}, &models.BatchItem{}, &models.CarrierShipment{}); err != nil {
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
	if err := db.AutoMigrate(&models.Batch{}, &models.BatchItem{}, &models.CarrierShipment{}); err != nil {
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
