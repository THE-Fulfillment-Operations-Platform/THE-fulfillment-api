package repositories

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// Màn Đơn hàng/Sản phẩm, ô "Trạng thái nội bộ" (anh Kiên 19/11, Nghĩa #6360):
// "Đã có file SX" = sản phẩm Chờ xử lý mà mọi phần sản xuất còn sống đã nằm trong
// batch có đủ link in + cắt (và phủ đủ NVL của SKU). "Chờ xử lý" (?files=missing)
// là phần PENDING còn lại. Hai nhóm cộng lại đúng bằng PENDING, tổng đếm khớp phân
// trang, và cờ production_files_ready của từng dòng trùng bộ lọc.
// Chạy trên SQLite (TestItemList_FilesFilter) và Postgres thật
// (TestItemList_FilesFilterPostgres, cần FFM_PG_TEST_DSN).
func checkItemListFilesFilter(t *testing.T, db *gorm.DB) {
	t.Helper()
	matA := &models.Material{Code: "MA", Name: "Gỗ A"}
	matB := &models.Material{Code: "MB", Name: "Gỗ B"}
	for _, m := range []*models.Material{matA, matB} {
		if err := db.Create(m).Error; err != nil {
			t.Fatalf("material: %v", err)
		}
	}
	combo := &models.SKU{Code: "COMBO-AB", Name: "Combo 2 NVL"}
	if err := db.Create(combo).Error; err != nil {
		t.Fatalf("sku: %v", err)
	}
	for _, m := range []*models.Material{matA, matB} {
		if err := db.Create(&models.SKUMaterial{SKUID: combo.ID, MaterialID: m.ID}).Error; err != nil {
			t.Fatalf("sku material: %v", err)
		}
	}

	n := 0
	mkBatch := func(mat *models.Material, links ...models.BatchLinkKind) *models.Batch {
		n++
		b := &models.Batch{Code: "#F" + string(rune('A'+n)), MaterialID: mat.ID, Status: models.StatusPending}
		if err := db.Create(b).Error; err != nil {
			t.Fatalf("batch: %v", err)
		}
		for _, k := range links {
			l := &models.BatchLink{BatchID: b.ID, Kind: k, URL: "https://files/" + b.Code + "/" + string(k), LinkUpdatedAt: time.Now()}
			if err := db.Create(l).Error; err != nil {
				t.Fatalf("link: %v", err)
			}
		}
		return b
	}
	part := func(b *models.Batch, it *models.OrderItem, attempt int, scrapped bool) {
		bi := &models.BatchItem{BatchID: b.ID, OrderItemID: it.ID, MaterialID: b.MaterialID, Status: models.StatusPending, Attempt: attempt}
		if scrapped {
			now := time.Now()
			bi.ScrappedAt = &now
		}
		if err := db.Create(bi).Error; err != nil {
			t.Fatalf("batch item: %v", err)
		}
	}
	m := 0
	item := func() *models.OrderItem {
		m++
		return seedQueueItem(t, db, "30000"+string(rune('a'+m))+"_1/1", models.DesignReady)
	}
	both := []models.BatchLinkKind{models.BatchLinkPrint, models.BatchLinkCut}

	ready := item() // batch có đủ in + cắt
	part(mkBatch(matA, both...), ready, 1, false)

	printOnly := item() // mới có file in
	part(mkBatch(matA, models.BatchLinkPrint), printOnly, 1, false)

	unbatched := item() // chưa vào batch nào

	deletedCut := item() // link cắt đã bị xoá mềm
	bDel := mkBatch(matA, both...)
	part(bDel, deletedCut, 1, false)
	if err := db.Where("batch_id = ? AND kind = ?", bDel.ID, models.BatchLinkCut).Delete(&models.BatchLink{}).Error; err != nil {
		t.Fatal(err)
	}

	// Phần cũ bị huỷ ở QC (batch không có file) không kéo sản phẩm xuống: phần còn
	// sống ở batch mới đã đủ file.
	reworkReady := item()
	part(mkBatch(matA), reworkReady, 1, true)
	part(mkBatch(matA, both...), reworkReady, 2, false)

	// Chỉ còn phần đã huỷ, chưa gom lại: chưa có file.
	reworkWaiting := item()
	part(mkBatch(matA, both...), reworkWaiting, 1, true)

	// SKU 2 NVL: mới gom tấm A (đủ file) thì chưa đủ; tấm B chưa có file cũng chưa đủ;
	// cả hai tấm đủ file mới là "Đã có file SX".
	comboHalf := item()
	comboNoFileB := item()
	comboReady := item()
	for _, it := range []*models.OrderItem{comboHalf, comboNoFileB, comboReady} {
		if err := db.Model(it).Update("sku_id", combo.ID).Error; err != nil {
			t.Fatal(err)
		}
	}
	part(mkBatch(matA, both...), comboHalf, 1, false)
	part(mkBatch(matA, both...), comboNoFileB, 1, false)
	part(mkBatch(matB, models.BatchLinkPrint), comboNoFileB, 1, false)
	part(mkBatch(matA, both...), comboReady, 1, false)
	part(mkBatch(matB, both...), comboReady, 1, false)

	// Đã in: có file nhưng không thuộc "Chờ xử lý" nên không ra ở cả hai nhóm.
	printed := item()
	part(mkBatch(matA, both...), printed, 1, false)
	if err := db.Model(printed).Update("internal_status", models.StatusPrinted).Error; err != nil {
		t.Fatal(err)
	}

	// Dòng đã huỷ: không phải việc, không ra ở danh sách mặc định.
	cancelled := item()
	part(mkBatch(matA, both...), cancelled, 1, false)
	if err := db.Model(cancelled).Update("cancellation_status", models.CancellationSeller).Error; err != nil {
		t.Fatal(err)
	}

	repo := &OrderItemRepository{db: db}
	list := func(f ItemFilter, size, page int) (map[uint]bool, int64, int) {
		f.Page = Page{Page: page, PageSize: size}
		rows, total, err := repo.List(f)
		if err != nil {
			t.Fatalf("List %+v: %v", f, err)
		}
		got := map[uint]bool{}
		for _, r := range rows {
			got[r.ID] = true
		}
		return got, total, len(rows)
	}
	same := func(name string, got map[uint]bool, want ...*models.OrderItem) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %d dòng %v, want %d", name, len(got), got, len(want))
		}
		for _, w := range want {
			if !got[w.ID] {
				t.Fatalf("%s: thiếu dòng %s", name, w.InternalCode)
			}
		}
	}

	wantReady := []*models.OrderItem{ready, reworkReady, comboReady}
	wantMissing := []*models.OrderItem{printOnly, unbatched, deletedCut, reworkWaiting, comboHalf, comboNoFileB}

	got, total, _ := list(ItemFilter{Files: "ready"}, 50, 1)
	same("Đã có file SX", got, wantReady...)
	if total != int64(len(wantReady)) {
		t.Fatalf("Đã có file SX: total %d", total)
	}
	// Web gửi kèm status=PENDING: kết quả y hệt.
	got, _, _ = list(ItemFilter{InternalStatus: string(models.StatusPending), Files: "ready"}, 50, 1)
	same("Đã có file SX + status", got, wantReady...)

	got, total, _ = list(ItemFilter{InternalStatus: string(models.StatusPending), Files: "missing"}, 50, 1)
	same("Chờ xử lý (chưa có file)", got, wantMissing...)
	if total != int64(len(wantMissing)) {
		t.Fatalf("Chờ xử lý: total %d", total)
	}

	// Hai nhóm cộng lại đúng bằng PENDING (dashboard vẫn đếm chung).
	_, pending, _ := list(ItemFilter{InternalStatus: string(models.StatusPending)}, 50, 1)
	if pending != int64(len(wantReady)+len(wantMissing)) {
		t.Fatalf("PENDING = %d, want %d", pending, len(wantReady)+len(wantMissing))
	}

	// Phân trang: tổng giữ nguyên, trang 2 còn phần dư.
	_, t1, n1 := list(ItemFilter{Files: "ready"}, 2, 1)
	_, t2, n2 := list(ItemFilter{Files: "ready"}, 2, 2)
	if t1 != 3 || t2 != 3 || n1 != 2 || n2 != 1 {
		t.Fatalf("phân trang sai: t1=%d t2=%d n1=%d n2=%d", t1, t2, n1, n2)
	}

	// Giá trị lạ = không lọc (handler đã chặn, repo cũng không lọc bừa).
	_, every, _ := list(ItemFilter{Files: "bogus"}, 50, 1)
	if every != int64(len(wantReady)+len(wantMissing)+1) { // + printed
		t.Fatalf("không lọc phải thấy %d dòng, got %d", len(wantReady)+len(wantMissing)+1, every)
	}

	// Cờ từng dòng = cùng điều kiện: đúng các dòng "Đã có file SX" + dòng Đã in
	// (đã có file, chỉ là qua khâu rồi). Dòng đã huỷ vẫn tính theo dữ liệu.
	all := []*models.OrderItem{ready, printOnly, unbatched, deletedCut, reworkReady, reworkWaiting, comboHalf, comboNoFileB, comboReady, printed, cancelled}
	ids := make([]uint, len(all))
	for i, it := range all {
		ids[i] = it.ID
	}
	flags, err := repo.FilesReadyIDs(ids)
	if err != nil {
		t.Fatalf("FilesReadyIDs: %v", err)
	}
	wantFlag := map[uint]bool{ready.ID: true, reworkReady.ID: true, comboReady.ID: true, printed.ID: true, cancelled.ID: true}
	for _, it := range all {
		if flags[it.ID] != wantFlag[it.ID] {
			t.Fatalf("cờ file SX của %s = %v, want %v", it.InternalCode, flags[it.ID], wantFlag[it.ID])
		}
	}
	if empty, err := repo.FilesReadyIDs(nil); err != nil || len(empty) != 0 {
		t.Fatalf("FilesReadyIDs(nil) = %v, %v", empty, err)
	}
}

func TestItemList_FilesFilter(t *testing.T) {
	checkItemListFilesFilter(t, newQueueTestDB(t))
}

func TestItemList_FilesFilterPostgres(t *testing.T) {
	db := openHandoverPG(t)
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{},
		&models.Batch{}, &models.BatchItem{}, &models.BatchLink{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&models.Seller{Code: "S1", Name: "S1"}).Error; err != nil {
		t.Fatalf("seller: %v", err)
	}
	checkItemListFilesFilter(t, db)
}
