package repositories

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// Bộ lọc của Bảng sản xuất (GET /api/batches?open=1&status=…): mỗi tham số, kết
// hợp nhiều tham số, khoảng ngày giáp ranh theo giờ VN, mã đơn có hậu tố _1/1,
// SKU có ký tự đại diện LIKE.

type boardSeed struct {
	db      *gorm.DB
	repo    *BatchRepository
	matWood *models.Material
	matMica *models.Material
	s1, s2  *models.Seller
}

// boardBatch tạo một batch với các dòng cho trước; trả id.
type boardLine struct {
	code, sku, storeOrder string
	seller                *models.Seller
}

func (b *boardSeed) batch(t *testing.T, code string, mat *models.Material, st models.InternalStatus, prio models.Priority, createdAt time.Time, lines ...boardLine) uint {
	t.Helper()
	bt := &models.Batch{Code: code, MaterialID: mat.ID, Status: st, Priority: prio}
	if err := b.db.Create(bt).Error; err != nil {
		t.Fatal(err)
	}
	if err := b.db.Model(bt).UpdateColumn("created_at", createdAt.UTC()).Error; err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		o := &models.Order{
			InternalCode: l.code, StoreOrderID: l.storeOrder, SellerID: l.seller.ID,
			ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
		}
		if err := b.db.Create(o).Error; err != nil {
			t.Fatal(err)
		}
		it := &models.OrderItem{
			OrderID: o.ID, InternalCode: l.code + "_1/1", SKUCode: l.sku, Quantity: 1,
			DesignStatus: models.DesignReady,
		}
		if err := b.db.Create(it).Error; err != nil {
			t.Fatal(err)
		}
		if err := b.db.Create(&models.BatchItem{BatchID: bt.ID, OrderItemID: it.ID, MaterialID: mat.ID, Status: st}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return bt.ID
}

func (b *boardSeed) ids(t *testing.T, f BatchFilter) []uint {
	t.Helper()
	f.Page = Page{Page: 1, PageSize: 200}
	rows, total, err := b.repo.List(f)
	if err != nil {
		t.Fatalf("List(%+v): %v", f, err)
	}
	if int(total) != len(rows) {
		t.Fatalf("total %d != len(rows) %d (%+v)", total, len(rows), f)
	}
	out := make([]uint, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func want(ids ...uint) []uint {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if ids == nil {
		return []uint{}
	}
	return ids
}

func TestBoardFilters(t *testing.T) {
	db := newQueueTestDB(t)
	bs := &boardSeed{db: db, repo: &BatchRepository{db: db}}
	bs.s1, bs.s2, _ = multiSeedSellers(t, db, "BD")
	bs.matWood = &models.Material{Code: "BD-WOOD", Name: "Basswood 5 ly"}
	bs.matMica = &models.Material{Code: "BD-MICA", Name: "Mica trắng 3 ly"}
	for _, m := range []*models.Material{bs.matWood, bs.matMica} {
		if err := db.Create(m).Error; err != nil {
			t.Fatal(err)
		}
	}
	vn := time.FixedZone("VN", 7*3600)
	// SQLite so sánh thời gian như chuỗi nên mọi mốc đổi sang UTC trước khi dùng
	// (Postgres thật so timestamptz, không phụ thuộc cách in múi giờ).
	d := func(y int, m time.Month, day, h, mi, s int) time.Time {
		return time.Date(y, m, day, h, mi, s, 0, vn).UTC()
	}

	// A: gỗ, s1, PENDING, NORMAL, 24/11 00:00:00 VN (đúng đầu ngày)
	A := bs.batch(t, "#BD-A", bs.matWood, models.StatusPending, models.PriorityNormal, d(2026, 11, 24, 0, 0, 0),
		boardLine{"100047", "WOOD-01", "Etsy-7821", bs.s1})
	// B: mica, s2, PRINTED, HIGH, 24/11 23:59:59 VN (giây cuối ngày)
	B := bs.batch(t, "#BD-B", bs.matMica, models.StatusPrinted, models.PriorityHigh, d(2026, 11, 24, 23, 59, 59),
		boardLine{"100048", "MICA-02", "Etsy-9000", bs.s2})
	// C: gỗ, nhiều seller + nhiều SKU, CUT, NORMAL, 25/11 00:00:00 VN (đầu ngày kế)
	C := bs.batch(t, "#BD-C", bs.matWood, models.StatusCut, models.PriorityNormal, d(2026, 11, 25, 0, 0, 0),
		boardLine{"100049", "WOOD-01-A", "Etsy-7821-B", bs.s1},
		boardLine{"100050", "WOOD_02", "Shopify-55", bs.s2})
	// D: gỗ, s1, QC_PASSED, URGENT, 23/11 23:59:59.999 VN gần như (23:59:59)
	D := bs.batch(t, "#BD-D", bs.matWood, models.StatusQCPassed, models.PriorityUrgent, d(2026, 11, 23, 23, 59, 59),
		boardLine{"100051", "WOODX02", "Etsy-1", bs.s1})
	// E: đã đóng (huỷ hết ở QC) — bảng không hiện khi open=1
	E := bs.batch(t, "#BD-E", bs.matWood, models.StatusPending, models.PriorityNormal, d(2026, 11, 24, 10, 0, 0),
		boardLine{"100052", "WOOD-01", "Etsy-7821", bs.s1})
	if err := db.Model(&models.Batch{}).Where("id = ?", E).Update("closed_at", d(2026, 11, 24, 11, 0, 0).UTC()).Error; err != nil {
		t.Fatal(err)
	}
	all := want(A, B, C, D, E)
	open := want(A, B, C, D)

	cases := []struct {
		name string
		f    BatchFilter
		want []uint
	}{
		{"không lọc", BatchFilter{}, all},
		{"open bỏ batch đã đóng", BatchFilter{ExcludeClosed: true}, open},
		{"chất liệu gỗ", BatchFilter{ExcludeClosed: true, MaterialID: &bs.matWood.ID}, want(A, C, D)},
		{"chất liệu mica", BatchFilter{ExcludeClosed: true, MaterialID: &bs.matMica.ID}, want(B)},
		{"seller s1", BatchFilter{ExcludeClosed: true, SellerIDs: []uint{bs.s1.ID}}, want(A, C, D)},
		{"seller s2 (batch nhiều seller vẫn ra một lần)", BatchFilter{ExcludeClosed: true, SellerIDs: []uint{bs.s2.ID}}, want(B, C)},
		{"seller s1+s2 (OR, không trùng dòng)", BatchFilter{ExcludeClosed: true, SellerIDs: []uint{bs.s1.ID, bs.s2.ID}}, open},
		{"ưu tiên HIGH", BatchFilter{ExcludeClosed: true, Priority: "HIGH"}, want(B)},
		{"ưu tiên URGENT", BatchFilter{ExcludeClosed: true, Priority: "URGENT"}, want(D)},
		{"trạng thái PRINTED", BatchFilter{ExcludeClosed: true, Statuses: []string{"PRINTED"}}, want(B)},
		{"trạng thái PENDING + CUT", BatchFilter{ExcludeClosed: true, Statuses: []string{"PENDING", "CUT"}}, want(A, C)},
		{"SKU đúng", BatchFilter{ExcludeClosed: true, SKU: "MICA-02"}, want(B)},
		{"SKU chữ thường, chứa chuỗi", BatchFilter{ExcludeClosed: true, SKU: "wood-01"}, want(A, C)},
		{"SKU chứa dòng khớp (batch nhiều SKU)", BatchFilter{ExcludeClosed: true, SKU: "WOOD-01-A"}, want(C)},
		{"SKU có _ khớp đúng ký tự _ chứ không phải ký tự bất kỳ", BatchFilter{ExcludeClosed: true, SKU: "WOOD_02"}, want(C)},
		{"SKU có % không thành ký tự đại diện", BatchFilter{ExcludeClosed: true, SKU: "WOOD%"}, want()},
		{"SKU dấu cách hai đầu", BatchFilter{ExcludeClosed: true, SKU: "  mica-02 "}, want(B)},
		{"SKU không tồn tại", BatchFilter{ExcludeClosed: true, SKU: "NOPE"}, want()},
		{"mã nội bộ đơn", BatchFilter{ExcludeClosed: true, Code: "100047"}, want(A)},
		{"mã nội bộ đơn kiểu tem _1/1", BatchFilter{ExcludeClosed: true, Code: "100047_1/1"}, want(A)},
		{"mã tem của dòng thứ hai trong batch nhiều đơn", BatchFilter{ExcludeClosed: true, Code: "100050_1/1"}, want(C)},
		{"mã đơn thiếu hậu tố không khớp chuỗi con", BatchFilter{ExcludeClosed: true, Code: "10004"}, want()},
		{"mã đơn shop (chứa chuỗi, không phân biệt hoa thường)", BatchFilter{ExcludeClosed: true, StoreOrder: "etsy-7821"}, want(A, C)},
		{"mã đơn shop đầy đủ", BatchFilter{ExcludeClosed: true, StoreOrder: "Shopify-55"}, want(C)},
		{"mã batch đúng, có #", BatchFilter{ExcludeClosed: true, BatchCode: "#BD-B"}, want(B)},
		{"mã batch đúng, không #", BatchFilter{ExcludeClosed: true, BatchCode: "bd-b"}, want(B)},
		{"ô tìm nhanh theo tên seller", BatchFilter{ExcludeClosed: true, Q: "Seller BD2"}, want(B, C)},
		// Kết hợp
		{"gỗ + s1 + NORMAL", BatchFilter{ExcludeClosed: true, MaterialID: &bs.matWood.ID, SellerIDs: []uint{bs.s1.ID}, Priority: "NORMAL"}, want(A, C)},
		{"seller + SKU + đơn shop phải cùng MỘT dòng: C có s2 và có WOOD-01 nhưng ở hai dòng khác nhau thì không ra", BatchFilter{ExcludeClosed: true, MaterialID: &bs.matWood.ID, SellerIDs: []uint{bs.s2.ID}, SKU: "WOOD-01"}, want()},
		{"cùng một dòng thì ra: s1 + WOOD-01-A", BatchFilter{ExcludeClosed: true, SellerIDs: []uint{bs.s1.ID}, SKU: "WOOD-01-A"}, want(C)},
		{"s1 + SKU WOOD-01 + đơn shop Etsy-7821", BatchFilter{ExcludeClosed: true, SellerIDs: []uint{bs.s1.ID}, SKU: "WOOD-01", StoreOrder: "Etsy-7821"}, want(A, C)},
		{"tất cả tham số trùng một batch", BatchFilter{ExcludeClosed: true, MaterialID: &bs.matMica.ID, SellerIDs: []uint{bs.s2.ID}, Priority: "HIGH", Statuses: []string{"PRINTED"}, SKU: "mica", Code: "100048", StoreOrder: "Etsy-9000", BatchCode: "#BD-B"}, want(B)},
		{"tất cả tham số, một cái lệch", BatchFilter{ExcludeClosed: true, MaterialID: &bs.matMica.ID, SellerIDs: []uint{bs.s2.ID}, Priority: "NORMAL", Statuses: []string{"PRINTED"}, SKU: "mica"}, want()},
	}
	for _, tc := range cases {
		if got := bs.ids(t, tc.f); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}

	// Khoảng ngày theo ngày TẠO batch, nửa mở [from, before), mốc là nửa đêm giờ VN.
	from := d(2026, 11, 24, 0, 0, 0)
	before := d(2026, 11, 25, 0, 0, 0)
	for _, tc := range []struct {
		name         string
		from, before *time.Time
		want         []uint
	}{
		{"đúng ngày 24/11 VN: gồm 00:00:00 và 23:59:59, bỏ 25/11 00:00:00 và 23/11 23:59:59", &from, &before, want(A, B, E)},
		{"chỉ có Từ ngày 25/11 VN", ptrT(d(2026, 11, 25, 0, 0, 0)), nil, want(C)},
		{"chỉ có Đến hết ngày 23/11 VN", nil, ptrT(d(2026, 11, 24, 0, 0, 0)), want(D)},
		{"từ 00:00:01 bỏ batch đúng 00:00:00", ptrT(d(2026, 11, 24, 0, 0, 1)), &before, want(B, E)},
		{"đến 23:59:59 loại trừ batch tạo đúng 23:59:59 (cận trên loại trừ)", &from, ptrT(d(2026, 11, 24, 23, 59, 59)), want(A, E)},
		{"khoảng đảo ngược cho kết quả rỗng", &before, &from, want()},
		{"hai đầu cùng một thời điểm rỗng", &from, &from, want()},
		{"cả ba ngày 23-25/11", ptrT(d(2026, 11, 23, 0, 0, 0)), ptrT(d(2026, 11, 26, 0, 0, 0)), all},
	} {
		got := bs.ids(t, BatchFilter{DateFrom: tc.from, DateBefore: tc.before})
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ngày: %s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	// Ngày + open + seller cùng lúc.
	if got := bs.ids(t, BatchFilter{ExcludeClosed: true, DateFrom: &from, DateBefore: &before, SellerIDs: []uint{bs.s1.ID}}); !reflect.DeepEqual(got, want(A)) {
		t.Errorf("ngày + open + seller: got %v, want [A]", got)
	}

	// Số đếm theo cột = tổng (total) của từng trạng thái dưới CÙNG bộ lọc.
	counts := map[string]int64{}
	for _, st := range []string{"PENDING", "PRINTED", "CUT", "QC_PASSED"} {
		_, total, err := bs.repo.List(BatchFilter{Page: Page{Page: 1, PageSize: 1}, ExcludeClosed: true, Statuses: []string{st}, SellerIDs: []uint{bs.s1.ID}})
		if err != nil {
			t.Fatal(err)
		}
		counts[st] = total
	}
	if !reflect.DeepEqual(counts, map[string]int64{"PENDING": 1, "PRINTED": 0, "CUT": 1, "QC_PASSED": 1}) {
		t.Errorf("số đếm cột (seller s1): %v", counts)
	}
}

func ptrT(t time.Time) *time.Time { return &t }
