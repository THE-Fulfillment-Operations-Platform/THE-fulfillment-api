package services

import (
	"fmt"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// Định mức sản xuất nằm ở CẶP (SKU, NVL), không phải ở NVL: cùng một tấm mica,
// một SKU khay nhỏ ra 10 sản phẩm còn một SKU khay lớn chỉ ra 4. Vì thế một
// batch không thể cộng số sản phẩm đơn thuần — nó cộng MỨC CHIẾM DỤNG của từng
// sản phẩm trên một đơn vị NVL (1/10 tấm so với 1/4 tấm), và một batch đầy khi
// tổng chiếm dụng chạm đúng một đơn vị.

// quotaFixture dựng NVL + các SKU của nó. skuQuotas[i] là định mức của cặp
// (SKU thứ i, NVL này); 0 nghĩa là cặp đó không khai định mức riêng (rơi về
// định mức cấp NVL).
type quotaFixture struct {
	db       *gorm.DB
	svc      *BatchService
	material *models.Material
	skus     []*models.SKU
	order    *models.Order
	nextItem int
}

func newQuotaFixture(t *testing.T, materialQuota int, skuQuotas ...int) *quotaFixture {
	t.Helper()
	db := newSplitDB(t)
	f := &quotaFixture{db: db, svc: newBatchService(db)}

	f.material = &models.Material{Code: "MICA", Name: "Mica"}
	if materialQuota > 0 {
		q := materialQuota
		f.material.ProductsPerUnit = &q
	}
	if err := db.Create(f.material).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	for i, quota := range skuQuotas {
		sku := &models.SKU{Code: fmt.Sprintf("MICA-%02d", i+1), Name: fmt.Sprintf("Mica %d", i+1)}
		if err := db.Create(sku).Error; err != nil {
			t.Fatalf("seed sku: %v", err)
		}
		link := &models.SKUMaterial{SKUID: sku.ID, MaterialID: f.material.ID, QuantityPerUnit: 1}
		if quota > 0 {
			q := quota
			link.ProductsPerUnit = &q
		}
		if err := db.Create(link).Error; err != nil {
			t.Fatalf("seed sku-material: %v", err)
		}
		f.skus = append(f.skus, sku)
	}
	f.order = &models.Order{
		InternalCode: "100001", StoreOrderID: "Etsy-1", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(f.order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return f
}

// add tạo n sản phẩm (mỗi dòng quantity=qty) của SKU thứ skuIdx.
func (f *quotaFixture) add(t *testing.T, skuIdx, n, qty int) []uint {
	t.Helper()
	ids := make([]uint, 0, n)
	for i := 0; i < n; i++ {
		f.nextItem++
		sku := f.skus[skuIdx]
		it := &models.OrderItem{
			OrderID: f.order.ID, InternalCode: fmt.Sprintf("100001_%d", f.nextItem),
			SKUID: &sku.ID, SKUCode: sku.Code, Quantity: qty,
			InternalStatus: models.StatusPending, DesignStatus: models.DesignReady,
		}
		if err := f.db.Create(it).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
		ids = append(ids, it.ID)
	}
	return ids
}

// groupSizes tạo batch từ mọi item của NVL và trả về số item của từng nhóm
// (batch phẳng = một nhóm; batch mẹ = các con theo thứ tự).
func (f *quotaFixture) groupSizes(t *testing.T, ids []uint) []int {
	t.Helper()
	batch, skipped, err := f.svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{
		MaterialID: f.material.ID, OrderItemIDs: ids,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("want nothing skipped, got %v", skipped)
	}
	if !batch.IsParent {
		return []int{len(batch.Items)}
	}
	sizes := make([]int, 0, len(batch.ChildBatches))
	for _, c := range batch.ChildBatches {
		sizes = append(sizes, len(c.Items))
	}
	return sizes
}

// TestBatchQuota_PerSKUMaterialBeatsMaterialDefault: cùng một NVL (định mức
// chung 20), SKU khai riêng 4 sp/tấm thì 5 sản phẩm phải chẻ thành 2 tấm — định
// mức của cặp (SKU, NVL) thắng định mức cấp NVL.
func TestBatchQuota_PerSKUMaterialBeatsMaterialDefault(t *testing.T) {
	f := newQuotaFixture(t, 20, 4)
	ids := f.add(t, 0, 5, 1)

	got := f.groupSizes(t, ids)
	if len(got) != 2 || got[0] != 4 || got[1] != 1 {
		t.Fatalf("want groups [4 1] theo định mức của cặp SKU×NVL, got %v", got)
	}
}

// TestBatchQuota_FallsBackToMaterialQuota: dữ liệu cũ chưa khai định mức trên
// cặp SKU×NVL vẫn chạy y như trước bằng định mức cấp NVL.
func TestBatchQuota_FallsBackToMaterialQuota(t *testing.T) {
	f := newQuotaFixture(t, 20, 0) // SKU không khai định mức riêng
	ids := f.add(t, 0, 25, 1)

	got := f.groupSizes(t, ids)
	if len(got) != 2 || got[0] != 20 || got[1] != 5 {
		t.Fatalf("want fallback về định mức NVL → [20 5], got %v", got)
	}
}

// TestBatchQuota_NoQuotaAnywhere_SingleGroup: không nơi nào khai định mức →
// không chẻ, đúng hành vi cũ.
func TestBatchQuota_NoQuotaAnywhere_SingleGroup(t *testing.T) {
	f := newQuotaFixture(t, 0, 0)
	ids := f.add(t, 0, 30, 1)

	got := f.groupSizes(t, ids)
	if len(got) != 1 || got[0] != 30 {
		t.Fatalf("không định mức → một batch phẳng 30 item, got %v", got)
	}
}

// TestBatchQuota_MixedSKUsShareOneUnit: HAI SKU cùng NVL nhưng khác định mức.
// SKU A 10 sp/tấm (mỗi sp = 1/10 tấm), SKU B 4 sp/tấm (mỗi sp = 1/4 tấm).
// 5×A + 2×B = 1/2 + 1/2 = đúng 1 tấm → một batch. Cộng số sản phẩm đơn thuần
// (7 sản phẩm) sẽ nói sai hoàn toàn.
func TestBatchQuota_MixedSKUsShareOneUnit(t *testing.T) {
	f := newQuotaFixture(t, 0, 10, 4)
	ids := append(f.add(t, 0, 5, 1), f.add(t, 1, 2, 1)...)

	got := f.groupSizes(t, ids)
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("5×(1/10) + 2×(1/4) = đúng 1 tấm → một batch 7 item, got %v", got)
	}
}

// TestBatchQuota_MixedSKUsOverflowToSecondUnit: thêm một sản phẩm SKU B nữa là
// vượt một tấm → sản phẩm đó mở tấm thứ hai.
func TestBatchQuota_MixedSKUsOverflowToSecondUnit(t *testing.T) {
	f := newQuotaFixture(t, 0, 10, 4)
	ids := append(f.add(t, 0, 5, 1), f.add(t, 1, 3, 1)...)

	got := f.groupSizes(t, ids)
	if len(got) != 2 || got[0] != 7 || got[1] != 1 {
		t.Fatalf("1/2 + 3/4 > 1 tấm → [7 1], got %v", got)
	}
}

// TestBatchQuota_ExactBoundaryIsNotSplit: biên chính xác — 3 sản phẩm của SKU
// định mức 3 là đúng một tấm, không phải một tấm cộng phần dư. Đây là chỗ số
// thực (1/3+1/3+1/3 = 0.9999… hoặc 1.0000…2) đẻ ra batch thừa.
func TestBatchQuota_ExactBoundaryIsNotSplit(t *testing.T) {
	f := newQuotaFixture(t, 0, 3)
	ids := f.add(t, 0, 3, 1)
	if got := f.groupSizes(t, ids); len(got) != 1 || got[0] != 3 {
		t.Fatalf("3 sp trên định mức 3 = đúng 1 tấm, got %v", got)
	}

	f2 := newQuotaFixture(t, 0, 3)
	ids2 := f2.add(t, 0, 4, 1)
	if got := f2.groupSizes(t, ids2); len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Fatalf("4 sp trên định mức 3 → [3 1], got %v", got)
	}
}

// TestBatchQuota_QuantityCountsAsProducts: một dòng đơn 3 sản phẩm chiếm 3 suất
// trên tấm, không phải một suất.
func TestBatchQuota_QuantityCountsAsProducts(t *testing.T) {
	f := newQuotaFixture(t, 0, 4)
	ids := f.add(t, 0, 3, 2) // 3 dòng × SL 2 = 6 sản phẩm, định mức 4/tấm

	got := f.groupSizes(t, ids)
	// Dòng 1+2 = 4 sản phẩm = đúng một tấm; dòng 3 sang tấm mới.
	if len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("want [2 1] (theo sản phẩm, không theo số dòng), got %v", got)
	}
}

