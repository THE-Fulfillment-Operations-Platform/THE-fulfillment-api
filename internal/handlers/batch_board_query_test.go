package handlers

import (
	"reflect"
	"testing"
	"time"
)

// Bảng sản xuất gọi GET /api/batches với đủ bộ lọc; kiểm cách đọc từng tham số,
// kể cả mã đơn có "/" (_1/1) đã mã hoá URL, và khoảng ngày theo giờ VN.
func TestBatchFilterFrom_Board(t *testing.T) {
	q := "open=1&status=PENDING,CUT&material_id=7&seller_id=3,9&sku=wood-01&priority=HIGH" +
		"&code=100047_1%2F1&store_order=Etsy-7821&batch=%23101602&q=abc" +
		"&date_from=2026-11-23T17:00:00Z&date_to=2026-11-24T17:00:00Z"
	f := batchFilterFrom(ctxWithQuery(q))
	if !f.ExcludeClosed || !reflect.DeepEqual(f.Statuses, []string{"PENDING", "CUT"}) ||
		f.MaterialID == nil || *f.MaterialID != 7 || !reflect.DeepEqual(f.SellerIDs, []uint{3, 9}) ||
		f.SKU != "wood-01" || f.Priority != "HIGH" || f.Code != "100047_1/1" ||
		f.StoreOrder != "Etsy-7821" || f.BatchCode != "#101602" || f.Q != "abc" {
		t.Fatalf("đọc tham số sai: %+v", f)
	}
	from := time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC) // 24/11 00:00 giờ VN
	before := time.Date(2026, 11, 24, 17, 0, 0, 0, time.UTC)
	if f.DateFrom == nil || !f.DateFrom.Equal(from) || f.DateBefore == nil || !f.DateBefore.Equal(before) {
		t.Fatalf("khoảng ngày: from=%v before=%v", f.DateFrom, f.DateBefore)
	}
	// Mốc kiểu +07:00 (web chạy máy VN gửi Z, nhưng client khác có thể gửi offset) là cùng thời điểm.
	g := batchFilterFrom(ctxWithQuery("date_from=2026-11-24T00:00:00%2B07:00"))
	if g.DateFrom == nil || !g.DateFrom.Equal(from) {
		t.Fatalf("offset +07:00: %v", g.DateFrom)
	}
	// Không tham số: không lọc gì, không tự bật open.
	e := batchFilterFrom(ctxWithQuery(""))
	if e.ExcludeClosed || e.SKU != "" || e.DateFrom != nil || e.DateBefore != nil || len(e.Statuses) != 0 || len(e.SellerIDs) != 0 {
		t.Fatalf("mặc định phải rỗng: %+v", e)
	}
}
