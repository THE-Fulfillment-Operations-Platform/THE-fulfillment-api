package repositories

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// Lọc chọn nhiều (chủ xưởng 22/11): ô Seller và ô Trạng thái của màn Batch và màn
// Đơn hàng tick được nhiều mục. Luật: OR trong cùng một ô, AND giữa các ô; mỗi
// token trạng thái khớp ĐÚNG nhóm dòng của lựa chọn đơn kiểu cũ tương ứng; tổng
// đếm và phân trang theo cùng bộ lọc. Chạy trên SQLite (Test…) và Postgres thật
// (Test…Postgres, cần FFM_PG_TEST_DSN — xem batch_handover_pg_test.go).

// multiSeedSellers tạo 3 seller (Postgres có khoá ngoại thật nên phải có hàng).
func multiSeedSellers(t *testing.T, db *gorm.DB, prefix string) (*models.Seller, *models.Seller, *models.Seller) {
	t.Helper()
	out := make([]*models.Seller, 3)
	for i := range out {
		out[i] = &models.Seller{Code: fmt.Sprintf("%s%d", prefix, i+1), Name: fmt.Sprintf("Seller %s%d", prefix, i+1)}
		if err := db.Create(out[i]).Error; err != nil {
			t.Fatalf("seed seller: %v", err)
		}
	}
	return out[0], out[1], out[2]
}

