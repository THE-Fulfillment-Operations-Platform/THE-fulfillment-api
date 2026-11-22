package repositories

import (
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// Lọc theo ngày tạo ở màn Đơn hàng (/api/items, /api/orders) và màn Batch
// (/api/batches) là NỬA MỞ [DateFrom, DateBefore): web gửi cận dưới = 00:00 ngày
// đầu (hoặc đúng phút đã chọn) và cận trên = 00:00 NGÀY KẾ TIẾP (hoặc đầu phút kế
// tiếp) theo giờ máy người dùng. Mốc biên phải đúng tới từng phần giây: 00:00:00
// ngày đầu có, 23:59:59,999999 ngày cuối có, 00:00 ngày kế KHÔNG. Ngày thử là
// 31/12 — sang năm mới ở cận trên.
//
// Chạy trên SQLite (Test…) và Postgres thật (Test…Postgres, cần FFM_PG_TEST_DSN —
// xem batch_handover_pg_test.go). SQLite (chỉ trong test) so thời gian bằng CHUỖI
// nên ở đó mọi mốc được đưa về UTC (cùng thời điểm); Postgres so timestamptz nên
// giữ nguyên múi +07:00 như giá trị thật.

func vnLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatalf("load Asia/Ho_Chi_Minh: %v", err)
	}
	return loc
}