// TestBatchQuota_OversizeLineStandsAlone: một DÒNG ĐƠN tự nó đã vượt định mức
// không bị cắt đôi (giả định hiện hành: không tách một OrderItem sang nhiều
// batch) — nó chiếm trọn một batch vượt định mức, và người vận hành nhìn thấy.
func TestBatchQuota_OversizeLineStandsAlone(t *testing.T) {
	f := newQuotaFixture(t, 0, 4)
	ids := append(f.add(t, 0, 1, 1), f.add(t, 0, 1, 9)...)
	ids = append(ids, f.add(t, 0, 1, 1)...)

	got := f.groupSizes(t, ids)
	if len(got) != 3 || got[1] != 1 {
		t.Fatalf("dòng vượt định mức phải đứng riêng → [1 1 1], got %v", got)
	}
}

// TestBatchQuota_MultiMaterialSKU_OnePartPerMaterial: SKU ba NVL — auto-create
// phải sinh ba batch, mỗi NVL một batch, và sản phẩm có ĐÚNG một phần sản xuất
// trong mỗi batch: không mất, không nhân đôi. Mỗi cặp có định mức riêng nên số
// batch con của từng NVL cũng khác nhau.
func TestBatchQuota_MultiMaterialSKU_OnePartPerMaterial(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)

	// Ba NVL, không NVL nào khai định mức chung → chỉ cặp SKU×NVL quyết định.
	quotas := []int{2, 3, 0} // NVL 1: 2 sp/tấm, NVL 2: 3 sp/tấm, NVL 3: không giới hạn
	mats := make([]*models.Material, 0, 3)
	sku := &models.SKU{Code: "COMBO-3", Name: "Combo 3 NVL", IsCombo: true}
	if err := db.Create(sku).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}
	for i, q := range quotas {
		m := &models.Material{Code: fmt.Sprintf("MAT-%d", i+1), Name: fmt.Sprintf("NVL %d", i+1)}
		if err := db.Create(m).Error; err != nil {
			t.Fatalf("seed material: %v", err)
		}
		mats = append(mats, m)
		link := &models.SKUMaterial{SKUID: sku.ID, MaterialID: m.ID, QuantityPerUnit: 1}
		if q > 0 {
			qq := q
			link.ProductsPerUnit = &qq
		}
		if err := db.Create(link).Error; err != nil {
			t.Fatalf("seed sku-material: %v", err)
		}
	}
	order := &models.Order{
		InternalCode: "100001", StoreOrderID: "Etsy-1", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	itemIDs := make([]uint, 0, 6)
	for i := 0; i < 6; i++ {
		it := &models.OrderItem{
			OrderID: order.ID, InternalCode: fmt.Sprintf("100001_%d", i+1),
			SKUID: &sku.ID, SKUCode: sku.Code, Quantity: 1,
			InternalStatus: models.StatusPending, DesignStatus: models.DesignReady,
		}
		if err := db.Create(it).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
		itemIDs = append(itemIDs, it.ID)
	}

	res, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("auto create: %v", err)
	}
	if len(res.Created) != 3 {
		t.Fatalf("want một cụm batch cho mỗi NVL, got %+v", res.Created)
	}
	wantChildren := map[uint]int{
		mats[0].ID: 3, // 6 sp / 2 mỗi tấm
		mats[1].ID: 2, // 6 sp / 3 mỗi tấm
		mats[2].ID: 1, // không giới hạn → một batch phẳng
	}
	for _, c := range res.Created {
		if want := wantChildren[c.MaterialID]; len(c.BatchCodes) != want {
			t.Fatalf("NVL %s: want %d batch sản xuất, got %d (%+v)", c.MaterialCode, want, len(c.BatchCodes), c.BatchCodes)
		}
	}

	// Mỗi (sản phẩm, NVL) đúng một phần sản xuất còn sống — không mất, không đôi.
	for _, itemID := range itemIDs {
		for _, m := range mats {
			var n int64
			db.Model(&models.BatchItem{}).
				Where("order_item_id = ? AND material_id = ? AND scrapped_at IS NULL", itemID, m.ID).
				Count(&n)
			if n != 1 {
				t.Fatalf("sản phẩm %d trên NVL %s: want đúng 1 phần, got %d", itemID, m.Code, n)
			}
		}
	}
	// Không batch nào trộn NVL.
	var parts []models.BatchItem
	if err := db.Find(&parts).Error; err != nil {
		t.Fatalf("load parts: %v", err)
	}
	if len(parts) != 18 { // 6 sản phẩm × 3 NVL
		t.Fatalf("want 18 phần sản xuất, got %d", len(parts))
	}
	for _, p := range parts {
		var b models.Batch
		if err := db.First(&b, p.BatchID).Error; err != nil {
			t.Fatalf("load batch: %v", err)
		}
		if b.MaterialID != p.MaterialID {
			t.Fatalf("batch %s (NVL %d) chứa phần của NVL %d", b.Code, b.MaterialID, p.MaterialID)
		}
	}

	// Chạy lại: pool đã sạch → không tạo batch trùng.
	again, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(again.Created) != 0 {
		t.Fatalf("chạy lại không được tạo batch trùng, got %+v", again.Created)
	}
	var partsAfter int64
	db.Model(&models.BatchItem{}).Count(&partsAfter)
	if partsAfter != 18 {
		t.Fatalf("chạy lại đã nhân đôi phần sản xuất: %d → %d", 18, partsAfter)
	}
}

