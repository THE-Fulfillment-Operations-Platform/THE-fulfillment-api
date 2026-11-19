package services

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// #6380 — 19/11/2026 17:11: SKU WD-PBH-5L-* đang map Basswood 3 ly bị THÊM
// Basswood 5 ly (3 ly vẫn còn) → combo 2 tấm → 23 sản phẩm đã cắt trên 3 ly hiện
// lại ở màn Tạo batch của 5 ly và bị gom 8 batch trùng. Các test dưới đây khoá
// cửa xác nhận khi đổi NVL của SKU, nhãn ở hàng chờ gom batch, và việc Create
// không bao giờ để lại batch rỗng.

func confirmImpacts(t *testing.T, err error) []SKUMaterialImpact {
	t.Helper()
	ae, ok := apperr.As(err)
	if !ok || ae.Status != http.StatusConflict || ae.Code != CodeSKUMaterialChangeConfirm {
		t.Fatalf("want 409 %s, got %v", CodeSKUMaterialChangeConfirm, err)
	}
	d, ok := ae.Details.(map[string]any)
	if !ok {
		t.Fatalf("details = %#v, want a map", ae.Details)
	}
	imps, ok := d["skus"].([]SKUMaterialImpact)
	if !ok || len(imps) == 0 {
		t.Fatalf("details.skus = %#v", d["skus"])
	}
	if !strings.Contains(ae.Message, skuMaterialChangeHint) {
		t.Fatalf("message must carry the hint, got %q", ae.Message)
	}
	return imps
}

func TestMaterialFamily(t *testing.T) {
	cases := []struct {
		name, code, want string
	}{
		{"Basswood 3 ly", "", "BASSWOOD"},
		{"", "BASSWOOD-5-LY", "BASSWOOD"},
		{"Basswood 5mm", "", "BASSWOOD"},
		{"Basswood 2,5 ly", "", "BASSWOOD"},
		{"Mica trong 3 ly", "", "MICA-TRONG"},
		{"Mica trong 2ly", "", "MICA-TRONG"},
		{"Mica Hologram", "", "MICA-HOLOGRAM"},
		{"Gỗ thông 9 ly", "", "GO-THONG"},
		{"", "", ""},
		{"3 ly", "", ""},
	}
	for _, c := range cases {
		if got := materialFamily(MaterialRef{Name: c.name, Code: c.code}); got != c.want {
			t.Errorf("materialFamily(%q/%q) = %q, want %q", c.name, c.code, got, c.want)
		}
	}
}