// multiSeedItem tạo một đơn (đã duyệt) một sản phẩm của seller s.
func multiSeedItem(t *testing.T, db *gorm.DB, code string, s *models.Seller, st models.SellerStatus, is models.InternalStatus) *models.OrderItem {
	t.Helper()
	order := &models.Order{
		InternalCode: "ORD-" + code,
		StoreOrderID: "SO-" + code,
		SellerID:     s.ID,
		ReviewStatus: models.ReviewApproved,
		SellerStatus: st,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order %s: %v", code, err)
	}
	item := &models.OrderItem{
		OrderID:      order.ID,
		InternalCode: code + "_1/1",
		SKUCode:      "SKU-1",
		Quantity:     1,
		DesignStatus: models.DesignReady,
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("seed item %s: %v", code, err)
	}
	if err := db.Model(item).Update("internal_status", is).Error; err != nil {
		t.Fatalf("internal_status %s: %v", code, err)
	}
	return item
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func set(xs ...string) []string {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return sortedKeys(m)
}

// ---------- Batch ----------

func checkBatchMultiFilter(t *testing.T, db *gorm.DB) {
	t.Helper()
	s1, s2, s3 := multiSeedSellers(t, db, "MB")
	mat := &models.Material{Code: "MB-MAT", Name: "Gỗ MB"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("material: %v", err)
	}
	n := 0
	item := func(s *models.Seller, st models.SellerStatus) *models.OrderItem {
		n++
		return multiSeedItem(t, db, fmt.Sprintf("41%04d", n), s, st, models.StatusPending)
	}
	mk := func(code string, status models.InternalStatus, links bool, items ...*models.OrderItem) {
		b := &models.Batch{Code: code, MaterialID: mat.ID, Status: status}
		if err := db.Create(b).Error; err != nil {
			t.Fatalf("batch %s: %v", code, err)
		}
		if links {
			for _, k := range []models.BatchLinkKind{models.BatchLinkPrint, models.BatchLinkCut} {
				l := &models.BatchLink{BatchID: b.ID, Kind: k, URL: "https://files/" + code + "/" + string(k), LinkUpdatedAt: time.Now()}
				if err := db.Create(l).Error; err != nil {
					t.Fatalf("link: %v", err)
				}
			}
		}
		for _, it := range items {
			bi := &models.BatchItem{BatchID: b.ID, OrderItemID: it.ID, MaterialID: mat.ID, Status: status}
			if err := db.Create(bi).Error; err != nil {
				t.Fatalf("batch item: %v", err)
			}
		}
	}
	prod, off := models.SellerStatusProduction, models.SellerStatusHandedOff
	mk("#M-READY", models.StatusPending, true, item(s1, prod))   // Đã có file SX
	mk("#M-NOFILE", models.StatusPending, false, item(s2, prod)) // Chờ xử lý (thiếu file)
	mk("#M-PRINTED", models.StatusPrinted, true, item(s1, prod))
	mk("#M-CUT", models.StatusCut, true, item(s3, prod))
	mk("#M-HANDED", models.StatusQCPassed, true, item(s2, off))              // Đã bàn giao
	mk("#M-QC", models.StatusQCPassed, true, item(s1, prod), item(s2, prod)) // Đã QC, hàng của 2 seller

	repo := &BatchRepository{db: db}
	list := func(f BatchFilter, size, page int) ([]string, int64) {
		t.Helper()
		f.Page = Page{Page: page, PageSize: size}
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
	codes := func(f BatchFilter) []string {
		t.Helper()
		got, total := list(f, 50, 1)
		if int(total) != len(got) {
			t.Errorf("%+v: total %d != rows %d", f, total, len(got))
		}
		// Không dòng nào ra hai lần (batch có hàng của 2 seller, token trùng nhau…).
		seen := map[string]bool{}
		for _, c := range got {
			if seen[c] {
				t.Errorf("%+v: batch %s ra 2 lần", f, c)
			}
			seen[c] = true
		}
		return got
	}
	expect := func(name string, f BatchFilter, want ...string) {
		t.Helper()
		if got := codes(f); !sameSet(got, set(want...)) {
			t.Errorf("%s: got %v, want %v", name, got, set(want...))
		}
	}

	// 1) Mỗi token một mình = đúng lựa chọn đơn kiểu cũ.
	legacy := map[string]BatchFilter{
		StatusTokenFilesReady:      {Status: "PENDING", Files: "ready"},
		StatusTokenFilesMissing:    {Status: "PENDING", Files: "missing"},
		StatusTokenHandedOver:      {Status: "QC_PASSED", Handover: "done"},
		StatusTokenHandoverPending: {Status: "QC_PASSED", Handover: "pending"},
		"PENDING":                  {Status: "PENDING"},
		"PRINTED":                  {Status: "PRINTED"},
		"CUT":                      {Status: "CUT"},
		"QC_PASSED":                {Status: "QC_PASSED"},
	}
	for tok, old := range legacy {
		got, want := codes(BatchFilter{Statuses: []string{tok}}), codes(old)
		if !sameSet(got, want) {
			t.Errorf("token %s = %v, lựa chọn cũ %+v = %v", tok, got, old, want)
		}
		if len(want) == 0 {
			t.Errorf("token %s: dữ liệu mẫu không có dòng nào — test không kiểm được gì", tok)
		}
	}

	// 2) Nhiều token: OR. Ví dụ của khách: Chờ xử lý + Đã có file SX + Đã in.
	multi := BatchFilter{Statuses: []string{StatusTokenFilesMissing, StatusTokenFilesReady, "PRINTED"}}
	expect("Chờ xử lý + Đã có file SX + Đã in", multi, "#M-READY", "#M-NOFILE", "#M-PRINTED")
	expect("Đã bàn giao + Đã QC = mọi QC_PASSED", BatchFilter{Statuses: []string{StatusTokenHandedOver, StatusTokenHandoverPending}}, "#M-HANDED", "#M-QC")
	expect("PENDING thật + FILES_READY không nhân đôi", BatchFilter{Statuses: []string{"PENDING", StatusTokenFilesReady}}, "#M-READY", "#M-NOFILE")
	expect("token trùng / ô trống", BatchFilter{Statuses: []string{StatusTokenFilesReady, " ", StatusTokenFilesReady, ""}}, "#M-READY")
	expect("token lạ không khớp gì", BatchFilter{Statuses: []string{"NOPE"}})
	all := []string{"#M-READY", "#M-NOFILE", "#M-PRINTED", "#M-CUT", "#M-HANDED", "#M-QC"}
	expect("danh sách rỗng = không lọc", BatchFilter{Statuses: []string{}}, all...)
	expect("toàn ô trống = không lọc", BatchFilter{Statuses: []string{"", "  "}}, all...)
	expect("Status cũ + Statuses = AND", BatchFilter{Status: "PENDING", Statuses: []string{StatusTokenFilesReady, "PRINTED"}}, "#M-READY")

	// Phân trang: tổng theo toàn bộ, trang 2 còn phần dư.
	if p1, t1 := list(multi, 2, 1); len(p1) != 2 || t1 != 3 {
		t.Errorf("trang 1 = %v (total %d), want 2 dòng / total 3", p1, t1)
	}
	if p2, t2 := list(multi, 2, 2); len(p2) != 1 || t2 != 3 {
		t.Errorf("trang 2 = %v (total %d), want 1 dòng / total 3", p2, t2)
	}

	// 3) Seller nhiều giá trị.
	for _, s := range []*models.Seller{s1, s2, s3} {
		id := s.ID
		if got, want := codes(BatchFilter{SellerIDs: []uint{id}}), codes(BatchFilter{SellerID: &id}); !sameSet(got, want) {
			t.Errorf("SellerIDs [%d] = %v, SellerID = %v", id, got, want)
		}
	}
	expect("seller 1 + 2 (batch có cả 2 seller ra 1 lần)", BatchFilter{SellerIDs: []uint{s1.ID, s2.ID}},
		"#M-READY", "#M-NOFILE", "#M-PRINTED", "#M-HANDED", "#M-QC")
	expect("seller 3", BatchFilter{SellerIDs: []uint{s3.ID}}, "#M-CUT")
	sid := s1.ID
	expect("SellerID cố định không bị nới bởi SellerIDs", BatchFilter{SellerID: &sid, SellerIDs: []uint{s3.ID}})

	// 4) AND giữa hai ô.
	expect("seller 1+2 × Đã bàn giao + Đã QC", BatchFilter{
		SellerIDs: []uint{s1.ID, s2.ID},
		Statuses:  []string{StatusTokenHandedOver, StatusTokenHandoverPending},
	}, "#M-HANDED", "#M-QC")
	expect("seller 2 × Chờ xử lý + Đã có file SX", BatchFilter{
		SellerIDs: []uint{s2.ID},
		Statuses:  []string{StatusTokenFilesMissing, StatusTokenFilesReady},
	}, "#M-NOFILE")
	expect("seller 3 × Chờ xử lý + Đã in", BatchFilter{
		SellerIDs: []uint{s3.ID},
		Statuses:  []string{StatusTokenFilesMissing, "PRINTED"},
	})
}

func TestBatchList_MultiSelectFilters(t *testing.T) {
	checkBatchMultiFilter(t, newQueueTestDB(t))
}

func TestBatchList_MultiSelectFiltersPostgres(t *testing.T) {
	db := openHandoverPG(t)
	migrateMultiPG(t, db)
	checkBatchMultiFilter(t, db)
}

// ---------- Items (màn Đơn hàng / Sản phẩm, nút Xuất theo mẫu import) ----------

func checkItemMultiFilter(t *testing.T, db *gorm.DB) {
	t.Helper()
	s1, s2, s3 := multiSeedSellers(t, db, "MI")
	mat := &models.Material{Code: "MI-MAT", Name: "Gỗ MI"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("material: %v", err)
	}
	n := 0
	mk := func(s *models.Seller, st models.SellerStatus, is models.InternalStatus) *models.OrderItem {
		n++
		return multiSeedItem(t, db, fmt.Sprintf("42%04d", n), s, st, is)
	}
	prod := models.SellerStatusProduction
	pendReady := mk(s1, prod, models.StatusPending)
	pendMissing := mk(s2, prod, models.StatusPending)
	printed := mk(s1, prod, models.StatusPrinted)
	cut := mk(s3, prod, models.StatusCut)
	qc := mk(s2, prod, models.StatusQCPassed)
	qcUnapproved := mk(s1, prod, models.StatusQCPassed)
	packed := mk(s1, models.SellerStatusPacked, models.StatusQCPassed)
	packedCancelled := mk(s3, models.SellerStatusPacked, models.StatusQCPassed)
	handed := mk(s2, models.SellerStatusHandedOff, models.StatusQCPassed)
	shipped := mk(s3, models.SellerStatusShipped, models.StatusQCPassed)
	if err := db.Model(&models.Order{}).Where("id = ?", qcUnapproved.OrderID).Update("review_status", models.ReviewPending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.Order{}).Where("id = ?", packedCancelled.OrderID).Update("review_status", models.ReviewCancelled).Error; err != nil {
		t.Fatal(err)
	}
	// pendReady: nằm trong batch đủ link in + cắt → "Đã có file SX".
	b := &models.Batch{Code: "#MI-READY", MaterialID: mat.ID, Status: models.StatusPending}
	if err := db.Create(b).Error; err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, k := range []models.BatchLinkKind{models.BatchLinkPrint, models.BatchLinkCut} {
		if err := db.Create(&models.BatchLink{BatchID: b.ID, Kind: k, URL: "https://files/mi/" + string(k), LinkUpdatedAt: time.Now()}).Error; err != nil {
			t.Fatalf("link: %v", err)
		}
	}
	if err := db.Create(&models.BatchItem{BatchID: b.ID, OrderItemID: pendReady.ID, MaterialID: mat.ID, Status: models.StatusPending}).Error; err != nil {
		t.Fatalf("batch item: %v", err)
	}

	name := map[uint]string{
		pendReady.ID: "pendReady", pendMissing.ID: "pendMissing", printed.ID: "printed", cut.ID: "cut",
		qc.ID: "qc", qcUnapproved.ID: "qcUnapproved", packed.ID: "packed", packedCancelled.ID: "packedCancelled",
		handed.ID: "handed", shipped.ID: "shipped",
	}
	repo := &OrderItemRepository{db: db}
	list := func(f ItemFilter, size, page int) ([]string, int64) {
		t.Helper()
		f.Page = Page{Page: page, PageSize: size}
		rows, total, err := repo.List(f)
		if err != nil {
			t.Fatalf("List %+v: %v", f, err)
		}
		out := []string{}
		for _, r := range rows {
			out = append(out, name[r.ID])
		}
		sort.Strings(out)
		return out, total
	}
	items := func(f ItemFilter) []string {
		t.Helper()
		got, total := list(f, 50, 1)
		if int(total) != len(got) {
			t.Errorf("%+v: total %d != rows %d", f, total, len(got))
		}
		// ListAll (nút Xuất theo mẫu import) phải ra đúng tập của List.
		all, err := repo.ListAll(f)
		if err != nil {
			t.Fatalf("ListAll: %v", err)
		}
		if len(all) != len(got) {
			t.Errorf("%+v: ListAll %d dòng, List %d dòng", f, len(all), len(got))
		}
		return got
	}
	expect := func(label string, f ItemFilter, want ...string) {
		t.Helper()
		if got := items(f); !sameSet(got, set(want...)) {
			t.Errorf("%s: got %v, want %v", label, got, set(want...))
		}
	}

	// 1) Mỗi token một mình = đúng lựa chọn đơn kiểu cũ (statusParams ở web).
	legacy := map[string]ItemFilter{
		StatusTokenFilesReady:   {InternalStatus: "PENDING", Files: "ready"},
		StatusTokenFilesMissing: {InternalStatus: "PENDING", Files: "missing"},
		StatusTokenPacked:       {Packed: true},
		StatusTokenHandedOver:   {HandedOver: true},
		"QC_PASSED":             {InternalStatus: "QC_PASSED"},
		"PENDING":               {InternalStatus: "PENDING"},
		"PRINTED":               {InternalStatus: "PRINTED"},
		"CUT":                   {InternalStatus: "CUT"},
	}
	for tok, old := range legacy {
		got, want := items(ItemFilter{InternalStatuses: []string{tok}}), items(old)
		if !sameSet(got, want) {
			t.Errorf("token %s = %v, lựa chọn cũ %+v = %v", tok, got, old, want)
		}
		if len(want) == 0 {
			t.Errorf("token %s: dữ liệu mẫu không có dòng nào — test không kiểm được gì", tok)
		}
	}
	expect("Đã QC loại đơn chưa duyệt / đã đóng gói / đã bàn giao", ItemFilter{InternalStatuses: []string{"QC_PASSED"}}, "qc")
	expect("Đã đóng gói loại đơn đã huỷ", ItemFilter{InternalStatuses: []string{StatusTokenPacked}}, "packed")

	// 2) Nhiều token = hợp của từng token, với mọi cặp (và ví dụ của khách).
	toks := []string{StatusTokenFilesReady, StatusTokenFilesMissing, "PRINTED", "CUT", "QC_PASSED", StatusTokenPacked, StatusTokenHandedOver}
	single := map[string][]string{}
	for _, tok := range toks {
		single[tok] = items(ItemFilter{InternalStatuses: []string{tok}})
	}
	for i := range toks {
		for j := i + 1; j < len(toks); j++ {
			got := items(ItemFilter{InternalStatuses: []string{toks[i], toks[j]}})
			if want := set(append(append([]string{}, single[toks[i]]...), single[toks[j]]...)...); !sameSet(got, want) {
				t.Errorf("%s + %s = %v, want %v", toks[i], toks[j], got, want)
			}
		}
	}
	custMulti := ItemFilter{InternalStatuses: []string{StatusTokenFilesMissing, StatusTokenFilesReady, "PRINTED"}}
	expect("Chờ xử lý + Đã có file SX + Đã in", custMulti, "pendReady", "pendMissing", "printed")
	expect("Đã QC + Đã đóng gói + Đã bàn giao", ItemFilter{InternalStatuses: []string{"QC_PASSED", StatusTokenPacked, StatusTokenHandedOver}},
		"qc", "packed", "handed", "shipped")
	expect("token lạ không khớp gì", ItemFilter{InternalStatuses: []string{"NOPE"}})
	expect("InternalStatus cũ + InternalStatuses = AND", ItemFilter{InternalStatus: "QC_PASSED", InternalStatuses: []string{"QC_PASSED", StatusTokenPacked}}, "qc")
	if _, every := list(ItemFilter{InternalStatuses: []string{" ", ""}}, 50, 1); every != 10 {
		t.Errorf("toàn ô trống phải là không lọc (10 dòng), got %d", every)
	}

	// Phân trang.
	if p1, t1 := list(custMulti, 2, 1); len(p1) != 2 || t1 != 3 {
		t.Errorf("trang 1 = %v (total %d), want 2 dòng / total 3", p1, t1)
	}
	if p2, t2 := list(custMulti, 2, 2); len(p2) != 1 || t2 != 3 {
		t.Errorf("trang 2 = %v (total %d), want 1 dòng / total 3", p2, t2)
	}

	// 3) Seller nhiều giá trị.
	for _, s := range []*models.Seller{s1, s2, s3} {
		id := s.ID
		if got, want := items(ItemFilter{SellerIDs: []uint{id}}), items(ItemFilter{SellerID: &id}); !sameSet(got, want) {
			t.Errorf("SellerIDs [%d] = %v, SellerID = %v", id, got, want)
		}
	}
	expect("seller 1 + 3", ItemFilter{SellerIDs: []uint{s1.ID, s3.ID}},
		"pendReady", "printed", "qcUnapproved", "packed", "cut", "packedCancelled", "shipped")
	sid := s2.ID
	expect("SellerID cố định không bị nới bởi SellerIDs", ItemFilter{SellerID: &sid, SellerIDs: []uint{s1.ID}})

	// 4) AND giữa hai ô.
	expect("seller 1 + 3 × Đã đóng gói + Đã bàn giao", ItemFilter{
		SellerIDs:        []uint{s1.ID, s3.ID},
		InternalStatuses: []string{StatusTokenPacked, StatusTokenHandedOver},
	}, "packed", "shipped")
	expect("seller 2 × Chờ xử lý + Đã có file SX + Đã in", ItemFilter{
		SellerIDs:        []uint{s2.ID},
		InternalStatuses: []string{StatusTokenFilesMissing, StatusTokenFilesReady, "PRINTED"},
	}, "pendMissing")
	// Tham số cũ (?packed=true) vẫn AND với ô chọn nhiều.
	expect("Packed cũ AND ô trạng thái", ItemFilter{Packed: true, InternalStatuses: []string{StatusTokenPacked, StatusTokenHandedOver}}, "packed")
}

func TestItemList_MultiSelectFilters(t *testing.T) {
	checkItemMultiFilter(t, newQueueTestDB(t))
}

func TestItemList_MultiSelectFiltersPostgres(t *testing.T) {
	db := openHandoverPG(t)
	migrateMultiPG(t, db)
	checkItemMultiFilter(t, db)
}

func migrateMultiPG(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{},
		&models.Batch{}, &models.BatchItem{}, &models.BatchLink{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// Điều kiện dựng ra luôn nằm gọn trong MỘT cặp ngoặc: OR của ô trạng thái không
// được tràn ra chuỗi AND của các ô khác.
func TestStatusCond_GroupedAndArgsAligned(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func([]string) (string, []interface{}, bool)
		toks []string
	}{
		{"batch", batchStatusCond, []string{"PRINTED", StatusTokenFilesReady, "CUT", StatusTokenHandedOver}},
		{"item", itemStatusCond, []string{"PRINTED", StatusTokenFilesReady, StatusTokenPacked, "QC_PASSED", StatusTokenHandedOver}},
	} {
		sql, args, ok := tc.fn(tc.toks)
		if !ok {
			t.Fatalf("%s: ok=false", tc.name)
		}
		if !strings.HasPrefix(sql, "((") || !strings.HasSuffix(sql, "))") {
			t.Errorf("%s: điều kiện không bọc ngoặc: %s", tc.name, sql)
		}
		if q := strings.Count(sql, "?"); q != len(args) {
			t.Errorf("%s: %d dấu ? nhưng %d tham số", tc.name, q, len(args))
		}
		if _, _, ok := tc.fn([]string{" ", ""}); ok {
			t.Errorf("%s: toàn ô trống phải là không lọc", tc.name)
		}
	}
}
