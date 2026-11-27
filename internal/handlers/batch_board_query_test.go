package handlers

import (
	"reflect"
	"testing"
	"time"

	"the-fulfillment/backend/internal/apperr"
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

// ?stage= (chặng, Bảng sản xuất) và ?sort=: đọc đủ giá trị, giá trị lạ bị từ chối
// với câu nêu giá trị hợp lệ (không âm thầm trả danh sách rỗng).
func TestBatchFilterFrom_StageAndSort(t *testing.T) {
	f := batchFilterFrom(ctxWithQuery("stage=FILES_READY,HANDED_OFF&stage=CLOSED&sort=oldest&material_id=2"))
	if !reflect.DeepEqual(f.Stages, []string{"FILES_READY", "HANDED_OFF", "CLOSED"}) || f.Sort != "oldest" || f.MaterialID == nil || *f.MaterialID != 2 {
		t.Fatalf("đọc stage/sort sai: %+v", f)
	}
	if err := checkBatchStages(f); err != nil {
		t.Fatalf("stage hợp lệ bị từ chối: %v", err)
	}
	for _, q := range []string{
		"stage=FILES_MISSING,FILES_READY,PRINTED,CUT,QC_PASSED,PACKED,HANDED_OFF,SHIPPED,DELIVERED,CLOSED,NO_ITEMS",
		"sort=newest", "sort=", "",
	} {
		if err := checkBatchStages(batchFilterFrom(ctxWithQuery(q))); err != nil {
			t.Errorf("%q bị từ chối: %v", q, err)
		}
	}
	for _, q := range []string{"stage=PENDING", "stage=handed_off", "stage=CUT,XYZ", "sort=asc"} {
		err := checkBatchStages(batchFilterFrom(ctxWithQuery(q)))
		if err == nil {
			t.Errorf("%q phải bị từ chối", q)
			continue
		}
		if ae, ok := apperr.As(err); !ok || ae.Status != 400 {
			t.Errorf("%q: phải là 400, got %v", q, err)
		}
	}
}