func checkDateRangeFilters(t *testing.T, db *gorm.DB, norm func(time.Time) time.Time) {
	t.Helper()
	vn := vnLoc(t)
	at := func(y int, mo time.Month, d, h, mi, s, ns int) time.Time {
		return time.Date(y, mo, d, h, mi, s, ns, vn)
	}
	// Mỗi mốc: một đơn một sản phẩm + một batch, cùng created_at.
	seeds := []struct {
		code string
		at   time.Time
	}{
		{"truoc", at(2026, 12, 30, 23, 59, 59, 999999000)}, // 23:59:59,999999 hôm trước
		{"dau", at(2026, 12, 31, 0, 0, 0, 0)},              // 00:00:00 đúng
		{"p1029", at(2026, 12, 31, 10, 29, 59, 999000000)},
		{"p1030", at(2026, 12, 31, 10, 30, 0, 0)},
		{"p1030s59", at(2026, 12, 31, 10, 30, 59, 900000000)}, // giây 59,9 của phút 10:30
		{"p1031", at(2026, 12, 31, 10, 31, 0, 0)},
		{"cuoi", at(2026, 12, 31, 23, 59, 59, 0)},
		{"cuoi999", at(2026, 12, 31, 23, 59, 59, 999999000)},
		{"ke", at(2027, 1, 1, 0, 0, 0, 0)}, // 00:00 ngày kế (năm mới)
	}
	orderCode := map[uint]string{}
	itemCode := map[uint]string{}
	batchCode := map[uint]string{}
	for i, s := range seeds {
		it := seedQueueItem(t, db, fmt.Sprintf("DR%d_1/1", i), models.DesignReady)
		b := batchItems(t, db, "#DR-"+s.code, it)
		ts := norm(s.at)
		if err := db.Model(&models.Order{}).Where("id = ?", it.OrderID).UpdateColumn("created_at", ts).Error; err != nil {
			t.Fatalf("order created_at: %v", err)
		}
		if err := db.Model(&models.OrderItem{}).Where("id = ?", it.ID).UpdateColumn("created_at", ts).Error; err != nil {
			t.Fatalf("item created_at: %v", err)
		}
		if err := db.Model(&models.Batch{}).Where("id = ?", b.ID).UpdateColumn("created_at", ts).Error; err != nil {
			t.Fatalf("batch created_at: %v", err)
		}
		orderCode[it.OrderID] = s.code
		itemCode[it.ID] = s.code
		batchCode[b.ID] = s.code
	}

	page := Page{Page: 1, PageSize: 100}
	items := func(f ItemFilter) []string {
		f.Page = page
		rows, total, err := (&OrderItemRepository{db: db}).List(f)
		if err != nil {
			t.Fatalf("items: %v", err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			got[itemCode[r.ID]] = true
		}
		if int(total) != len(rows) {
			t.Errorf("items total=%d rows=%d", total, len(rows))
		}
		return sortedKeys(got)
	}
	orders := func(f OrderFilter) []string {
		f.Page = page
		rows, total, err := (&OrderRepository{db: db}).List(f)
		if err != nil {
			t.Fatalf("orders: %v", err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			got[orderCode[r.ID]] = true
		}
		if int(total) != len(rows) {
			t.Errorf("orders total=%d rows=%d", total, len(rows))
		}
		return sortedKeys(got)
	}
	batches := func(f BatchFilter) []string {
		f.Page = page
		rows, total, err := (&BatchRepository{db: db}).List(f)
		if err != nil {
			t.Fatalf("batches: %v", err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			got[batchCode[r.ID]] = true
		}
		if int(total) != len(rows) {
			t.Errorf("batches total=%d rows=%d", total, len(rows))
		}
		return sortedKeys(got)
	}
	ptr := func(x time.Time) *time.Time { y := norm(x); return &y }

	cases := []struct {
		name         string
		from, before *time.Time
		want         []string
	}{
		{
			// Chọn ngày trơn 31/12 → 31/12.
			"trọn ngày 31/12 giờ VN",
			ptr(at(2026, 12, 31, 0, 0, 0, 0)), ptr(at(2027, 1, 1, 0, 0, 0, 0)),
			set("dau", "p1029", "p1030", "p1030s59", "p1031", "cuoi", "cuoi999"),
		},
		{
			// Chọn có giờ 31/12 10:30 → 31/12 10:30: trọn phút 10:30, kể cả giây 59,9.
			"trọn phút 10:30",
			ptr(at(2026, 12, 31, 10, 30, 0, 0)), ptr(at(2026, 12, 31, 10, 31, 0, 0)),
			set("p1030", "p1030s59"),
		},
		{"chỉ có Từ (01/01)", ptr(at(2027, 1, 1, 0, 0, 0, 0)), nil, set("ke")},
		{"chỉ có Đến (hết 30/12)", nil, ptr(at(2026, 12, 31, 0, 0, 0, 0)), set("truoc")},
	}
	for _, tc := range cases {
		if got := items(ItemFilter{DateFrom: tc.from, DateBefore: tc.before}); !sameSet(got, tc.want) {
			t.Errorf("items %s: got %v, want %v", tc.name, got, tc.want)
		}
		if got := orders(OrderFilter{DateFrom: tc.from, DateBefore: tc.before}); !sameSet(got, tc.want) {
			t.Errorf("orders %s: got %v, want %v", tc.name, got, tc.want)
		}
		if got := batches(BatchFilter{DateFrom: tc.from, DateBefore: tc.before}); !sameSet(got, tc.want) {
			t.Errorf("batches %s: got %v, want %v", tc.name, got, tc.want)
		}
	}

	// OrderFilter.DateTo (Open API created_to, màn seller) vẫn là <= như cũ: đúng
	// thời điểm cận trên thì CÓ.
	if got, want := orders(OrderFilter{DateFrom: ptr(at(2026, 12, 31, 0, 0, 0, 0)), DateTo: ptr(at(2027, 1, 1, 0, 0, 0, 0))}),
		set("dau", "p1029", "p1030", "p1030s59", "p1031", "cuoi", "cuoi999", "ke"); !sameSet(got, want) {
		t.Errorf("orders DateTo <=: got %v, want %v", got, want)
	}
}

func TestDateRangeFilters_HalfOpenVN(t *testing.T) {
	checkDateRangeFilters(t, newQueueTestDB(t), func(x time.Time) time.Time { return x.UTC() })
}

func TestDateRangeFilters_HalfOpenVNPostgres(t *testing.T) {
	db := openHandoverPG(t)
	migrateMultiPG(t, db)
	if err := db.Create(&models.Seller{Code: "S1", Name: "S1"}).Error; err != nil {
		t.Fatalf("seller: %v", err)
	}
	if err := db.Create(&models.Material{Code: "M1", Name: "M1"}).Error; err != nil {
		t.Fatalf("material: %v", err)
	}
	checkDateRangeFilters(t, db, func(x time.Time) time.Time { return x })
}