// Đúng vụ 19/11: SKU 3 ly có hàng đã vào batch, thêm 5 ly mà không bỏ 3 ly.
func TestUpdateSKU_AddingSameKindWhileInProductionNeedsConfirm(t *testing.T) {
	f := newRemapFixture(t)
	f.mapSKU(t, f.m3.ID)
	cat := &CatalogService{repo: f.repo, audit: &AuditService{repo: f.repo}}
	owner := Actor{ID: 1, Role: models.RoleOwner}

	cut := f.item(t) // đã cắt trên 3 ly
	b1 := f.batchOn(t, f.m3.ID, cut)
	f.setParts(t, cut, models.StatusCut)
	pending := f.item(t) // vào batch 3 ly, chưa in
	b2 := f.batchOn(t, f.m3.ID, pending)
	shipped := f.item(t) // đã gửi đi: không còn là hàng trong xưởng
	f.batchOn(t, f.m3.ID, shipped)
	f.setParts(t, shipped, models.StatusQCPassed)
	f.setOrder(t, shipped, models.SellerStatusShipped)
	f.item(t) // chưa gom: đổi NVL là chuyện bình thường với nó

	in := SKUUpdateInput{Materials: []SKUMaterialInput{{MaterialID: f.m3.ID}, {MaterialID: f.m5.ID}}}
	_, err := cat.UpdateSKU(owner, f.sku.ID, in)
	imps := confirmImpacts(t, err)
	imp := imps[0]
	if imp.InProduction != 2 {
		t.Fatalf("in_production = %d, want 2 (cut + pending; shipped excluded)", imp.InProduction)
	}
	if !imp.Grows || len(imp.Added) != 1 || imp.Added[0].ID != f.m5.ID || len(imp.Removed) != 0 {
		t.Fatalf("impact = %+v, want grows + added 5 ly", imp)
	}
	if len(imp.SameFamily) != 1 {
		t.Fatalf("same_family = %+v, want Basswood 3 ly + 5 ly", imp.SameFamily)
	}
	if len(imp.InProductionBy) != 1 || imp.InProductionBy[0].Material.ID != f.m3.ID || imp.InProductionBy[0].Items != 2 {
		t.Fatalf("in_production_by = %+v", imp.InProductionBy)
	}
	if got := strings.Join(imp.InProductionBy[0].Batches, ","); got != b1.Code+","+b2.Code {
		t.Fatalf("batches = %s, want %s,%s", got, b1.Code, b2.Code)
	}
	// Không lưu gì: SKU vẫn chỉ 3 ly, hàng chờ 5 ly vẫn trống.
	if got, _ := f.repo.SKU.FindByID(f.sku.ID); len(got.Materials) != 1 {
		t.Fatalf("refused change must not be saved, SKU has %d materials", len(got.Materials))
	}
	if got := f.pool(t, f.m5.ID); len(got) != 0 {
		t.Fatalf("pool 5 ly = %v, want empty", got)
	}

	// Xác nhận rồi thì lưu (combo thật vẫn làm được) — và đúng như cảnh báo, hai
	// sản phẩm đang làm trên 3 ly hiện ở hàng chờ 5 ly cùng hàng chưa gom.
	in.ConfirmMaterialChange = true
	if _, err := cat.UpdateSKU(owner, f.sku.ID, in); err != nil {
		t.Fatalf("confirmed update: %v", err)
	}
	if got := f.pool(t, f.m5.ID); len(got) != 3 {
		t.Fatalf("pool 5 ly after confirmed combo = %v, want 3 (2 in production + 1 waiting)", got)
	}

	// Sửa định mức, không đổi bộ NVL: không hỏi.
	q := 12
	if _, err := cat.UpdateSKU(owner, f.sku.ID, SKUUpdateInput{Materials: []SKUMaterialInput{
		{MaterialID: f.m3.ID, ProductsPerUnit: &q}, {MaterialID: f.m5.ID},
	}}); err != nil {
		t.Fatalf("quota-only edit must not ask: %v", err)
	}
}

// Đổi hẳn tấm (bỏ 3 ly, lấy 5 ly) khi có hàng đang làm: vẫn hỏi, nhưng câu nói rõ
// hàng cũ làm tiếp trên NVL cũ (không hiện lại).
func TestUpdateSKU_ReplacingWhileInProductionAsksWithoutGrow(t *testing.T) {
	f := newRemapFixture(t)
	f.mapSKU(t, f.m3.ID)
	cat := &CatalogService{repo: f.repo, audit: &AuditService{repo: f.repo}}
	it := f.item(t)
	f.batchOn(t, f.m3.ID, it)

	_, err := cat.UpdateSKU(Actor{ID: 1, Role: models.RoleOwner}, f.sku.ID,
		SKUUpdateInput{Materials: []SKUMaterialInput{{MaterialID: f.m5.ID}}})
	imp := confirmImpacts(t, err)[0]
	if imp.Grows || imp.InProduction != 1 || len(imp.SameFamily) != 0 || len(imp.Removed) != 1 {
		t.Fatalf("impact = %+v, want replace (no grow, no same family), 1 in production", imp)
	}
	if ae, _ := apperr.As(err); !strings.Contains(ae.Message, "xoá batch cũ") {
		t.Fatalf("message should say how to move the product, got %q", ae.Message)
	}
}

