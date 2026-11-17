package services

import (
	"sort"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Anh Kiên 17/11/2026: "a sửa lại nguyên vật liệu từ 5 ly thành 3 ly, thế là nó
// tự sinh ra 1 loạt đơn chưa gắn Batch" — màn Tạo batch hiện "Basswood 3 ly · 81
// item sẵn sàng". Hàng chờ gom batch đọc cặp (sản phẩm, NVL) từ NVL HIỆN TẠI của
// SKU, và chỉ hỏi "đã có batch của NVL này chưa". Đổi 5 ly → 3 ly thì mọi sản phẩm
// của SKU, kể cả đã làm xong trên 5 ly và đã gửi đi, đều chưa có batch 3 ly nên
// hiện lại là "sẵn sàng". Các test dưới đây khoá hành vi đúng.

type remapFixture struct {
	db    *gorm.DB
	repo  *repositories.Repositories
	batch *BatchService
	qc    *QCService
	m5    *models.Material
	m3    *models.Material
	sku   *models.SKU
	seq   int
}

func newRemapFixture(t *testing.T) *remapFixture {
	t.Helper()
	db := newQCDB(t)
	repo := repositories.New(db)
	f := &remapFixture{
		db: db, repo: repo,
		batch: &BatchService{repo: repo, audit: &AuditService{repo: repo}},
		qc:    &QCService{repo: repo, audit: &AuditService{repo: repo}},
		m5:    &models.Material{Code: "BASSWOOD-5-LY", Name: "Basswood 5 ly"},
		m3:    &models.Material{Code: "BASSWOOD-3-LY", Name: "Basswood 3 ly"},
		sku:   &models.SKU{Code: "DEN-NUI", Name: "Đèn núi"},
	}
	for _, v := range []any{f.m5, f.m3, f.sku} {
		if err := db.Create(v).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	f.mapSKU(t, f.m5.ID)
	return f
}

// mapSKU sets the SKU's materials through the same repository call the SKU form
// ("Lưu mapping") uses: old pairs are hard-deleted, the new ones inserted.
func (f *remapFixture) mapSKU(t *testing.T, materialIDs ...uint) {
	t.Helper()
	mats := make([]models.SKUMaterial, 0, len(materialIDs))
	for _, id := range materialIDs {
		mats = append(mats, models.SKUMaterial{MaterialID: id, QuantityPerUnit: 1})
	}
	if err := f.repo.SKU.ReplaceMaterials(f.sku.ID, mats); err != nil {
		t.Fatalf("map SKU: %v", err)
	}
}

// item seeds an approved order with one line on the fixture's SKU.
func (f *remapFixture) item(t *testing.T) *models.OrderItem {
	t.Helper()
	f.seq++
	code := "1020" + string(rune('0'+f.seq/10)) + string(rune('0'+f.seq%10))
	order := &models.Order{
		InternalCode: code, StoreOrderID: "Etsy-" + code, StoreOrderRef: "Etsy-" + code, SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	if err := f.db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	it := &models.OrderItem{
		OrderID: order.ID, LineNo: 1, InternalCode: code + "_1/1",
		SKUID: &f.sku.ID, SKUCode: f.sku.Code, Quantity: 1,
		MockupURL: "https://example.com/m.png", InternalStatus: models.StatusPending,
	}
	if err := f.db.Create(it).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	return it
}

// batchOn puts the items into one new batch of the material through the real
// BatchService.Create and returns that batch.
func (f *remapFixture) batchOn(t *testing.T, materialID uint, items ...*models.OrderItem) *models.Batch {
	t.Helper()
	ids := make([]uint, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	created, skipped, err := f.batch.Create(Actor{ID: 1, Role: models.RoleOwner}, CreateBatchInput{MaterialID: materialID, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	if len(skipped) != 0 || len(created) != 1 {
		t.Fatalf("create batch: want 1 batch and nothing skipped, got %d batch, skipped %v", len(created), skipped)
	}
	return created[0]
}

func (f *remapFixture) setParts(t *testing.T, item *models.OrderItem, status models.InternalStatus) {
	t.Helper()
	if err := f.db.Model(&models.BatchItem{}).Where("order_item_id = ?", item.ID).Update("status", status).Error; err != nil {
		t.Fatalf("set parts: %v", err)
	}
	if err := f.db.Model(&models.OrderItem{}).Where("id = ?", item.ID).Update("internal_status", status).Error; err != nil {
		t.Fatalf("set item: %v", err)
	}
}

func (f *remapFixture) setOrder(t *testing.T, item *models.OrderItem, status models.SellerStatus) {
	t.Helper()
	if err := f.db.Model(&models.Order{}).Where("id = ?", item.OrderID).Update("seller_status", status).Error; err != nil {
		t.Fatalf("set order: %v", err)
	}
}

// pool returns the ids the create-batch screen lists for a material, sorted.
func (f *remapFixture) pool(t *testing.T, materialID uint) []uint {
	t.Helper()
	rows, total, err := f.repo.OrderItem.DesignReadyItemsForMaterial(materialID, repositories.Page{Page: 1, PageSize: 100}, "", "")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if int(total) != len(rows) {
		t.Fatalf("pool: total %d but %d rows", total, len(rows))
	}
	ids := make([]uint, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func remapIDs(items ...*models.OrderItem) []uint {
	ids := make([]uint, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func sameUintSet(a, b []uint) bool {
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

func TestCreateBatchPool_SKUMaterialChangeDoesNotReopenBatchedItems(t *testing.T) {
	f := newRemapFixture(t)

	// Trạng thái trước khi anh đổi NVL, tất cả trên Basswood 5 ly:
	inBatch := f.item(t) // đang nằm trong batch 5 ly chưa in
	f.batchOn(t, f.m5.ID, inBatch)
	shipped := f.item(t) // đã làm xong trên 5 ly và đã gửi đi
	f.batchOn(t, f.m5.ID, shipped)
	f.setParts(t, shipped, models.StatusQCPassed)
	f.setOrder(t, shipped, models.SellerStatusShipped)
	scrapped := f.item(t) // QC fail, tấm 5 ly đã ghi bỏ, đang chờ làm lại
	f.batchOn(t, f.m5.ID, scrapped)
	now := time.Now()
	if err := f.db.Model(&models.BatchItem{}).Where("order_item_id = ?", scrapped.ID).Update("scrapped_at", &now).Error; err != nil {
		t.Fatalf("scrap: %v", err)
	}
	waiting := f.item(t) // đã duyệt, chưa gom batch
	delivered := f.item(t)
	f.setOrder(t, delivered, models.SellerStatusDelivered) // đã giao mà chưa từng có batch (xuất kho vượt)

	if got, want := f.pool(t, f.m5.ID), remapIDs(scrapped, waiting); !sameUintSet(got, want) {
		t.Fatalf("trước khi đổi NVL, hàng chờ 5 ly = %v, want %v", got, want)
	}

	// Anh Kiên sửa NVL của SKU: 5 ly → 3 ly.
	f.mapSKU(t, f.m3.ID)

	// Chỉ hàng THẬT SỰ chưa có batch chuyển sang 3 ly. Trước bản sửa, cả 5 sản phẩm
	// (gồm đơn đang trong batch 5 ly và đơn đã gửi / đã giao) hiện ở đây.
	if got, want := f.pool(t, f.m3.ID), remapIDs(scrapped, waiting); !sameUintSet(got, want) {
		t.Fatalf("sau khi đổi NVL, hàng chờ 3 ly = %v, want %v (chỉ hàng chưa gom + hàng chờ làm lại)", got, want)
	}
	if got := f.pool(t, f.m5.ID); len(got) != 0 {
		t.Fatalf("SKU không còn dùng 5 ly mà hàng chờ 5 ly còn %v", got)
	}
	buckets, err := f.repo.OrderItem.MaterialBuckets()
	if err != nil {
		t.Fatalf("buckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].MaterialID != f.m3.ID || buckets[0].ItemCount != 2 {
		t.Fatalf("thẻ NVL ở màn Tạo batch = %+v, want một thẻ Basswood 3 ly · 2 item", buckets)
	}

	// Gửi thẳng cả 5 sản phẩm vào batch 3 ly (màn cũ chưa tải lại, hoặc gọi API
	// tay): chỉ 2 sản phẩm hợp lệ vào batch, 3 sản phẩm kia bị loại kèm lý do và
	// sản phẩm đã gửi đi giữ nguyên "Đã QC".
	all := []uint{inBatch.ID, shipped.ID, scrapped.ID, waiting.ID, delivered.ID}
	created, skipped, err := f.batch.Create(Actor{ID: 1, Role: models.RoleOwner}, CreateBatchInput{MaterialID: f.m3.ID, OrderItemIDs: all})
	if err != nil {
		t.Fatalf("create 3 ly: %v", err)
	}
	sort.Slice(skipped, func(i, j int) bool { return skipped[i] < skipped[j] })
	if want := remapIDs(inBatch, shipped, delivered); !sameUintSet(skipped, want) {
		t.Fatalf("skipped = %v, want %v", skipped, want)
	}
	var parts []models.BatchItem
	f.db.Where("batch_id = ?", created[0].ID).Find(&parts)
	got := make([]uint, 0, len(parts))
	for _, p := range parts {
		got = append(got, p.OrderItemID)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if want := remapIDs(scrapped, waiting); !sameUintSet(got, want) {
		t.Fatalf("batch 3 ly chứa %v, want %v", got, want)
	}
	var after models.OrderItem
	f.db.First(&after, shipped.ID)
	if after.InternalStatus != models.StatusQCPassed {
		t.Fatalf("sản phẩm đã gửi bị hạ trạng thái xuống %s", after.InternalStatus)
	}
}

// Muốn chuyển hàng đang nằm trong batch 5 ly chưa in sang 3 ly thật: xoá batch cũ,
// sản phẩm quay lại hàng chờ và giờ nằm ở 3 ly.
func TestCreateBatchPool_DeletingOldBatchMovesItemToNewMaterial(t *testing.T) {
	f := newRemapFixture(t)
	it := f.item(t)
	old := f.batchOn(t, f.m5.ID, it)
	f.mapSKU(t, f.m3.ID)
	if got := f.pool(t, f.m3.ID); len(got) != 0 {
		t.Fatalf("còn trong batch 5 ly mà đã hiện ở 3 ly: %v", got)
	}
	if err := f.batch.Delete(Actor{ID: 1, Role: models.RoleOwner}, old.ID); err != nil {
		t.Fatalf("delete old batch: %v", err)
	}
	if got, want := f.pool(t, f.m3.ID), remapIDs(it); !sameUintSet(got, want) {
		t.Fatalf("xoá batch 5 ly rồi, hàng chờ 3 ly = %v, want %v", got, want)
	}
}

// Hàng combo vẫn chạy như cũ: đã có batch NVL A thì vẫn phải gom NVL B.
func TestCreateBatchPool_ComboStillNeedsItsOtherMaterial(t *testing.T) {
	f := newRemapFixture(t)
	f.mapSKU(t, f.m5.ID, f.m3.ID) // combo 5 ly + 3 ly
	half := f.item(t)
	f.batchOn(t, f.m5.ID, half)
	if got, want := f.pool(t, f.m3.ID), remapIDs(half); !sameUintSet(got, want) {
		t.Fatalf("combo đã có 5 ly, hàng chờ 3 ly = %v, want %v", got, want)
	}
	if got := f.pool(t, f.m5.ID); len(got) != 0 {
		t.Fatalf("combo đã có batch 5 ly mà vẫn hiện ở 5 ly: %v", got)
	}

	// Combo đã đủ hai NVL rồi SKU đổi một NVL (5 ly → NVL khác): không mở lại.
	f.batchOn(t, f.m3.ID, half)
	other := &models.Material{Code: "MICA-3-LY", Name: "Mica 3 ly"}
	if err := f.db.Create(other).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	f.mapSKU(t, other.ID, f.m3.ID)
	if got := f.pool(t, other.ID); len(got) != 0 {
		t.Fatalf("combo đã đủ phần mà hiện lại ở NVL mới: %v", got)
	}
}

// Hàng đã cắt xong trên 5 ly rồi SKU mới đổi sang 3 ly: QC phải chấm được theo
// phần 5 ly nó đang có. Không thì nó kẹt: QC đòi phần 3 ly, mà hàng chờ gom batch
// (đúng ra) không đưa nó ra để làm phần 3 ly.
func TestQCPass_SKUMaterialChangedAfterCut(t *testing.T) {
	f := newRemapFixture(t)
	cut := f.item(t)
	f.batchOn(t, f.m5.ID, cut)
	f.setParts(t, cut, models.StatusCut)
	printed := f.item(t)
	f.batchOn(t, f.m5.ID, printed)
	f.setParts(t, printed, models.StatusPrinted)

	f.mapSKU(t, f.m3.ID)

	qcActor := Actor{ID: 5, Role: models.RoleQC}
	if _, err := f.qc.Pass(qcActor, QCDecisionInput{ScanRef: ScanRef{Code: cut.InternalCode}}); err != nil {
		t.Fatalf("đã cắt trên 5 ly mà QC không cho đạt: %v", err)
	}
	var after models.OrderItem
	f.db.First(&after, cut.ID)
	if after.InternalStatus != models.StatusQCPassed {
		t.Fatalf("sau QC đạt, sản phẩm ở %s, want QC_PASSED", after.InternalStatus)
	}

	// Mới in, chưa cắt: vẫn bị chặn như mọi sản phẩm chưa cắt xong.
	_, err := f.qc.Pass(qcActor, QCDecisionInput{ScanRef: ScanRef{Code: printed.InternalCode}})
	if err == nil || !strings.Contains(err.Error(), "chưa cắt xong") {
		t.Fatalf("mới in mà QC không chặn đúng câu: %v", err)
	}
}

// Combo còn thiếu hẳn một NVL thì QC vẫn chặn (không được nới nhầm).
func TestQCPass_ComboMissingMaterialStillBlocked(t *testing.T) {
	f := newRemapFixture(t)
	f.mapSKU(t, f.m5.ID, f.m3.ID)
	half := f.item(t)
	f.batchOn(t, f.m5.ID, half)
	f.setParts(t, half, models.StatusCut)
	_, err := f.qc.Pass(Actor{ID: 5, Role: models.RoleQC}, QCDecisionInput{ScanRef: ScanRef{Code: half.InternalCode}})
	if err == nil || !strings.Contains(err.Error(), "Basswood 3 ly") {
		t.Fatalf("combo thiếu phần 3 ly mà QC không chặn đúng: %v", err)
	}
}

func TestLivePartsCoverSKU(t *testing.T) {
	sku := &models.SKU{Materials: []models.SKUMaterial{{MaterialID: 1}, {MaterialID: 2}}}
	cases := []struct {
		name  string
		sku   *models.SKU
		live  map[uint]bool
		cover bool
		allIn bool
	}{
		{"chưa có phần nào", sku, nil, false, false},
		{"combo mới có A", sku, map[uint]bool{1: true}, false, false},
		{"combo đủ A, B", sku, map[uint]bool{1: true, 2: true}, true, true},
		{"đủ số phần nhưng một phần ở NVL cũ", sku, map[uint]bool{1: true, 9: true}, true, false},
		{"SKU không có NVL", &models.SKU{}, map[uint]bool{1: true}, false, true},
		{"không có SKU", nil, map[uint]bool{1: true}, false, false},
	}
	for _, c := range cases {
		if got := livePartsCoverSKU(c.sku, c.live); got != c.cover {
			t.Errorf("%s: livePartsCoverSKU = %v, want %v", c.name, got, c.cover)
		}
		if got := skuMaterialsAllIn(c.sku, c.live); got != c.allIn {
			t.Errorf("%s: skuMaterialsAllIn = %v, want %v", c.name, got, c.allIn)
		}
	}
}