// TestBatchQuota_TwoMaterialSKU_DifferentQuotas: SKU hai NVL, mỗi NVL định mức
// khác nhau → số batch con của mỗi bên khác nhau, sản phẩm vẫn đủ ở cả hai.
func TestBatchQuota_TwoMaterialSKU_DifferentQuotas(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)

	sku := &models.SKU{Code: "COMBO-2", Name: "Combo 2 NVL", IsCombo: true}
	if err := db.Create(sku).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}
	mats := make([]*models.Material, 0, 2)
	for i, q := range []int{5, 2} {
		m := &models.Material{Code: fmt.Sprintf("M%d", i+1), Name: fmt.Sprintf("NVL %d", i+1)}
		if err := db.Create(m).Error; err != nil {
			t.Fatalf("seed material: %v", err)
		}
		mats = append(mats, m)
		qq := q
		if err := db.Create(&models.SKUMaterial{
			SKUID: sku.ID, MaterialID: m.ID, QuantityPerUnit: 1, ProductsPerUnit: &qq,
		}).Error; err != nil {
			t.Fatalf("seed sku-material: %v", err)
		}
	}
	order := &models.Order{
		InternalCode: "100001", StoreOrderID: "Etsy-1", SellerID: 1, ReviewStatus: models.ReviewApproved,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := db.Create(&models.OrderItem{
			OrderID: order.ID, InternalCode: fmt.Sprintf("100001_%d", i+1),
			SKUID: &sku.ID, SKUCode: sku.Code, Quantity: 1,
			InternalStatus: models.StatusPending, DesignStatus: models.DesignReady,
		}).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}

	res, err := svc.AutoCreateBatches(Actor{ID: 1, Role: models.RoleOps})
	if err != nil {
		t.Fatalf("auto create: %v", err)
	}
	byMat := map[uint]AutoCreatedBatch{}
	for _, c := range res.Created {
		byMat[c.MaterialID] = c
	}
	if got := len(byMat[mats[0].ID].BatchCodes); got != 2 { // 10 sp / 5
		t.Fatalf("NVL 1 (định mức 5): want 2 batch, got %d", got)
	}
	if got := len(byMat[mats[1].ID].BatchCodes); got != 5 { // 10 sp / 2
		t.Fatalf("NVL 2 (định mức 2): want 5 batch, got %d", got)
	}
	for _, m := range mats {
		var n int64
		db.Model(&models.BatchItem{}).Where("material_id = ?", m.ID).Count(&n)
		if n != 10 {
			t.Fatalf("NVL %s: want 10 phần sản xuất, got %d", m.Code, n)
		}
	}
}

