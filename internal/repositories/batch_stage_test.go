package repositories

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// Chặng của batch (batch_stage.go) cho ô Trạng thái của Bảng sản xuất: mỗi batch
// đúng MỘT chặng, lọc ?stage= ra đúng tập đó, đếm khớp danh sách, chặng gộp lại
// khớp các token cũ (HANDED_OVER, FILES_READY), kết hợp với bộ lọc khác, sắp xếp
// cũ nhất trước + trang 2, và danh sách chọn chất liệu / seller lấy từ batch thật.

type stageLine struct {
	seller *models.Seller
	ss     models.SellerStatus
	review models.ReviewStatus       // rỗng = APPROVED
	cancel models.CancellationStatus // của DÒNG đơn; rỗng = NONE
	scrap  bool                      // phần đã huỷ ở QC
}

type stageSeed struct {
	db     *gorm.DB
	repo   *BatchRepository
	mat    *models.Material
	mat2   *models.Material
	s1, s2 *models.Seller
	n      int
}

func (s *stageSeed) batch(t *testing.T, code string, mat *models.Material, st models.InternalStatus, links []models.BatchLinkKind, lines ...stageLine) uint {
	t.Helper()
	b := &models.Batch{Code: code, MaterialID: mat.ID, Status: st, Priority: models.PriorityNormal}
	if err := s.db.Create(b).Error; err != nil {
		t.Fatal(err)
	}
	for _, k := range links {
		if err := s.db.Create(&models.BatchLink{BatchID: b.ID, Kind: k, URL: "https://x/" + code + "/" + string(k), LinkUpdatedAt: time.Now()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range lines {
		s.n++
		review := l.review
		if review == "" {
			review = models.ReviewApproved
		}
		o := &models.Order{
			InternalCode: code + "-" + string(rune('a'+s.n)), StoreOrderID: "SO-" + code, SellerID: l.seller.ID,
			ReviewStatus: review, SellerStatus: l.ss,
		}
		if err := s.db.Create(o).Error; err != nil {
			t.Fatal(err)
		}
		cancel := l.cancel
		if cancel == "" {
			cancel = models.CancellationNone
		}
		it := &models.OrderItem{OrderID: o.ID, InternalCode: o.InternalCode + "_1/1", SKUCode: "SKU-" + code, Quantity: 1,
			DesignStatus: models.DesignReady, CancellationStatus: cancel}
		if err := s.db.Create(it).Error; err != nil {
			t.Fatal(err)
		}
		bi := &models.BatchItem{BatchID: b.ID, OrderItemID: it.ID, MaterialID: mat.ID, Status: st}
		if l.scrap {
			now := time.Now()
			bi.ScrappedAt = &now
		}
		if err := s.db.Create(bi).Error; err != nil {
			t.Fatal(err)
		}
	}
	return b.ID
}

func (s *stageSeed) list(t *testing.T, f BatchFilter) ([]uint, map[uint]string) {
	t.Helper()
	if f.PageSize == 0 {
		f.Page = Page{Page: 1, PageSize: 200}
	}
	rows, total, err := s.repo.List(f)
	if err != nil {
		t.Fatalf("List(%+v): %v", f, err)
	}
	if f.PageSize == 200 && int(total) != len(rows) {
		t.Fatalf("total %d != len(rows) %d (%+v)", total, len(rows), f)
	}
	ids := make([]uint, 0, len(rows))
	stages := map[uint]string{}
	for _, r := range rows {
		ids = append(ids, r.ID)
		stages[r.ID] = r.Stage
	}
	return ids, stages
}

func sortedIDs(in []uint) []uint {
	out := append([]uint{}, in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func checkBatchStages(t *testing.T, db *gorm.DB) {
	s := &stageSeed{db: db, repo: &BatchRepository{db: db}}
	s.s1 = &models.Seller{Code: "ST1", Name: "Seller ST1"}
	s.s2 = &models.Seller{Code: "ST2", Name: "Seller ST2"}
	s.mat = &models.Material{Code: "ST-WOOD", Name: "Basswood 5 ly"}
	s.mat2 = &models.Material{Code: "ST-WOOD-B", Name: "Basswood 5 ly"} // trùng tên, khác mã
	unused := &models.Material{Code: "ST-NONE", Name: "Không batch nào"}
	for _, x := range []interface{}{s.s1, s.s2, s.mat, s.mat2, unused} {
		if err := db.Create(x).Error; err != nil {
			t.Fatal(err)
		}
	}
	both := []models.BatchLinkKind{models.BatchLinkPrint, models.BatchLinkCut}
	prod := func(sl *models.Seller) stageLine { return stageLine{seller: sl, ss: models.SellerStatusProduction} }
	with := func(sl *models.Seller, ss models.SellerStatus) stageLine { return stageLine{seller: sl, ss: ss} }

	A := s.batch(t, "#ST-A", s.mat, models.StatusPending, nil, prod(s.s1))
	A2 := s.batch(t, "#ST-A2", s.mat, models.StatusPending, []models.BatchLinkKind{models.BatchLinkPrint}, prod(s.s1)) // thiếu link cắt
	B := s.batch(t, "#ST-B", s.mat2, models.StatusPending, both, prod(s.s2))
	C := s.batch(t, "#ST-C", s.mat, models.StatusPrinted, both, prod(s.s1))
	D := s.batch(t, "#ST-D", s.mat, models.StatusCut, both, prod(s.s1), prod(s.s2))
	E := s.batch(t, "#ST-E", s.mat, models.StatusQCPassed, both, prod(s.s1))
	// Bàn giao một phần: còn một đơn ở xưởng → vẫn "Đã QC".
	M := s.batch(t, "#ST-M", s.mat, models.StatusQCPassed, both, prod(s.s1), with(s.s2, models.SellerStatusHandedOff))
	// Đơn chậm nhất đã đóng gói (đơn kia đã bàn giao) → "Đã đóng gói".
	F := s.batch(t, "#ST-F", s.mat, models.StatusQCPassed, both, with(s.s1, models.SellerStatusPacked), with(s.s2, models.SellerStatusHandedOff))
	G := s.batch(t, "#ST-G", s.mat2, models.StatusQCPassed, both, with(s.s1, models.SellerStatusHandedOff), with(s.s2, models.SellerStatusDelivered))
	H := s.batch(t, "#ST-H", s.mat, models.StatusQCPassed, both, with(s.s1, models.SellerStatusShipped))
	// Đã giao hết; phần đã huỷ ở QC và dòng đơn đã duyệt huỷ (đều còn PRODUCTION)
	// không được giữ batch lại ở "Đã QC".
	I := s.batch(t, "#ST-I", s.mat, models.StatusQCPassed, both,
		with(s.s1, models.SellerStatusDelivered),
		stageLine{seller: s.s2, ss: models.SellerStatusProduction, scrap: true},
		stageLine{seller: s.s2, ss: models.SellerStatusProduction, cancel: models.CancellationApproved})
	// Đã đóng (huỷ batch): ưu tiên trên mọi thứ khác, kể cả còn phần sống.
	J := s.batch(t, "#ST-J", s.mat, models.StatusCut, both, prod(s.s1))
	if err := db.Model(&models.Batch{}).Where("id = ?", J).Updates(map[string]interface{}{"closed_at": time.Now(), "close_reason": "Huỷ batch: test"}).Error; err != nil {
		t.Fatal(err)
	}
	// Không còn phần sống: dòng đơn đã duyệt huỷ / đơn CANCELLED.
	K := s.batch(t, "#ST-K", s.mat, models.StatusCut, both, stageLine{seller: s.s1, ss: models.SellerStatusProduction, cancel: models.CancellationApproved})
	L := s.batch(t, "#ST-L", s.mat, models.StatusQCPassed, both, stageLine{seller: s.s2, ss: models.SellerStatusProduction, review: models.ReviewCancelled})

	expect := map[string][]uint{
		BatchStageFilesMissing: {A, A2},
		BatchStageFilesReady:   {B},
		BatchStagePrinted:      {C},
		BatchStageCut:          {D},
		BatchStageQCPassed:     {E, M},
		BatchStagePacked:       {F},
		BatchStageHandedOff:    {G},
		BatchStageShipped:      {H},
		BatchStageDelivered:    {I},
		BatchStageClosed:       {J},
		BatchStageNoItems:      {K, L},
	}
	all := []uint{A, A2, B, C, D, E, M, F, G, H, I, J, K, L}

	// 1) Mỗi chặng lọc ra đúng tập của nó, mỗi dòng mang đúng trường stage.
	seen := map[uint]int{}
	for _, st := range AllBatchStages {
		ids, stages := s.list(t, BatchFilter{Stages: []string{st}})
		if got, want := sortedIDs(ids), sortedIDs(expect[st]); !reflect.DeepEqual(got, want) {
			t.Errorf("stage %s: got %v, want %v", st, got, want)
		}
		for _, id := range ids {
			seen[id]++
			if stages[id] != st {
				t.Errorf("stage %s: dòng %d mang stage %q", st, id, stages[id])
			}
		}
	}
	// 2) Loại trừ nhau và phủ hết: mỗi batch đúng một chặng.
	for _, id := range all {
		if seen[id] != 1 {
			t.Errorf("batch %d nằm ở %d chặng (phải đúng 1)", id, seen[id])
		}
	}
	// 3) Danh sách không lọc cũng mang stage cho từng dòng.
	_, stages := s.list(t, BatchFilter{})
	for st, ids := range expect {
		for _, id := range ids {
			if stages[id] != st {
				t.Errorf("danh sách thường: batch %d stage %q, want %q", id, stages[id], st)
			}
		}
	}
	// 4) Nhiều chặng = OR; khớp token cũ.
	ids, _ := s.list(t, BatchFilter{Stages: []string{BatchStageHandedOff, BatchStageShipped, BatchStageDelivered}})
	old, _ := s.list(t, BatchFilter{Statuses: []string{StatusTokenHandedOver}})
	if !reflect.DeepEqual(sortedIDs(ids), sortedIDs(old)) || !reflect.DeepEqual(sortedIDs(ids), sortedIDs([]uint{G, H, I})) {
		t.Errorf("HANDED_OFF+SHIPPED+DELIVERED %v phải = HANDED_OVER %v = [G H I]", sortedIDs(ids), sortedIDs(old))
	}
	ids, _ = s.list(t, BatchFilter{Stages: []string{BatchStageFilesReady}})
	old, _ = s.list(t, BatchFilter{ExcludeClosed: true, Statuses: []string{StatusTokenFilesReady}})
	if !reflect.DeepEqual(sortedIDs(ids), sortedIDs(old)) {
		t.Errorf("FILES_READY stage %v != token %v", ids, old)
	}
	// 5) AND với bộ lọc khác.
	ids, _ = s.list(t, BatchFilter{Stages: []string{BatchStageQCPassed, BatchStageHandedOff}, SellerIDs: []uint{s.s2.ID}})
	if got, want := sortedIDs(ids), sortedIDs([]uint{M, G}); !reflect.DeepEqual(got, want) {
		t.Errorf("stage + seller s2: got %v, want %v", got, want)
	}
	ids, _ = s.list(t, BatchFilter{Stages: []string{BatchStageFilesReady, BatchStageHandedOff}, MaterialID: &s.mat2.ID})
	if got, want := sortedIDs(ids), sortedIDs([]uint{B, G}); !reflect.DeepEqual(got, want) {
		t.Errorf("stage + chất liệu 2: got %v, want %v", got, want)
	}
	// open=1 + CLOSED = rỗng (người gọi phải bỏ open cho cột Đã đóng).
	ids, _ = s.list(t, BatchFilter{ExcludeClosed: true, Stages: []string{BatchStageClosed}})
	if len(ids) != 0 {
		t.Errorf("open + CLOSED phải rỗng, got %v", ids)
	}

	// 6) Đếm: khớp danh sách, bỏ qua status/stage/open, theo bộ lọc khác.
	counts, err := s.repo.StageCounts(BatchFilter{ExcludeClosed: true, Statuses: []string{"PRINTED"}, Stages: []string{BatchStageCut}})
	if err != nil {
		t.Fatal(err)
	}
	byStage := map[string]int64{}
	byStatus := map[string]int64{}
	for _, c := range counts {
		byStage[c.Stage] += c.Count
		byStatus[c.Status] += c.Count
	}
	for st, ids := range expect {
		if byStage[st] != int64(len(ids)) {
			t.Errorf("đếm %s = %d, want %d", st, byStage[st], len(ids))
		}
	}
	if byStatus["PENDING"] != 3 || byStatus["PRINTED"] != 1 || byStatus["CUT"] != 3 || byStatus["QC_PASSED"] != 7 {
		t.Errorf("đếm theo trạng thái lưu: %v", byStatus)
	}
	counts, err = s.repo.StageCounts(BatchFilter{SellerIDs: []uint{s.s2.ID}})
	if err != nil {
		t.Fatal(err)
	}
	byStage = map[string]int64{}
	for _, c := range counts {
		byStage[c.Stage] += c.Count
	}
	// s2 có hàng ở B, D, M, F, G, I (phần huỷ / dòng huỷ vẫn là "có hàng" theo ?seller_id=), L.
	want := map[string]int64{BatchStageFilesReady: 1, BatchStageCut: 1, BatchStageQCPassed: 1, BatchStagePacked: 1, BatchStageHandedOff: 1, BatchStageDelivered: 1, BatchStageNoItems: 1}
	if !reflect.DeepEqual(byStage, want) {
		t.Errorf("đếm theo seller s2: %v, want %v", byStage, want)
	}

	// 7) Cũ nhất trước + trang 2 nối tiếp, không trùng.
	p1, _ := s.list(t, BatchFilter{Sort: BatchSortOldest, Page: Page{Page: 1, PageSize: 5}})
	p2, _ := s.list(t, BatchFilter{Sort: BatchSortOldest, Page: Page{Page: 2, PageSize: 5}})
	if !reflect.DeepEqual(p1, []uint{A, A2, B, C, D}) || !reflect.DeepEqual(p2, []uint{E, M, F, G, H}) {
		t.Errorf("oldest: trang 1 %v, trang 2 %v", p1, p2)
	}
	n1, _ := s.list(t, BatchFilter{Page: Page{Page: 1, PageSize: 3}})
	if !reflect.DeepEqual(n1, []uint{L, K, J}) {
		t.Errorf("mặc định mới nhất trước: %v", n1)
	}

	// 8) Trạng thái lưu lạ: chặng = chính giá trị đó, đếm thấy nó.
	if err := db.Model(&models.Batch{}).Where("id = ?", C).UpdateColumn("status", "WEIRD").Error; err != nil {
		t.Fatal(err)
	}
	counts, _ = s.repo.StageCounts(BatchFilter{})
	found := false
	for _, c := range counts {
		if c.Stage == "WEIRD" && c.Status == "WEIRD" && c.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("trạng thái lạ WEIRD không có trong bảng đếm: %+v", counts)
	}
	if err := db.Model(&models.Batch{}).Where("id = ?", C).UpdateColumn("status", "PRINTED").Error; err != nil {
		t.Fatal(err)
	}

	// 9) Danh sách chọn: từ batch thật, kể cả NVL / seller đã xoá mềm còn batch.
	if err := db.Delete(s.mat2).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(s.s2).Error; err != nil {
		t.Fatal(err)
	}
	opts, err := s.repo.FilterOptions()
	if err != nil {
		t.Fatal(err)
	}
	mats := map[string]BatchFacet{}
	for _, m := range opts.Materials {
		mats[m.Code] = m
	}
	if _, ok := mats["ST-NONE"]; ok {
		t.Errorf("NVL không có batch không được có trong danh sách: %+v", opts.Materials)
	}
	if m := mats["ST-WOOD"]; m.Count != 12 || m.Deleted {
		t.Errorf("NVL ST-WOOD: %+v", m)
	}
	if m := mats["ST-WOOD-B"]; m.Count != 2 || !m.Deleted || m.Name != "Basswood 5 ly" {
		t.Errorf("NVL ST-WOOD-B (đã xoá, trùng tên): %+v", m)
	}
	sel := map[string]BatchFacet{}
	for _, x := range opts.Sellers {
		sel[x.Code] = x
	}
	// s1: A A2 C D E M F G H I J K = 12; s2: B D M F G I L = 7.
	if x := sel["ST1"]; x.Count != 12 || x.Deleted {
		t.Errorf("seller ST1: %+v", x)
	}
	if x := sel["ST2"]; x.Count != 7 || !x.Deleted {
		t.Errorf("seller ST2 (đã xoá): %+v", x)
	}
	// Số trong danh sách chọn = số batch lọc ra bằng chính lựa chọn đó.
	ids, _ = s.list(t, BatchFilter{SellerIDs: []uint{s.s2.ID}})
	if int64(len(ids)) != sel["ST2"].Count {
		t.Errorf("seller ST2: danh sách chọn ghi %d, lọc ra %d", sel["ST2"].Count, len(ids))
	}
	ids, _ = s.list(t, BatchFilter{MaterialID: &s.mat2.ID})
	if int64(len(ids)) != mats["ST-WOOD-B"].Count {
		t.Errorf("NVL ST-WOOD-B: danh sách chọn ghi %d, lọc ra %d", mats["ST-WOOD-B"].Count, len(ids))
	}
	if len(opts.Priorities) != 1 || opts.Priorities[0].Value != "NORMAL" || opts.Priorities[0].Count != 14 {
		t.Errorf("ưu tiên: %+v", opts.Priorities)
	}
}

func TestBatchStages(t *testing.T) {
	checkBatchStages(t, newQueueTestDB(t))
}

func TestBatchStagesPostgres(t *testing.T) {
	db := openHandoverPG(t)
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{},
		&models.Batch{}, &models.BatchItem{}, &models.BatchLink{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	checkBatchStages(t, db)
}
