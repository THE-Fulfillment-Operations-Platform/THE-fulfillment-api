package services

import (
	"fmt"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Định mức sản xuất nằm ở CẶP (SKU, NVL), không phải ở NVL, và được TÍNH từ
// kích thước: ⌊diện tích tấm / diện tích sản phẩm⌋ (khách chốt 2026-09-18).
// Cùng một tấm mica, một SKU khay nhỏ ra 10 sản phẩm còn một SKU khay lớn chỉ
// ra 4. Một batch là một file in + một file cắt, tức MỘT SKU lặp trên tấm, nên
// (khách chốt 2026-09-24) trong một NVL, item nhóm theo SKU trước, mỗi SKU chẻ
// theo định mức của cặp, SKU khác nhau không bao giờ chung batch — kể cả hai
// SKU con cùng một SKU cha.

// quotaFixture dựng NVL + các SKU của nó. Tấm NVL 60 × 1 mm; skuQuotas[i] là
// định mức mong muốn của SKU thứ i, đạt được bằng cách cho SKU kích thước
// (60/q) × 1 mm — 60 chia hết cho mọi định mức các test dùng. 0 = SKU không khai
// kích thước (không có định mức).
type quotaFixture struct {
	db       *gorm.DB
	svc      *BatchService
	material *models.Material
	skus     []*models.SKU
	order    *models.Order
	nextItem int
}

const fixtureSheetLength = 60.0

func newQuotaFixture(t *testing.T, skuQuotas ...int) *quotaFixture {
	t.Helper()
	sizes := make([]*float64, len(skuQuotas))
	for i, q := range skuQuotas {
		if q > 0 {
			if fixtureSheetLength/float64(q) != float64(int(fixtureSheetLength)/q) {
				t.Fatalf("fixture: định mức %d không chia hết tấm %v", q, fixtureSheetLength)
			}
			l := fixtureSheetLength / float64(q)
			sizes[i] = &l
		}
	}
	one := 1.0
	sheet := fixtureSheetLength
	return newSizedFixture(t, &sheet, &one, sizes...)
}

// newSizedFixture is the explicit form: the sheet's D (R = 1 mm) and each SKU's
// D (R = 1 mm); nil = no size on that side.
func newSizedFixture(t *testing.T, sheetLength, sheetWidth *float64, skuLengths ...*float64) *quotaFixture {
	t.Helper()
	db := newSplitDB(t)
	f := &quotaFixture{db: db, svc: newBatchService(db)}

	f.material = &models.Material{Code: "MICA", Name: "Mica", LengthMM: sheetLength, WidthMM: sheetWidth}
	if err := db.Create(f.material).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	one := 1.0
	for i, l := range skuLengths {
		sku := &models.SKU{Code: fmt.Sprintf("MICA-%02d", i+1), Name: fmt.Sprintf("Mica %d", i+1)}
		if l != nil {
			sku.LengthMM, sku.WidthMM = l, &one
		}
		if err := db.Create(sku).Error; err != nil {
			t.Fatalf("seed sku: %v", err)
		}
		if err := db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: f.material.ID, QuantityPerUnit: 1}).Error; err != nil {
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

// TestBatchQuota_DerivedFromSizes: tấm 60×1, SKU 15×1 → ⌊60/15⌋ = 4 sp/tấm, nên
// 5 sản phẩm phải chẻ thành 2 tấm. Không ai nhập số 4 — nó ra từ hai kích thước.
func TestBatchQuota_DerivedFromSizes(t *testing.T) {
	f := newQuotaFixture(t, 4)
	ids := f.add(t, 0, 5, 1)

	got := f.groupSizes(t, ids)
	if len(got) != 2 || got[0] != 4 || got[1] != 1 {
		t.Fatalf("want groups [4 1] theo ⌊S_tấm/S_sp⌋, got %v", got)
	}
}

// TestBatchQuota_SKUWithoutSize_Unlimited: SKU chưa khai kích thước thì không có
// định mức — không chẻ, không đoán.
func TestBatchQuota_SKUWithoutSize_Unlimited(t *testing.T) {
	f := newQuotaFixture(t, 0)
	ids := f.add(t, 0, 25, 1)

	got := f.groupSizes(t, ids)
	if len(got) != 1 || got[0] != 25 {
		t.Fatalf("SKU không kích thước → một batch phẳng 25 item, got %v", got)
	}
}

// TestBatchQuota_SheetWithoutSize_Unlimited: NVL chưa khai kích thước tấm → SKU
// có kích thước cũng không ra định mức được → không chẻ.
func TestBatchQuota_SheetWithoutSize_Unlimited(t *testing.T) {
	l := 15.0
	f := newSizedFixture(t, nil, nil, &l)
	ids := f.add(t, 0, 30, 1)

	got := f.groupSizes(t, ids)
	if len(got) != 1 || got[0] != 30 {
		t.Fatalf("NVL không kích thước → một batch phẳng 30 item, got %v", got)
	}
}

// TestBatchQuota_MixedSKUsNeverShareABatch: HAI SKU cùng NVL, mỗi SKU vài sản
// phẩm lẻ, đều dưới định mức. Luật cũ (18/09) cho chúng chung một tấm theo mức
// chiếm dụng (5/10 + 2/4 = 1 tấm); luật mới (24/09): mỗi SKU một batch riêng,
// vì một batch là một file cắt của một SKU.
func TestBatchQuota_MixedSKUsNeverShareABatch(t *testing.T) {
	f := newQuotaFixture(t, 10, 4)
	ids := append(f.add(t, 0, 5, 1), f.add(t, 1, 2, 1)...)

	got := f.groupSizes(t, ids)
	if len(got) != 2 || got[0] != 5 || got[1] != 2 {
		t.Fatalf("hai SKU → hai batch riêng [5 2], got %v", got)
	}
}

// TestBatchQuota_GroupsBySKUThenSplitsEach: hai SKU trộn lẫn thứ tự trong pool.
// Nhóm theo SKU theo thứ tự xuất hiện, rồi mỗi nhóm chẻ theo định mức của nó:
// A (định mức 4) 5 sp → [4 1]; B (định mức 3) 3 sp → [3]. Tổng 3 batch con.
func TestBatchQuota_GroupsBySKUThenSplitsEach(t *testing.T) {
	f := newQuotaFixture(t, 4, 3)
	var ids []uint
	ids = append(ids, f.add(t, 0, 2, 1)...)
	ids = append(ids, f.add(t, 1, 3, 1)...)
	ids = append(ids, f.add(t, 0, 3, 1)...)

	got := f.groupSizes(t, ids)
	if len(got) != 3 || got[0] != 4 || got[1] != 1 || got[2] != 3 {
		t.Fatalf("A[4 1] rồi B[3] theo thứ tự xuất hiện, got %v", got)
	}
}

// makeSiblings files the fixture's SKUs under one parent (or a parent each when
// separate=true) — the family the batch splitter groups by.
func (f *quotaFixture) makeSiblings(t *testing.T, separate bool) {
	t.Helper()
	var parent *models.SKU
	for i, sku := range f.skus {
		if parent == nil || separate {
			parent = &models.SKU{Code: fmt.Sprintf("CHA-%d", i+1), Name: fmt.Sprintf("Cha %d", i+1)}
			if err := f.db.Create(parent).Error; err != nil {
				t.Fatalf("seed parent: %v", err)
			}
		}
		if err := f.db.Model(sku).Update("parent_id", parent.ID).Error; err != nil {
			t.Fatalf("set parent: %v", err)
		}
	}
}

// TestBatchQuota_SiblingsTopUpTheSheet: hai SKU con cùng cha, mỗi SKU 3 sp, định
// mức 10/tấm → 3/10 + 3/10 = 0,6 tấm → MỘT batch 6 item (khách chốt 24/09: SKU
// cùng mã cha được nhét vào chỗ trống). Khác cha thì tách (test dưới).
func TestBatchQuota_SiblingsTopUpTheSheet(t *testing.T) {
	f := newQuotaFixture(t, 10, 10)
	f.makeSiblings(t, false)
	ids := append(f.add(t, 0, 3, 1), f.add(t, 1, 3, 1)...)

	got := f.groupSizes(t, ids)
	if len(got) != 1 || got[0] != 6 {
		t.Fatalf("anh em cùng cha, còn chỗ → một batch [6], got %v", got)
	}
}

// TestBatchQuota_SiblingsOverflowOpensNextSheet: A 3 sp + B 3 sp, cả hai định
// mức 4/tấm. A chiếm 3/4, B nhét thêm 1 là đầy tấm; 2 B còn lại sang tấm sau.
func TestBatchQuota_SiblingsOverflowOpensNextSheet(t *testing.T) {
	f := newQuotaFixture(t, 4, 4)
	f.makeSiblings(t, false)
	ids := append(f.add(t, 0, 3, 1), f.add(t, 1, 3, 1)...)

	got := f.groupSizes(t, ids)
	if len(got) != 2 || got[0] != 4 || got[1] != 2 {
		t.Fatalf("3A + 1B đầy tấm, 2B sang tấm sau → [4 2], got %v", got)
	}
}

// TestBatchQuota_DifferentParentsNeverMix: hai SKU thuộc hai SKU cha khác nhau,
// dù còn thừa chỗ vẫn là hai batch — khác dòng sản phẩm là khác file in/cắt.
func TestBatchQuota_DifferentParentsNeverMix(t *testing.T) {
	f := newQuotaFixture(t, 10, 10)
	f.makeSiblings(t, true)
	ids := append(f.add(t, 0, 3, 1), f.add(t, 1, 3, 1)...)

	got := f.groupSizes(t, ids)
	if len(got) != 2 || got[0] != 3 || got[1] != 3 {
		t.Fatalf("khác cha → hai batch [3 3], got %v", got)
	}
}

// TestBatchQuota_OwnSKUFirstThenSiblings: trong pool xen kẽ A2 B3 A3 (A định mức
// 4, B định mức 3, cùng cha), A được gom lại trước: A×4 đầy một tấm; A còn 1
// (1/4) + B 2 (2/3) = 11/12 vừa; B thứ ba sang tấm nữa → [4 3 1].
func TestBatchQuota_OwnSKUFirstThenSiblings(t *testing.T) {
	f := newQuotaFixture(t, 4, 3)
	f.makeSiblings(t, false)
	var ids []uint
	ids = append(ids, f.add(t, 0, 2, 1)...)
	ids = append(ids, f.add(t, 1, 3, 1)...)
	ids = append(ids, f.add(t, 0, 3, 1)...)

	got := f.groupSizes(t, ids)
	if len(got) != 3 || got[0] != 4 || got[1] != 3 || got[2] != 1 {
		t.Fatalf("cùng SKU xếp trước rồi mới nhét anh em → [4 3 1], got %v", got)
	}
}

// TestBatchQuota_SiblingWithoutQuotaStandsAlone: SKU anh em chưa có định mức
// (không kích thước, không khai) không đo được trên tấm → batch riêng, không
// bị nhét vào tấm của SKU khác như thể chiếm 0 chỗ.
func TestBatchQuota_SiblingWithoutQuotaStandsAlone(t *testing.T) {
	f := newQuotaFixture(t, 10, 0)
	f.makeSiblings(t, false)
	ids := append(f.add(t, 0, 3, 1), f.add(t, 1, 3, 1)...)

	got := f.groupSizes(t, ids)
	if len(got) != 2 || got[0] != 3 || got[1] != 3 {
		t.Fatalf("SKU không định mức đứng riêng → [3 3], got %v", got)
	}
}

// TestBatchQuota_DeclaredQuotaBeatsEstimate: kích thước cho ước tính 4/tấm, nhưng
// xưởng khai định mức 2 cho cặp → chia theo 2 (khách chốt 24/09: số khai tay là
// số hệ thống dùng). Số tấm ở màn danh sách/chi tiết cũng phải theo số khai:
// một dòng 3 sp, định mức 1 → 3 tấm, không phải ⌈3/4⌉ = 1.
func TestBatchQuota_DeclaredQuotaBeatsEstimate(t *testing.T) {
	f := newQuotaFixture(t, 4)
	if err := f.db.Model(&models.SKUMaterial{}).Where("sku_id = ? AND material_id = ?", f.skus[0].ID, f.material.ID).
		Update("products_per_unit", 2).Error; err != nil {
		t.Fatalf("declare quota: %v", err)
	}
	ids := f.add(t, 0, 5, 1)
	got := f.groupSizes(t, ids)
	if len(got) != 3 || got[0] != 2 || got[1] != 2 || got[2] != 1 {
		t.Fatalf("định mức khai 2 thắng ước tính 4 → [2 2 1], got %v", got)
	}

	g := newQuotaFixture(t, 4)
	if err := g.db.Model(&models.SKUMaterial{}).Where("sku_id = ? AND material_id = ?", g.skus[0].ID, g.material.ID).
		Update("products_per_unit", 1).Error; err != nil {
		t.Fatalf("declare quota: %v", err)
	}
	flat, _, err := g.svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{
		MaterialID: g.material.ID, OrderItemIDs: g.add(t, 0, 1, 3),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	repo := repositories.New(g.db)
	// The declared quota costs the detail payload exactly ONE extra statement
	// (the pair lookup), on top of TestFindByID_RoundTripBudget's five.
	var detail *models.Batch
	n := countQueries(g.db, func(tx *gorm.DB) {
		var err error
		detail, err = repositories.New(tx).Batch.FindByID(flat.ID)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
	})
	if n > 6 {
		t.Errorf("FindByID with declared quotas sent %d statements, budget is 6", n)
	}
	if detail.MaterialUnits == nil || *detail.MaterialUnits != 3 {
		t.Fatalf("chi tiết: 3 sp / định mức khai 1 → 3 tấm, got %v", detail.MaterialUnits)
	}
	rows, _, err := repo.Batch.List(repositories.BatchFilter{Page: repositories.Page{Page: 1, PageSize: 50}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].MaterialUnits == nil || *rows[0].MaterialUnits != 3 {
		t.Fatalf("danh sách: muốn 3 tấm theo định mức khai, got %+v", rows)
	}
}

// TestBatchQuota_ExactBoundaryIsNotSplit: biên chính xác — 3 sản phẩm của SKU
// định mức 3 là đúng một tấm, không phải một tấm cộng phần dư. Đây là chỗ số
// thực (1/3+1/3+1/3 = 0.9999… hoặc 1.0000…2) đẻ ra batch thừa.
func TestBatchQuota_ExactBoundaryIsNotSplit(t *testing.T) {
	f := newQuotaFixture(t, 3)
	ids := f.add(t, 0, 3, 1)
	if got := f.groupSizes(t, ids); len(got) != 1 || got[0] != 3 {
		t.Fatalf("3 sp trên định mức 3 = đúng 1 tấm, got %v", got)
	}

	f2 := newQuotaFixture(t, 3)
	ids2 := f2.add(t, 0, 4, 1)
	if got := f2.groupSizes(t, ids2); len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Fatalf("4 sp trên định mức 3 → [3 1], got %v", got)
	}
}

// TestBatchQuota_QuantityCountsAsProducts: một dòng đơn 3 sản phẩm chiếm 3 suất
// trên tấm, không phải một suất.
func TestBatchQuota_QuantityCountsAsProducts(t *testing.T) {
	f := newQuotaFixture(t, 4)
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
	f := newQuotaFixture(t, 4)
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

	// Sản phẩm 1×1 mm; tấm NVL 1 là 2×1 (2 sp/tấm), NVL 2 là 3×1 (3 sp/tấm), NVL 3
	// chưa khai kích thước (không giới hạn). Một kích thước sản phẩm, ba định mức
	// khác nhau — vì mỗi tấm một cỡ.
	one := 1.0
	sheets := []*float64{ptrFloat(2), ptrFloat(3), nil}
	mats := make([]*models.Material, 0, 3)
	sku := &models.SKU{Code: "COMBO-3", Name: "Combo 3 NVL", IsCombo: true, LengthMM: &one, WidthMM: &one}
	if err := db.Create(sku).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}
	for i, l := range sheets {
		m := &models.Material{Code: fmt.Sprintf("MAT-%d", i+1), Name: fmt.Sprintf("NVL %d", i+1)}
		if l != nil {
			m.LengthMM, m.WidthMM = l, &one
		}
		if err := db.Create(m).Error; err != nil {
			t.Fatalf("seed material: %v", err)
		}
		mats = append(mats, m)
		if err := db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: m.ID, QuantityPerUnit: 1}).Error; err != nil {
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

	// Sản phẩm 1×1 mm; tấm NVL 1 là 5×1 (5 sp/tấm), NVL 2 là 2×1 (2 sp/tấm).
	one := 1.0
	sku := &models.SKU{Code: "COMBO-2", Name: "Combo 2 NVL", IsCombo: true, LengthMM: &one, WidthMM: &one}
	if err := db.Create(sku).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}
	mats := make([]*models.Material, 0, 2)
	for i, l := range []float64{5, 2} {
		sheet := l
		m := &models.Material{Code: fmt.Sprintf("M%d", i+1), Name: fmt.Sprintf("NVL %d", i+1), LengthMM: &sheet, WidthMM: &one}
		if err := db.Create(m).Error; err != nil {
			t.Fatalf("seed material: %v", err)
		}
		mats = append(mats, m)
		if err := db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: m.ID, QuantityPerUnit: 1}).Error; err != nil {
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

// TestProductionQuota_GridAndDeclared: định mức ước tính = xếp hộp bao D×R lên
// tấm theo lưới, lấy chiều tốt hơn — số thật của khách (600×800 mica: 127×127
// ra 24 chứ không phải 29 như chia diện tích). Định mức khai cho cặp luôn
// thắng ước tính; khai cho NVL khác thì không ảnh hưởng.
func TestProductionQuota_GridAndDeclared(t *testing.T) {
	sku := func(l, w float64) *models.SKU { return &models.SKU{LengthMM: &l, WidthMM: &w} }
	mat := func(l, w float64) *models.Material { return &models.Material{LengthMM: &l, WidthMM: &w} }
	cases := []struct {
		name string
		sku  *models.SKU
		mat  *models.Material
		want int
	}{
		{"khách: mica 600×800, hàng 5×5 inch (127) → 4×6 = 24 (diện tích nói 29)", sku(127, 127), mat(600, 800), 24},
		{"khách: mica 600×800, hàng 6×6 inch (152) → 3×5 = 15 (diện tích nói 20)", sku(152, 152), mat(600, 800), 15},
		{"khách: mica 600×800, hàng 10 inch (254) → 2×3 = 6 (diện tích nói 7)", sku(254, 254), mat(600, 800), 6},
		{"xoay 90° lợi hơn: 300×200 trên 600×800 → 2×4 = 8, không phải 3×2 = 6", sku(300, 200), mat(600, 800), 8},
		{"chia hết: 60×1 / 15×1 = 4", sku(15, 1), mat(60, 1), 4},
		{"số thực ở biên: 0,3×1 / 0,1×1 = 3 (float cho 2,999…)", sku(0.1, 1), mat(0.3, 1), 3},
		{"tấm 1220×2440 / 88,9×88,9 = 13×27 = 351", sku(88.9, 88.9), mat(1220, 2440), 351},
		{"sản phẩm to hơn tấm → 1, không phải 0", sku(300, 300), mat(100, 100), 1},
		{"SKU chưa có kích thước → 0 (không định mức)", &models.SKU{}, mat(100, 100), 0},
		{"tấm chưa có kích thước → 0", sku(10, 10), &models.Material{}, 0},
	}
	for _, c := range cases {
		if got := models.EstimatedQuota(c.sku, c.mat); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
		if got, src := models.ProductionQuotaSource(c.sku, c.mat); got != c.want || (c.want > 0 && src != models.QuotaEstimated) {
			t.Errorf("%s (ProductionQuotaSource): got %d/%q, want %d/estimated", c.name, got, src, c.want)
		}
	}

	forty := 40
	declared := sku(127, 127)
	declared.Materials = []models.SKUMaterial{{MaterialID: 7, ProductsPerUnit: &forty}}
	mica := mat(600, 800)
	mica.ID = 7
	if q, src := models.ProductionQuotaSource(declared, mica); q != 40 || src != models.QuotaDeclared {
		t.Errorf("định mức khai 40 phải thắng ước tính 24: got %d/%q", q, src)
	}
	other := mat(600, 800)
	other.ID = 8
	if q, src := models.ProductionQuotaSource(declared, other); q != 24 || src != models.QuotaEstimated {
		t.Errorf("khai cho NVL 7 không ảnh hưởng NVL 8: got %d/%q, want 24/estimated", q, src)
	}
	zero := 0
	declared.Materials = []models.SKUMaterial{{MaterialID: 7, ProductsPerUnit: &zero}}
	if q, src := models.ProductionQuotaSource(declared, mica); q != 24 || src != models.QuotaEstimated {
		t.Errorf("khai 0 = chưa khai → ước tính: got %d/%q", q, src)
	}
	if models.ProductFitsSheet(sku(300, 300), mat(100, 100)) {
		t.Errorf("300×300 phải KHÔNG vừa tấm 100×100")
	}
	if !models.ProductFitsSheet(sku(100, 100), mat(100, 100)) || !models.ProductFitsSheet(&models.SKU{}, mat(1, 1)) {
		t.Errorf("bằng tấm, hoặc chưa khai, phải coi là vừa")
	}
}

// TestBatchMaterialUnits: the sheet count a batch reports comes from its parts'
// quota (declared, else size estimate) — a child says ⌈Σ qty/quota⌉, a parent the
// sum of its children, and a batch whose parts have no quota says nothing
// rather than guess. It rides on the associations the detail/list queries already load, so
// it costs no extra round trip (TestFindByID_RoundTripBudget pins that).
func TestBatchMaterialUnits(t *testing.T) {
	f := newQuotaFixture(t, 4)
	ids := f.add(t, 0, 5, 1) // 5 sp, 4/tấm → mẹ + 2 con (4 + 1)
	parent, _, err := f.svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{
		MaterialID: f.material.ID, OrderItemIDs: ids,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	repo := repositories.New(f.db)
	got, err := repo.Batch.FindByID(parent.ID)
	if err != nil {
		t.Fatalf("find parent: %v", err)
	}
	if got.MaterialUnits == nil || *got.MaterialUnits != 2 {
		t.Fatalf("parent material_units = %v, want 2 (two children)", got.MaterialUnits)
	}
	if len(got.ChildBatches) != 2 {
		t.Fatalf("children = %d", len(got.ChildBatches))
	}
	for _, c := range got.ChildBatches {
		if c.MaterialUnits == nil || *c.MaterialUnits != 1 {
			t.Fatalf("child %s material_units = %v, want 1 (4/4 and 1/4 both round up to one sheet)", c.Code, c.MaterialUnits)
		}
	}
	rows, _, err := repo.Batch.List(repositories.BatchFilter{Page: repositories.Page{Page: 1, PageSize: 50}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, b := range rows {
		if b.MaterialUnits == nil {
			t.Fatalf("list must fill material_units for %s", b.Code)
		}
	}

	// No size → no quota → no number.
	g := newQuotaFixture(t, 0)
	flat, _, err := g.svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{
		MaterialID: g.material.ID, OrderItemIDs: g.add(t, 0, 3, 1),
	})
	if err != nil {
		t.Fatalf("create flat: %v", err)
	}
	if gotFlat, _ := repositories.New(g.db).Batch.FindByID(flat.ID); gotFlat.MaterialUnits != nil {
		t.Fatalf("flat batch without quota: material_units = %v, want nil", *gotFlat.MaterialUnits)
	}
}
