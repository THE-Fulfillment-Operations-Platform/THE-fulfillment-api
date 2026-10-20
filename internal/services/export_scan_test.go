package services

import (
	"context"
	"testing"

	"the-fulfillment/backend/internal/models"
)

// Khách (20/10/2026): quét xuất kho (XK) chỉ để đếm và ghi nhận hàng đã ra khỏi
// kho. Không được vướng điều kiện cân nặng / hộp (cái đó ở chỗ dán label), và quét
// được cả hàng hệ thống còn ghi là đang sản xuất.

func TestExportScan_GoesThroughWhateverTheQCState(t *testing.T) {
	db := newHandoffDB(t)
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))
	// 3 sản phẩm: 1 đã QC, 1 mới cắt, 1 còn chờ — luồng gửi THE sẽ chặn đơn này.
	o := seedOrderForShipping(t, db, "100101", models.StatusQCPassed, models.StatusCut, models.StatusPending)

	if _, err := svc.ShipScannedOrderWith(context.Background(), opsActor(), "100101", ShipSendOptions{}); err == nil {
		t.Fatalf("gửi THE vẫn phải chặn đơn chưa QC đủ")
	}

	res, err := svc.ShipScannedOrderWith(context.Background(), opsActor(), "100101", ShipSendOptions{ExportOnly: true})
	if err != nil {
		t.Fatalf("quét xuất kho phải qua: %v", err)
	}
	if !res.Exported || res.UnfinishedItems != 2 || res.TotalItems != 3 || res.THE != nil {
		t.Fatalf("kết quả XK: %+v (muốn exported, còn 2/3 chưa QC, không tạo gì trên THE)", res)
	}
	var got models.Order
	db.First(&got, o.ID)
	if got.SellerStatus != models.SellerStatusHandedOff || got.HandedOverAt == nil {
		t.Fatalf("đơn phải ghi nhận đã ra khỏi kho: %s", got.SellerStatus)
	}
	var h models.Handoff
	db.Where("order_id = ?", o.ID).First(&h)
	if h.Note != exportNote {
		t.Fatalf("handoff note = %q, want %q", h.Note, exportNote)
	}
	var audits int64
	db.Model(&models.AuditLog{}).Where("action = ?", "ORDER_EXPORT_UNFINISHED").Count(&audits)
	if audits != 1 {
		t.Fatalf("đơn xuất kho khi còn chưa QC phải để lại dấu vết trong nhật ký, got %d", audits)
	}
}

func TestExportScan_StillRefusesWhatCannotLeave(t *testing.T) {
	db := newHandoffDB(t)
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))
	scan := func(code string) error {
		_, err := svc.ShipScannedOrderWith(context.Background(), opsActor(), code, ShipSendOptions{ExportOnly: true})
		return err
	}

	cancelled := seedOrderForShipping(t, db, "100110", models.StatusCut)
	db.Model(&models.Order{}).Where("id = ?", cancelled.ID).Update("cancellation_status", models.CancellationApproved)
	if scan("100110") == nil {
		t.Errorf("đơn đã huỷ không được xuất kho")
	}
	unapproved := seedOrderForShipping(t, db, "100111", models.StatusCut)
	db.Model(&models.Order{}).Where("id = ?", unapproved.ID).Update("review_status", models.ReviewPending)
	if scan("100111") == nil {
		t.Errorf("đơn chưa duyệt không được xuất kho")
	}
	done := seedOrderForShipping(t, db, "100112", models.StatusQCPassed)
	if err := scan("100112"); err != nil {
		t.Fatalf("lần đầu: %v", err)
	}
	if scan("100112") == nil {
		t.Errorf("quét lần hai cùng đơn phải báo đã xuất kho rồi")
	}
	if scan("999999") == nil {
		t.Errorf("mã không có phải báo lỗi")
	}
	_ = done
}

// Hàng đã ra khỏi kho thì giỏ chờ của đơn không còn gì để chờ: tự trả số giỏ.
func TestExportScan_FreesTheBasket(t *testing.T) {
	db := newHandoffDB(t)
	if err := db.AutoMigrate(&models.QCRecord{}, &models.User{}); err != nil {
		t.Fatal(err)
	}
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))
	o := seedOrderForShipping(t, db, "100120", models.StatusQCPassed, models.StatusCut)
	if err := db.Create(&models.Basket{Number: 7, OrderID: o.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ShipScannedOrderWith(context.Background(), opsActor(), "100120", ShipSendOptions{ExportOnly: true}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if b, _ := svc.repo.Basket.LiveByOrder(o.ID); b != nil {
		t.Fatalf("giỏ #%d phải được trả khi đơn đã xuất kho", b.Number)
	}
}
