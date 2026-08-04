package services

import (
	"fmt"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// seedQCResultOrder tạo 1 đơn đã duyệt với n sản phẩm ở trạng thái cho trước.
func seedQCResultOrder(t *testing.T, db *gorm.DB, code string, statuses ...models.InternalStatus) *models.Order {
	t.Helper()
	seller := &models.Seller{Code: "S" + code, Name: "Seller " + code}
	db.Create(seller)
	o := &models.Order{
		InternalCode: code, StoreOrderID: "SO-" + code, SellerID: seller.ID,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	for i, st := range statuses {
		it := &models.OrderItem{
			OrderID: o.ID, LineNo: i + 1, InternalCode: code + "_" + string(rune('1'+i)),
			SKUCode: "SKU-1", Quantity: 1, InternalStatus: st, DesignStatus: models.DesignReady,
		}
		if err := db.Create(it).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}
	return o
}

// TestQCResults_GroupsOrdersByQCState là hợp đồng của màn "Kết quả QC": đơn nào
// đã QC đủ (đóng gói lấy được), đơn nào mới xong một phần, đơn nào chưa có gì —
// và con số tổng phải tính trên TOÀN BỘ tập lọc chứ không phải trang đang xem.
func TestQCResults_GroupsOrdersByQCState(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}

	seedQCResultOrder(t, db, "100001", models.StatusQCPassed, models.StatusQCPassed) // DONE
	seedQCResultOrder(t, db, "100002", models.StatusQCPassed, models.StatusCut)      // PARTIAL
	seedQCResultOrder(t, db, "100003", models.StatusPending)                         // NONE

	rows, summary, total, err := qc.QCResults(repositories.QCResultFilter{})
	if err != nil {
		t.Fatalf("QC results: %v", err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("want 3 đơn, got total=%d rows=%d", total, len(rows))
	}
	byCode := map[string]QCResultOrder{}
	for _, r := range rows {
		byCode[r.InternalCode] = r
	}
	if got := byCode["100001"]; got.Status != QCOrderDone || got.PassedItems != 2 || got.WaitingItems != 0 {
		t.Fatalf("đơn QC đủ: %+v", got)
	}
	if got := byCode["100002"]; got.Status != QCOrderPartial || got.PassedItems != 1 || got.WaitingItems != 1 {
		t.Fatalf("đơn đạt một phần: %+v", got)
	}
	if got := byCode["100003"]; got.Status != QCOrderNone || got.PassedItems != 0 {
		t.Fatalf("đơn chưa QC: %+v", got)
	}
	if summary.OrdersDone != 1 || summary.OrdersPartial != 1 || summary.OrdersNone != 1 {
		t.Fatalf("summary đơn: %+v", summary)
	}
	if summary.ItemsPassed != 3 || summary.ItemsWaiting != 2 {
		t.Fatalf("summary sản phẩm: %+v", summary)
	}

	// Lọc theo nhóm phải khớp với con số trên ô thống kê…
	done, doneSummary, doneTotal, err := qc.QCResults(repositories.QCResultFilter{State: "done"})
	if err != nil {
		t.Fatalf("lọc done: %v", err)
	}
	if doneTotal != 1 || len(done) != 1 || done[0].InternalCode != "100001" {
		t.Fatalf("lọc 'đã QC đủ' phải ra đúng đơn 100001, got %+v", done)
	}
	// …và ô thống kê KHÔNG được đổi theo bộ lọc, nếu không người dùng lọc một
	// nhóm là mất đường quay lại các nhóm khác.
	if doneSummary.OrdersNone != 1 || doneSummary.OrdersPartial != 1 {
		t.Fatalf("summary phải giữ nguyên khi lọc: %+v", doneSummary)
	}
}

// TestQCResults_ReworkAndSearch: sản phẩm đã fail và đang làm lại phải hiện là
// "đang làm lại"; làm lại xong và đạt thì thôi. Và ô tìm kiếm phải bắt được cả
// mã đơn lẫn mã sản phẩm.
func TestQCResults_ReworkAndSearch(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}

	o := seedQCResultOrder(t, db, "100010", models.StatusCut, models.StatusQCPassed)
	var items []models.OrderItem
	db.Where("order_id = ?", o.ID).Order("line_no asc").Find(&items)
	// #1 đã fail 1 lần, chưa đạt lại → đang làm lại.
	db.Model(&models.OrderItem{}).Where("id = ?", items[0].ID).Update("rework_count", 1)
	// #2 từng fail nhưng đã đạt sau khi làm lại → KHÔNG còn là việc phải theo.
	db.Model(&models.OrderItem{}).Where("id = ?", items[1].ID).Update("rework_count", 2)

	rows, summary, _, err := qc.QCResults(repositories.QCResultFilter{})
	if err != nil {
		t.Fatalf("QC results: %v", err)
	}
	if len(rows) != 1 || rows[0].ReworkItems != 1 {
		t.Fatalf("chỉ sản phẩm chưa đạt mới tính là đang làm lại: %+v", rows)
	}
	if summary.ItemsRework != 1 {
		t.Fatalf("summary items_rework = %d, want 1", summary.ItemsRework)
	}
	states := map[string]string{}
	for _, it := range rows[0].Items {
		states[it.InternalCode] = it.QCStatus
	}
	if states[items[0].InternalCode] != QCItemRework || states[items[1].InternalCode] != QCItemPassed {
		t.Fatalf("trạng thái từng sản phẩm sai: %+v", states)
	}

	// Tìm theo mã sản phẩm phải ra đơn chứa nó.
	found, _, total, err := qc.QCResults(repositories.QCResultFilter{Search: items[0].InternalCode})
	if err != nil || total != 1 || len(found) != 1 {
		t.Fatalf("tìm theo mã sản phẩm: err=%v total=%d rows=%d", err, total, len(found))
	}
	// Tìm chuỗi không tồn tại thì không ra gì.
	if _, _, n, _ := qc.QCResults(repositories.QCResultFilter{Search: "KHONG-CO-MA-NAY"}); n != 0 {
		t.Fatalf("tìm mã không tồn tại phải ra 0, got %d", n)
	}
}

// TestQCResults_StaysOffTheRowByRowPath: số câu lệnh phải CỐ ĐỊNH, không tăng
// theo số đơn hay số sản phẩm. Đo trên 5 đơn và 30 đơn rồi so — cách này không
// phụ thuộc vào con số tuyệt đối (preload/driver có thể đổi), mà bắt đúng thứ
// cần bắt: truy vấn theo từng dòng.
func TestQCResults_StaysOffTheRowByRowPath(t *testing.T) {
	measure := func(orders int) int {
		db := newQCDB(t)
		repo := repositories.New(db)
		qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}
		for i := 0; i < orders; i++ {
			seedQCResultOrder(t, db, fmt.Sprintf("9%04d", i),
				models.StatusQCPassed, models.StatusCut, models.StatusPending)
		}
		stmts := 0
		count := func(*gorm.DB) { stmts++ }
		for _, reg := range []func(string, func(*gorm.DB)) error{
			db.Callback().Query().After("gorm:query").Register,
			db.Callback().Row().After("gorm:row").Register,
			db.Callback().Raw().After("gorm:raw").Register,
		} {
			if err := reg("test:count", count); err != nil {
				t.Fatalf("register callback: %v", err)
			}
		}
		rows, _, _, err := qc.QCResults(repositories.QCResultFilter{
			Page: repositories.Page{Page: 1, PageSize: 100},
		})
		if err != nil {
			t.Fatalf("QC results: %v", err)
		}
		if len(rows) != orders {
			t.Fatalf("want %d đơn, got %d", orders, len(rows))
		}
		return stmts
	}

	small, big := measure(5), measure(30)
	if small != big {
		t.Fatalf("số câu lệnh tăng theo dữ liệu: 5 đơn → %d, 30 đơn → %d", small, big)
	}
	// Và phải là một nhúm, không phải hàng chục.
	if big > 12 {
		t.Fatalf("một trang tốn %d câu lệnh — quá nhiều cho một màn đọc", big)
	}
}
