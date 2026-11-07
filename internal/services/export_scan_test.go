package services

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// Luật 07/11/2026 (Nghĩa chốt, anh Kiên báo đơn chưa QC vẫn quét xuất kho được):
// XK chặn đơn còn sản phẩm sống chưa QC đạt; chỉ Owner / Admin vượt được, kèm lý
// do, và lần vượt để lại dấu vết. (Luật 20/10 "cảnh báo, không chặn" đã bỏ.)

func exportOpts(force bool, reason string) ShipSendOptions {
	return ShipSendOptions{ExportOnly: true, ForceUnQC: force, ForceReason: reason}
}

func TestExportScan_BlocksUnQCOrder(t *testing.T) {
	db := newHandoffDB(t)
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))
	o := seedOrderForShipping(t, db, "100101", models.StatusQCPassed, models.StatusCut, models.StatusPending)

	_, err := svc.ShipScannedOrderWith(context.Background(), opsActor(), "100101", exportOpts(false, ""))
	ae, ok := apperr.As(err)
	if !ok || ae.Status != http.StatusConflict || !strings.Contains(ae.Message, "Đơn 100101 còn 2/3 sản phẩm chưa QC đạt") {
		t.Fatalf("muốn 409 'còn 2/3 sản phẩm chưa QC đạt', got %v", err)
	}
	var got models.Order
	db.First(&got, o.ID)
	if got.SellerStatus.HandedOver() {
		t.Fatalf("đơn bị chặn không được ghi đã xuất kho")
	}
	// Cờ vượt từ vai trò vận hành: 403, vẫn không ghi gì.
	_, err = svc.ShipScannedOrderWith(context.Background(), opsActor(), "100101", exportOpts(true, "xe đang đợi"))
	if ae, ok := apperr.As(err); !ok || ae.Status != http.StatusForbidden {
		t.Fatalf("OPS gửi cờ vượt phải 403, got %v", err)
	}
	db.First(&got, o.ID)
	if got.SellerStatus.HandedOver() {
		t.Fatalf("OPS vượt bị 403 mà đơn vẫn xuất kho")
	}
	// Item đã huỷ không tính; đủ QC thì qua không cần cờ.
	done := seedOrderForShipping(t, db, "100102", models.StatusQCPassed, models.StatusQCPassed)
	if _, err := svc.ShipScannedOrderWith(context.Background(), opsActor(), "100102", exportOpts(false, "")); err != nil {
		t.Fatalf("đơn đủ QC phải qua: %v", err)
	}
	_ = done
}

func TestExportScan_OwnerAdminCanForce(t *testing.T) {
	db := newHandoffDB(t)
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))
	for i, role := range []models.Role{models.RoleOwner, models.RoleAdmin} {
		code := fmt.Sprintf("10013%d", i)
		o := seedOrderForShipping(t, db, code, models.StatusQCPassed, models.StatusCut, models.StatusPending)
		actor := Actor{ID: 1, Email: "boss@t", Role: role}
		if _, err := svc.ShipScannedOrderWith(context.Background(), actor, code, exportOpts(true, "  ")); err == nil {
			t.Fatalf("%s: vượt mà không có lý do phải bị từ chối", role)
		}
		res, err := svc.ShipScannedOrderWith(context.Background(), actor, code, exportOpts(true, "xe đang đợi"))
		if err != nil {
			t.Fatalf("%s vượt phải qua: %v", role, err)
		}
		if !res.Exported || res.UnfinishedItems != 2 || res.TotalItems != 3 || res.THE != nil {
			t.Fatalf("kết quả XK: %+v", res)
		}
		var got models.Order
		db.First(&got, o.ID)
		if got.SellerStatus != models.SellerStatusHandedOff || got.HandedOverAt == nil {
			t.Fatalf("đơn phải ghi nhận đã ra khỏi kho: %s", got.SellerStatus)
		}
		var h models.Handoff
		db.Where("order_id = ?", o.ID).First(&h)
		if h.Note != exportNote {
			t.Fatalf("handoff note = %q", h.Note)
		}
		var log models.AuditLog
		if err := db.Where("action = ? AND entity_id = ?", "ORDER_EXPORT_UNFINISHED", o.ID).First(&log).Error; err != nil {
			t.Fatalf("lần vượt phải có nhật ký: %v", err)
		}
		if !strings.Contains(log.Summary, "xe đang đợi") || !strings.Contains(log.Summary, "2/3") || !strings.Contains(log.Summary, "boss@t") {
			t.Fatalf("nhật ký thiếu người / lý do / số lượng: %q", log.Summary)
		}
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

// Đơn đang ở giỏ chờ (còn hàng chưa QC) bị chặn như đơn chưa QC; Owner vượt thì
// hàng đã ra khỏi kho nên giỏ chờ tự trả số giỏ.
func TestExportScan_BasketBlocksThenFreesOnForce(t *testing.T) {
	db := newHandoffDB(t)
	if err := db.AutoMigrate(&models.QCRecord{}, &models.User{}); err != nil {
		t.Fatal(err)
	}
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))
	o := seedOrderForShipping(t, db, "100120", models.StatusQCPassed, models.StatusCut)
	if err := db.Create(&models.Basket{Number: 7, OrderID: o.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ShipScannedOrderWith(context.Background(), opsActor(), "100120", exportOpts(false, "")); err == nil {
		t.Fatalf("đơn ở giỏ chờ phải bị chặn")
	}
	if b, _ := svc.repo.Basket.LiveByOrder(o.ID); b == nil {
		t.Fatalf("giỏ phải còn khi xuất kho bị chặn")
	}
	owner := Actor{ID: 1, Email: "boss@t", Role: models.RoleOwner}
	if _, err := svc.ShipScannedOrderWith(context.Background(), owner, "100120", exportOpts(true, "xe đang đợi")); err != nil {
		t.Fatalf("export: %v", err)
	}
	if b, _ := svc.repo.Basket.LiveByOrder(o.ID); b != nil {
		t.Fatalf("giỏ #%d phải được trả khi đơn đã xuất kho", b.Number)
	}
}