// Không có hàng trong xưởng: đổi tấm hay thêm NVL khác loại không hỏi; thêm NVL
// cùng loại khác độ dày thì vẫn hỏi (gần như chắc là nhầm).
func TestUpdateSKU_NoProduction(t *testing.T) {
	f := newRemapFixture(t)
	f.mapSKU(t, f.m3.ID)
	cat := &CatalogService{repo: f.repo, audit: &AuditService{repo: f.repo}}
	owner := Actor{ID: 1, Role: models.RoleOwner}
	mica := &models.Material{Code: "MICA-TRONG-3-LY", Name: "Mica trong 3 ly"}
	if err := f.db.Create(mica).Error; err != nil {
		t.Fatal(err)
	}

	_, err := cat.UpdateSKU(owner, f.sku.ID, SKUUpdateInput{Materials: []SKUMaterialInput{{MaterialID: f.m3.ID}, {MaterialID: f.m5.ID}}})
	if imp := confirmImpacts(t, err)[0]; imp.InProduction != 0 || len(imp.SameFamily) != 1 {
		t.Fatalf("impact = %+v, want same-family only", imp)
	}
	if _, err := cat.UpdateSKU(owner, f.sku.ID, SKUUpdateInput{Materials: []SKUMaterialInput{{MaterialID: f.m3.ID}, {MaterialID: mica.ID}}}); err != nil {
		t.Fatalf("combo of two different kinds must not ask: %v", err)
	}
	if _, err := cat.UpdateSKU(owner, f.sku.ID, SKUUpdateInput{Materials: []SKUMaterialInput{{MaterialID: f.m5.ID}}}); err != nil {
		t.Fatalf("plain replace with nothing in production must not ask: %v", err)
	}
}

func TestCreateSKU_SameKindAsks(t *testing.T) {
	f := newRemapFixture(t)
	cat := &CatalogService{repo: f.repo, audit: &AuditService{repo: f.repo}}
	owner := Actor{ID: 1, Role: models.RoleOwner}
	in := SKUInput{Code: "NEW-1", Name: "NEW-1", Materials: []SKUMaterialInput{{MaterialID: f.m3.ID}, {MaterialID: f.m5.ID}}}
	_, err := cat.CreateSKU(owner, in)
	confirmImpacts(t, err)
	if _, err := f.repo.SKU.FindByCode("NEW-1"); err == nil {
		t.Fatalf("refused create must not save the SKU")
	}
	in.ConfirmMaterialChange = true
	if _, err := cat.CreateSKU(owner, in); err != nil {
		t.Fatalf("confirmed create: %v", err)
	}
}

