package repositories

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// Màn Đơn hàng/Sản phẩm: lọc "Đã QC" không được ra dòng mà cột Trạng thái hiện
// "Đã bàn giao", và lọc "Đã bàn giao" (?handed_over) ra đúng nhóm đó. Đếm tổng và
// phân trang phải theo cùng bộ lọc.
func checkItemListHandoverFilter(t *testing.T, db *gorm.DB) {
	t.Helper()
	n := 0
	mk := func(st models.SellerStatus, is models.InternalStatus) *models.OrderItem {
		n++
		it := seedQueueItem(t, db, "20000"+string(rune('0'+n))+"_1/1", models.DesignReady)
		if err := db.Model(&models.Order{}).Where("id = ?", it.OrderID).Update("seller_status", st).Error; err != nil {
			t.Fatalf("seller_status: %v", err)
		}
		if err := db.Model(it).Update("internal_status", is).Error; err != nil {
			t.Fatalf("internal_status: %v", err)
		}
		return it
	}
	qc := models.StatusQCPassed
	qc1 := mk(models.SellerStatusProduction, qc)
	qc2 := mk(models.SellerStatusProduction, qc)
	qc3 := mk(models.SellerStatusProduction, qc)
	off := mk(models.SellerStatusHandedOff, qc)
	ship := mk(models.SellerStatusShipped, qc)
	done := mk(models.SellerStatusDelivered, qc)
	cut := mk(models.SellerStatusProduction, models.StatusCut)

	repo := &OrderItemRepository{db: db}
	ids := func(rows []models.OrderItem) map[uint]bool {
		m := map[uint]bool{}
		for _, r := range rows {
			m[r.ID] = true
		}
		return m
	}
	list := func(f ItemFilter, size int, page int) ([]models.OrderItem, int64) {
		f.Page = Page{Page: page, PageSize: size}
		rows, total, err := repo.List(f)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		return rows, total
	}

	// "Đã QC": chỉ 3 dòng của đơn chưa bàn giao.
	rows, total := list(ItemFilter{InternalStatus: string(qc)}, 20, 1)
	got := ids(rows)
	if total != 3 || len(rows) != 3 || !got[qc1.ID] || !got[qc2.ID] || !got[qc3.ID] {
		t.Fatalf("Đã QC phải ra đúng 3 dòng chưa bàn giao, got total=%d ids=%v", total, got)
	}
	for _, x := range []*models.OrderItem{off, ship, done, cut} {
		if got[x.ID] {
			t.Fatalf("Đã QC lọt dòng %d (đơn đã bàn giao / khâu khác)", x.ID)
		}
	}

	// Phân trang: tổng vẫn 3 khi mỗi trang 2 dòng, trang 2 còn 1.
	p1, t1 := list(ItemFilter{InternalStatus: string(qc)}, 2, 1)
	p2, t2 := list(ItemFilter{InternalStatus: string(qc)}, 2, 2)
	if t1 != 3 || t2 != 3 || len(p1) != 2 || len(p2) != 1 {
		t.Fatalf("phân trang sai: t1=%d t2=%d len1=%d len2=%d", t1, t2, len(p1), len(p2))
	}

	// "Đã bàn giao": 3 dòng (HANDED_OFF, SHIPPED, DELIVERED), kể cả khi phân trang.
	rows, total = list(ItemFilter{HandedOver: true}, 20, 1)
	got = ids(rows)
	if total != 3 || !got[off.ID] || !got[ship.ID] || !got[done.ID] {
		t.Fatalf("Đã bàn giao phải ra đúng 3 dòng, got total=%d ids=%v", total, got)
	}
	h1, ht := list(ItemFilter{HandedOver: true}, 2, 1)
	if ht != 3 || len(h1) != 2 {
		t.Fatalf("phân trang Đã bàn giao sai: total=%d len=%d", ht, len(h1))
	}

	// Hai nhóm không giao nhau và cộng lại bằng mọi dòng QC_PASSED.
	_, allQC := list(ItemFilter{InternalStatus: string(qc), HandedOver: true}, 20, 1)
	if allQC != 3 {
		t.Fatalf("QC_PASSED + bàn giao phải ra 3 dòng, got %d", allQC)
	}

	// Không lọc: thấy cả 7 (loại trừ chỉ áp dụng khi chọn Đã QC).
	_, every := list(ItemFilter{}, 20, 1)
	if every != 7 {
		t.Fatalf("không lọc phải thấy 7 dòng, got %d", every)
	}
}

func TestItemList_QCFilterExcludesHandedOver(t *testing.T) {
	checkItemListHandoverFilter(t, newQueueTestDB(t))
}
