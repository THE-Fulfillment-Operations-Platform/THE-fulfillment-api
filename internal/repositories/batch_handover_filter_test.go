package repositories

import (
	"sort"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// "Đã bàn giao" của batch là nhãn suy ra, không lưu: batch QC_PASSED mà mọi phần
// còn sống đều thuộc đơn đã rời xưởng. Lọc phải tách "Đã QC" đúng như nhãn, ở
// server, để phân trang và tổng số đúng.
func TestBatchList_HandoverFilter(t *testing.T) {
	checkBatchHandoverFilter(t, newQueueTestDB(t))
}

func checkBatchHandoverFilter(t *testing.T, db *gorm.DB) {
	t.Helper()
	n := 0
	// part tạo một sản phẩm của đơn đang ở seller status st.
	part := func(st models.SellerStatus) *models.OrderItem {
		n++
		it := seedQueueItem(t, db, "10000"+string(rune('0'+n))+"_1/1", models.DesignReady)
		if err := db.Model(&models.Order{}).Where("id = ?", it.OrderID).Update("seller_status", st).Error; err != nil {
			t.Fatalf("seller_status: %v", err)
		}
		return it
	}
	mk := func(code string, status models.InternalStatus, items ...*models.OrderItem) *models.Batch {
		b := batchItems(t, db, code, items...)
		if err := db.Model(b).Update("status", status).Error; err != nil {
			t.Fatalf("batch status: %v", err)
		}
		return b
	}
	prod, off, ship, done := models.SellerStatusProduction, models.SellerStatusHandedOff, models.SellerStatusShipped, models.SellerStatusDelivered
	qc := models.StatusQCPassed

	mk("#ALL", qc, part(off), part(ship), part(done))
	mk("#PARTIAL", qc, part(off), part(prod))
	mk("#QC-ONLY", qc, part(prod), part(models.SellerStatusPacked))
	mk("#CUT-HANDED", models.StatusCut, part(off)) // chưa QC xong: không thuộc nhóm nào
	mk("#EMPTY", qc)                               // QC nhưng không còn phần sống

	// Phần bị huỷ ở QC (scrapped) và sản phẩm bị huỷ không cản bàn giao.
	scrapSibling := part(prod)
	cancelled := part(prod)
	b := mk("#IGNORES-DEAD", qc, part(off), scrapSibling, cancelled)
	now := time.Now()
	db.Model(&models.BatchItem{}).Where("batch_id = ? AND order_item_id = ?", b.ID, scrapSibling.ID).Update("scrapped_at", now)
	db.Model(&models.OrderItem{}).Where("id = ?", cancelled.ID).Update("cancellation_status", models.CancellationSeller)

	repo := &BatchRepository{db: db}
	codes := func(f BatchFilter, pageSize int) ([]string, int64) {
		f.Page = Page{Page: 1, PageSize: pageSize}
		rows, total, err := repo.List(f)
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		out := []string{}
		for _, r := range rows {
			out = append(out, r.Code)
		}
		sort.Strings(out)
		return out, total
	}
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// Cờ handed_over trên từng dòng khớp bộ lọc.
	all, _, err := repo.List(BatchFilter{Page: Page{Page: 1, PageSize: 50}})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	for _, r := range all {
		if want := r.Code == "#ALL" || r.Code == "#IGNORES-DEAD"; r.HandedOver != want {
			t.Errorf("%s handed_over = %v, want %v", r.Code, r.HandedOver, want)
		}
	}

	got, total := codes(BatchFilter{Status: "QC_PASSED", Handover: "done"}, 50)
	if want := []string{"#ALL", "#IGNORES-DEAD"}; !eq(got, want) || total != 2 {
		t.Errorf("done = %v (total %d), want %v", got, total, want)
	}
	got, total = codes(BatchFilter{Status: "QC_PASSED", Handover: "pending"}, 50)
	if want := []string{"#EMPTY", "#PARTIAL", "#QC-ONLY"}; !eq(got, want) || total != 3 {
		t.Errorf("pending = %v (total %d), want %v", got, total, want)
	}
	// Không có Handover (bảng điều khiển đếm theo status) vẫn thấy đủ 5 batch QC.
	if got, total = codes(BatchFilter{Status: "QC_PASSED"}, 50); len(got) != 5 || total != 5 {
		t.Errorf("plain QC_PASSED = %v (total %d), want 5", got, total)
	}
	// Phân trang: tổng đếm theo toàn bộ, không theo trang.
	got, total = codes(BatchFilter{Status: "QC_PASSED", Handover: "pending"}, 2)
	if len(got) != 2 || total != 3 {
		t.Errorf("page of pending = %v (total %d), want 2 rows / total 3", got, total)
	}
}