// Import Excel chỉ THÊM: file ghi "Basswood 5 ly" cho SKU đang "Basswood 3 ly" =
// combo 3+5. Xem trước phải báo, áp dụng phải đòi xác nhận.
func TestMasterImport_AddingMaterialToExistingSKUNeedsConfirm(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	actor := Actor{ID: 1}
	const sku = "WD-PBH-5L-8IN"

	pv, err := svc.Preview(actor, "CSV", "a.csv", legacyRows([2]string{sku, "Basswood 3 ly"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.MaterialWarnings) != 0 {
		t.Fatalf("a new single-material SKU needs no confirm, got %+v", pv.MaterialWarnings)
	}
	if _, err := svc.Commit(actor, pv.ImportJobID); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	// Một sản phẩm của SKU đã vào batch 3 ly, đơn còn trong xưởng.
	var rec models.SKU
	db.Where("code = ?", sku).First(&rec)
	var m3 models.Material
	db.Where("name = ?", "Basswood 3 ly").First(&m3)
	order := &models.Order{InternalCode: "107218", StoreOrderID: "E-1", StoreOrderRef: "E-1", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction}
	db.Create(order)
	item := &models.OrderItem{OrderID: order.ID, LineNo: 1, InternalCode: "107218_1/1", SKUID: &rec.ID, SKUCode: sku, Quantity: 1}
	db.Create(item)
	batch := &models.Batch{Code: "#102655", MaterialID: m3.ID, Status: models.StatusCut}
	db.Create(batch)
	db.Create(&models.BatchItem{BatchID: batch.ID, OrderItemID: item.ID, MaterialID: m3.ID, Status: models.StatusCut, Attempt: 1})

	pv2, err := svc.Preview(actor, "CSV", "b.csv", legacyRows([2]string{sku, "Basswood 5 ly"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(pv2.MaterialWarnings) != 1 || pv2.Summary.MaterialWarnings != 1 {
		t.Fatalf("preview warnings = %+v, want 1", pv2.MaterialWarnings)
	}
	w := pv2.MaterialWarnings[0]
	if len(w.KeptNotInFile) != 1 || w.KeptNotInFile[0].ID != m3.ID || len(w.SameFamily) != 1 || w.InProduction != 1 {
		t.Fatalf("warning = %+v, want kept 3 ly + same family + 1 in production", w)
	}
	if len(w.InProductionBy) != 1 || len(w.InProductionBy[0].Batches) != 1 || w.InProductionBy[0].Batches[0] != "#102655" {
		t.Fatalf("in_production_by = %+v", w.InProductionBy)
	}

	_, err = svc.Commit(actor, pv2.ImportJobID)
	confirmImpacts(t, err)
	var mapCount, matCount int64
	db.Model(&models.SKUMaterial{}).Where("sku_id = ?", rec.ID).Count(&mapCount)
	db.Model(&models.Material{}).Count(&matCount)
	if mapCount != 1 || matCount != 1 {
		t.Fatalf("refused commit wrote %d mappings / %d materials, want 1/1 (rolled back)", mapCount, matCount)
	}

	res, err := svc.CommitWithConfirm(actor, pv2.ImportJobID, true)
	if err != nil {
		t.Fatalf("confirmed commit: %v", err)
	}
	if res.Applied.MappingsCreated != 1 {
		t.Fatalf("applied = %+v", res.Applied)
	}

	// Nạp lại đúng bộ đang có (file "SKU hiện có" sửa định mức): không hỏi gì.
	pv3, err := svc.Preview(actor, "CSV", "c.csv", legacyRows([2]string{sku, "Basswood 3 ly + Basswood 5 ly"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(pv3.MaterialWarnings) != 0 {
		t.Fatalf("re-import of the current set must not warn, got %+v", pv3.MaterialWarnings)
	}
	if _, err := svc.Commit(actor, pv3.ImportJobID); err != nil {
		t.Fatalf("re-import commit: %v", err)
	}
}

// Màn Tạo batch: hàng chờ của NVL X mang theo phần sống của sản phẩm ở NVL khác
// (mã batch, trạng thái, tấm), để giao diện gắn nhãn "Đã có phần trên …".
func TestDesignReadyItems_CarryLivePartsOnOtherMaterials(t *testing.T) {
	f := newRemapFixture(t)
	f.mapSKU(t, f.m3.ID)
	cut := f.item(t)
	b := f.batchOn(t, f.m3.ID, cut)
	f.setParts(t, cut, models.StatusCut)
	waiting := f.item(t)
	f.mapSKU(t, f.m3.ID, f.m5.ID) // thêm 5 ly, không bỏ 3 ly (đi thẳng repo, bỏ qua cửa xác nhận)

	rows, _, err := f.repo.OrderItem.DesignReadyItemsForMaterial(f.m5.ID, repoPage(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uint]models.OrderItem{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if len(byID) != 2 {
		t.Fatalf("pool 5 ly = %d items, want 2", len(byID))
	}
	parts := byID[cut.ID].BatchItems
	if len(parts) != 1 || parts[0].Batch == nil || parts[0].Batch.Code != b.Code ||
		parts[0].Material == nil || parts[0].Material.ID != f.m3.ID || parts[0].Status != models.StatusCut {
		t.Fatalf("cut item parts = %+v, want one CUT part on 3 ly in %s", parts, b.Code)
	}
	if len(byID[waiting.ID].BatchItems) != 0 {
		t.Fatalf("waiting item has parts %+v", byID[waiting.ID].BatchItems)
	}

	// Phần đã ghi bỏ ở QC không phải "đã có phần".
	now := time.Now()
	f.db.Model(&models.BatchItem{}).Where("order_item_id = ?", cut.ID).Update("scrapped_at", &now)
	rows, _, _ = f.repo.OrderItem.DesignReadyItemsForMaterial(f.m5.ID, repoPage(), "", "")
	for _, r := range rows {
		if r.ID == cut.ID && len(r.BatchItems) != 0 {
			t.Fatalf("scrapped part must not be listed: %+v", r.BatchItems)
		}
	}
}

// Create không bao giờ để lại batch 0 dòng: không còn sản phẩm hợp lệ → 422 rõ
// lý do và không ghi gì; id lặp / id 0 không làm hỏng lần tạo.
func TestBatchCreate_NeverLeavesAnEmptyBatch(t *testing.T) {
	f := newRemapFixture(t)
	owner := Actor{ID: 1, Role: models.RoleOwner}
	shipped := f.item(t)
	f.setOrder(t, shipped, models.SellerStatusShipped)
	other := f.item(t)
	f.batchOn(t, f.m5.ID, other) // đã có batch 5 ly

	var before int64
	f.db.Unscoped().Model(&models.Batch{}).Count(&before)

	_, _, err := f.batch.Create(owner, CreateBatchInput{MaterialID: f.m5.ID, OrderItemIDs: []uint{shipped.ID, other.ID}})
	ae, ok := apperr.As(err)
	if !ok || ae.Status != http.StatusUnprocessableEntity || !strings.Contains(ae.Message, "Không gom được sản phẩm nào") {
		t.Fatalf("want 422 with reasons, got %v", err)
	}
	if !strings.Contains(ae.Message, "đã nằm trong một batch khác của NVL này") || !strings.Contains(ae.Message, "đã đóng gói / bàn giao") {
		t.Fatalf("message must name each reason, got %q", ae.Message)
	}
	if _, _, err := f.batch.Create(owner, CreateBatchInput{MaterialID: f.m5.ID, OrderItemIDs: []uint{0}}); err == nil {
		t.Fatalf("id 0 only must be refused")
	}
	var after int64
	f.db.Unscoped().Model(&models.Batch{}).Count(&after)
	if after != before {
		t.Fatalf("a refused create left %d batch row(s)", after-before)
	}

	// Gửi trùng id: một phần mỗi sản phẩm, không 500.
	a, b := f.item(t), f.item(t)
	created, skipped, err := f.batch.Create(owner, CreateBatchInput{MaterialID: f.m5.ID, OrderItemIDs: []uint{a.ID, a.ID, b.ID, 0}})
	if err != nil || len(skipped) != 0 || len(created) != 1 {
		t.Fatalf("create with repeated ids: %v (skipped %v, %d batch)", err, skipped, len(created))
	}
	var parts int64
	f.db.Model(&models.BatchItem{}).Where("batch_id = ?", created[0].ID).Count(&parts)
	if parts != 2 {
		t.Fatalf("batch has %d parts, want 2", parts)
	}
}

// "Batch 0 dòng" trong DB mà không lọc deleted_at = batch ĐÃ XOÁ: Delete xoá hẳn
// các phần (idx_item_material_attempt) và chỉ xoá mềm đầu batch. Khoá hành vi đó
// để câu soi DB biết phải thêm `b.deleted_at IS NULL`.
func TestBatchDelete_LeavesSoftDeletedHeaderWithNoParts(t *testing.T) {
	f := newRemapFixture(t)
	it := f.item(t)
	b := f.batchOn(t, f.m5.ID, it)
	if err := f.batch.Delete(Actor{ID: 1, Role: models.RoleOwner}, b.ID); err != nil {
		t.Fatal(err)
	}
	var hdr models.Batch
	if err := f.db.Unscoped().First(&hdr, b.ID).Error; err != nil {
		t.Fatalf("header row should stay (soft delete): %v", err)
	}
	if !hdr.DeletedAt.Valid {
		t.Fatalf("deleted batch header must carry deleted_at")
	}
	var parts int64
	f.db.Unscoped().Model(&models.BatchItem{}).Where("batch_id = ?", b.ID).Count(&parts)
	if parts != 0 {
		t.Fatalf("deleted batch keeps %d part rows, want 0", parts)
	}
	var live int64
	f.db.Model(&models.Batch{}).Where("id = ?", b.ID).Count(&live)
	if live != 0 {
		t.Fatalf("deleted batch still visible to normal queries")
	}
}

func repoPage() repositories.Page { return repositories.Page{Page: 1, PageSize: 100} }
