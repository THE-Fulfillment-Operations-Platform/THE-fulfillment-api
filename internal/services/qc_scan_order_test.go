package services

import (
	"errors"
	"testing"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// TestQCScanOrder_ShowsWholeOrder là hợp đồng của "quét mã đơn ở trạm QC": quét một
// mã đơn → ra đủ các sản phẩm trong đơn kèm trạng thái QC từng cái, để nhân viên
// nhìn tổng thể rồi mở từng sản phẩm mà QC.
func TestQCScanOrder_ShowsWholeOrder(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}

	o := seedQCResultOrder(t, db, "100100", models.StatusQCPassed, models.StatusCut, models.StatusPending)
	db.Model(&models.Order{}).Where("id = ?", o.ID).Update("note", "  khách dặn gói kỹ  ")

	res, err := qc.ScanOrder(Actor{}, OrderScanRef{Code: " 100100 "})
	if err != nil {
		t.Fatalf("scan order: %v", err)
	}
	if res.InternalCode != "100100" || res.TotalItems != 3 || len(res.Items) != 3 {
		t.Fatalf("phải ra đủ 3 sản phẩm của đơn: %+v", res)
	}
	if res.PassedItems != 1 || res.WaitingItems != 2 || res.Status != QCOrderPartial {
		t.Fatalf("đếm trạng thái QC sai: %+v", res.QCResultOrder)
	}
	if res.OrderNote != "khách dặn gói kỹ" {
		t.Fatalf("ghi chú đơn phải đi kèm (đã trim), got %q", res.OrderNote)
	}
	// Mỗi dòng phải mang item_id + mã tem: nhân viên bấm vào dòng là mở đúng sản phẩm.
	for _, it := range res.Items {
		if it.ItemID == 0 || it.InternalCode == "" {
			t.Fatalf("dòng thiếu item_id / mã tem: %+v", it)
		}
	}

	// Mã đơn của người bán (store order id) cũng quét được, và theo id đơn thì khỏi cần mã.
	if r2, err := qc.ScanOrder(Actor{}, OrderScanRef{Code: "SO-100100"}); err != nil || r2.OrderID != o.ID {
		t.Fatalf("quét theo store order id: %+v err=%v", r2, err)
	}
	if r3, err := qc.ScanOrder(Actor{}, OrderScanRef{OrderID: &o.ID}); err != nil || r3.InternalCode != "100100" {
		t.Fatalf("quét theo order_id: %+v err=%v", r3, err)
	}
}

// Sản phẩm đã huỷ không tính vào đơn (không ai sản xuất / QC nó), và mã không có
// thật, đơn chưa duyệt, đơn đã huỷ đều phải báo lỗi rõ ràng chứ không trả đơn rỗng.
func TestQCScanOrder_RejectsWhatCannotBeQCd(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}

	o := seedQCResultOrder(t, db, "100200", models.StatusCut, models.StatusCut)
	var items []models.OrderItem
	db.Where("order_id = ?", o.ID).Order("line_no asc").Find(&items)
	db.Model(&models.OrderItem{}).Where("id = ?", items[1].ID).Update("cancellation_status", models.CancellationApproved)

	res, err := qc.ScanOrder(Actor{}, OrderScanRef{Code: "100200"})
	if err != nil {
		t.Fatalf("scan order: %v", err)
	}
	if res.TotalItems != 1 || len(res.Items) != 1 {
		t.Fatalf("sản phẩm đã huỷ không được tính vào đơn: %+v", res.QCResultOrder)
	}

	wantStatus := func(name string, err error, status int) {
		t.Helper()
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Status != status {
			t.Fatalf("%s: want HTTP %d, got %v", name, status, err)
		}
	}
	_, err = qc.ScanOrder(Actor{}, OrderScanRef{Code: "999999"})
	wantStatus("mã không tồn tại", err, 404)
	_, err = qc.ScanOrder(Actor{}, OrderScanRef{Code: "  "})
	wantStatus("mã rỗng", err, 400)

	pending := seedQCResultOrder(t, db, "100201", models.StatusPending)
	db.Model(&models.Order{}).Where("id = ?", pending.ID).Update("review_status", models.ReviewPending)
	_, err = qc.ScanOrder(Actor{}, OrderScanRef{Code: "100201"})
	wantStatus("đơn chưa duyệt", err, 409)

	gone := seedQCResultOrder(t, db, "100202", models.StatusCut)
	db.Model(&models.Order{}).Where("id = ?", gone.ID).Update("cancellation_status", models.CancellationApproved)
	_, err = qc.ScanOrder(Actor{}, OrderScanRef{Code: "100202"})
	wantStatus("đơn đã huỷ", err, 409)
}