// TestResolveProductionQuota_Precedence: đơn vị hoá luật ưu tiên — cặp
// SKU×NVL thắng; thiếu thì rơi về NVL; không có gì thì không giới hạn.
func TestResolveProductionQuota_Precedence(t *testing.T) {
	matQuota := 20
	material := &models.Material{Base: models.Base{ID: 7}, Code: "MICA", ProductsPerUnit: &matQuota}
	pairQuota := 4
	sku := &models.SKU{Base: models.Base{ID: 3}, Materials: []models.SKUMaterial{
		{SKUID: 3, MaterialID: 7, ProductsPerUnit: &pairQuota},
		{SKUID: 3, MaterialID: 9},
	}}

	if got := resolveProductionQuota(sku, material); got != 4 {
		t.Fatalf("cặp SKU×NVL phải thắng: got %d, want 4", got)
	}
	other := &models.Material{Base: models.Base{ID: 9}, Code: "WOOD", ProductsPerUnit: &matQuota}
	if got := resolveProductionQuota(sku, other); got != 20 {
		t.Fatalf("cặp không khai định mức → fallback NVL: got %d, want 20", got)
	}
	if got := resolveProductionQuota(nil, &models.Material{Base: models.Base{ID: 9}}); got != 0 {
		t.Fatalf("không định mức nào → 0 (không giới hạn), got %d", got)
	}
}
