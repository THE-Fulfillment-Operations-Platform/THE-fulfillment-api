package services

import (
	"net/http"
	"strings"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// seedFailedItem dựng đúng điểm xuất phát của "Làm lại tại chỗ": NVL + SKU khai
// NVL đó (để hàng chờ gom batch và tạo batch hoạt động), một sản phẩm đã cắt
// xong trong batch, rồi QC FAIL nó — phần cũ đã huỷ, batch cũ đã đóng, sản phẩm
// đang nằm ở hàng chờ gom batch.
func seedFailedItem(t *testing.T, db *gorm.DB, qc *QCService) (*models.OrderItem, *models.BatchItem, *models.Material) {
	t.Helper()
	mat := &models.Material{Code: "MICA", Name: "Mica"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	item, parts := seedQCItem(t, db, mat.ID)
	sku := &models.SKU{Code: "SKU-1", Name: "SKU"}
	db.Create(sku)
	db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mat.ID, QuantityPerUnit: 1})
	db.Model(&models.OrderItem{}).Where("id = ?", item.ID).Update("sku_id", sku.ID)

	if _, err := qc.Fail(Actor{ID: 5, Role: models.RoleQC}, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode}, DefectCode: "PRINT_WRONG",
	}); err != nil {
		t.Fatalf("QC fail: %v", err)
	}
	return item, parts[0], mat
}

// poolCount đếm sản phẩm đang chờ gom batch của một NVL — đúng truy vấn màn tạo batch dùng.
func poolCount(t *testing.T, repo *repositories.Repositories, materialID uint) int64 {
	t.Helper()
	_, total, err := repo.OrderItem.DesignReadyItemsForMaterial(materialID, repositories.Page{Page: 1, PageSize: 50}, "", "")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return total
}

// TestQCReworkInPlace_AddsNextAttemptToOldBatchAndPasses là luồng chính: sau QC
// fail, xưởng in lẻ xong → bấm làm lại tại chỗ → phần lần 2 (đã cắt) nằm trong
// CÙNG batch cũ, batch cũ mở lại, sản phẩm rời hàng chờ gom batch, rework_count
// không tăng thêm, nhật ký ghi QC_REWORK_INPLACE kèm lý do huỷ cũ — và PASS được
// ngay sau đó, batch lên QC_PASSED, ghi chú QC fail tự đóng.
func TestQCReworkInPlace_AddsNextAttemptToOldBatchAndPasses(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := newQCService(db)
	batchSvc := &BatchService{repo: repo, audit: &AuditService{repo: repo}}
	item, old, mat := seedFailedItem(t, db, qc)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	// Điểm xuất phát: batch cũ đã đóng, sản phẩm đang ở hàng chờ gom batch.
	var closed models.Batch
	db.First(&closed, old.BatchID)
	if closed.ClosedAt == nil {
		t.Fatalf("batch cũ phải đã đóng sau QC fail")
	}
	if n := poolCount(t, repo, mat.ID); n != 1 {
		t.Fatalf("sau QC fail sản phẩm phải ở hàng chờ gom batch, pool = %d", n)
	}

	res, err := qc.ReworkInPlace(owner, ReworkInPlaceInput{ScanRef: ScanRef{Code: item.InternalCode}})
	if err != nil {
		t.Fatalf("làm lại tại chỗ: %v", err)
	}

	// 1. Kết quả trả về là màn quét mới: 1 phần huỷ (lần 1) + 1 phần sống lần 2, đã cắt, cùng batch.
	if len(res.Batches) != 2 {
		t.Fatalf("màn quét mới phải có 2 phần (1 huỷ + 1 làm lại), got %d", len(res.Batches))
	}
	var fresh *QCScanBatch
	for i := range res.Batches {
		b := &res.Batches[i]
		if b.Scrapped {
			if b.Attempt != 1 || b.ScrapReason != "PRINT_WRONG" {
				t.Fatalf("phần cũ phải vẫn là lần 1 đã huỷ vì PRINT_WRONG, got %+v", b)
			}
			continue
		}
		fresh = b
	}
	if fresh == nil {
		t.Fatalf("phải có một phần sống sau khi làm lại tại chỗ: %+v", res.Batches)
	}
	if fresh.Attempt != 2 || fresh.Status != models.StatusCut || fresh.BatchCode != closed.Code {
		t.Fatalf("phần làm lại phải là lần 2, đã cắt, trong batch cũ %s; got %+v", closed.Code, fresh)
	}
	if res.InternalStatus != models.StatusCut {
		t.Fatalf("sản phẩm phải về Đã cắt, got %s", res.InternalStatus)
	}

	// 2. DB: đúng một phần sống, attempt 2, batch_id = batch cũ.
	var live []models.BatchItem
	db.Where("order_item_id = ? AND scrapped_at IS NULL", item.ID).Find(&live)
	if len(live) != 1 || live[0].Attempt != 2 || live[0].BatchID != old.BatchID || live[0].Status != models.StatusCut {
		t.Fatalf("phần sống sau làm lại tại chỗ sai: %+v", live)
	}
	// Lịch sử phải nói rõ nguồn: in lẻ tại trạm QC, thay phần đã huỷ vì sao.
	var hist models.StatusHistory
	if err := db.Where("entity_type = ? AND entity_id = ?", models.EntityBatchItem, live[0].ID).First(&hist).Error; err != nil {
		t.Fatalf("phải có lịch sử trạng thái cho phần làm lại: %v", err)
	}
	if !strings.Contains(hist.Note, reworkInPlaceSource) || !strings.Contains(hist.Note, "PRINT_WRONG") {
		t.Fatalf("lịch sử phải ghi nguồn in lẻ + lý do huỷ cũ, got %q", hist.Note)
	}

	// 3. Batch cũ mở lại, trạng thái theo phần sống (CUT).
	var reopened models.Batch
	db.First(&reopened, old.BatchID)
	if reopened.ClosedAt != nil || reopened.CloseReason != "" {
		t.Fatalf("batch cũ phải được mở lại, got closed_at=%v reason=%q", reopened.ClosedAt, reopened.CloseReason)
	}
	if reopened.Status != models.StatusCut {
		t.Fatalf("batch mở lại phải ở CUT theo phần sống, got %s", reopened.Status)
	}

	// 4. Sản phẩm rời hàng chờ gom batch; tạo batch cho nó bị bỏ qua (đã có phần sống).
	if n := poolCount(t, repo, mat.ID); n != 0 {
		t.Fatalf("sau làm lại tại chỗ sản phẩm phải rời hàng chờ gom batch, pool = %d", n)
	}
	if _, skipped, err := batchSvc.Create(owner, CreateBatchInput{MaterialID: mat.ID, OrderItemIDs: []uint{item.ID}}); err == nil && len(skipped) == 0 {
		t.Fatalf("không được gom lại sản phẩm đã làm lại tại chỗ vào batch mới")
	}

	// 5. rework_count giữ nguyên 1 (QC fail đã đếm), không mở thêm ghi chú.
	var reloaded models.OrderItem
	db.First(&reloaded, item.ID)
	if reloaded.ReworkCount != 1 {
		t.Fatalf("rework_count = %d, want 1 (làm lại tại chỗ không đếm thêm)", reloaded.ReworkCount)
	}
	if reloaded.InternalStatus != models.StatusCut {
		t.Fatalf("sản phẩm phải ở CUT, got %s", reloaded.InternalStatus)
	}
	var notes int64
	db.Model(&models.Note{}).Where("entity_id = ?", item.ID).Count(&notes)
	if notes != 1 {
		t.Fatalf("làm lại tại chỗ không được mở ghi chú mới, có %d ghi chú", notes)
	}

	// 6. Nhật ký: QC_REWORK_INPLACE kèm lý do huỷ cũ.
	var audit models.AuditLog
	if err := db.Where("action = ?", "QC_REWORK_INPLACE").First(&audit).Error; err != nil {
		t.Fatalf("phải có nhật ký QC_REWORK_INPLACE: %v", err)
	}
	if !strings.Contains(audit.Summary, "PRINT_WRONG") || !strings.Contains(audit.Summary, item.InternalCode) {
		t.Fatalf("nhật ký phải nêu sản phẩm + lý do huỷ cũ, got %q", audit.Summary)
	}

	// 7. PASS được ngay: phần lần 2 lên QC_PASSED, phần cũ giữ nguyên, batch lên QC_PASSED, ghi chú tự đóng.
	passed, err := qc.Pass(Actor{ID: 5, Role: models.RoleQC}, QCDecisionInput{ScanRef: ScanRef{Code: item.InternalCode}})
	if err != nil {
		t.Fatalf("QC pass sau làm lại tại chỗ: %v", err)
	}
	if passed.InternalStatus != models.StatusQCPassed {
		t.Fatalf("sản phẩm phải QC_PASSED, got %s", passed.InternalStatus)
	}
	var oldPart, newPart models.BatchItem
	db.First(&oldPart, old.ID)
	db.First(&newPart, live[0].ID)
	if oldPart.Status != models.StatusCut || oldPart.ScrappedAt == nil {
		t.Fatalf("tấm đã huỷ không được đóng dấu QC, got %+v", oldPart)
	}
	if newPart.Status != models.StatusQCPassed {
		t.Fatalf("phần làm lại phải QC_PASSED, got %s", newPart.Status)
	}
	var done models.Batch
	db.First(&done, old.BatchID)
	if done.Status != models.StatusQCPassed || done.ClosedAt != nil {
		t.Fatalf("batch cũ phải lên QC_PASSED và vẫn mở như batch thường, got status=%s closed_at=%v", done.Status, done.ClosedAt)
	}
	var open int64
	db.Model(&models.Note{}).Where("entity_id = ? AND status <> ?", item.ID, models.NoteResolved).Count(&open)
	if open != 0 {
		t.Fatalf("ghi chú QC fail phải tự đóng sau khi làm lại đạt, còn %d", open)
	}
}

// TestQCReworkInPlace_RefusesWhenLivePartExists: sản phẩm đã được gom batch mới
// (có phần sống) thì không làm lại tại chỗ nữa — không được đẻ ra hai phần sống
// cho cùng một NVL.
func TestQCReworkInPlace_RefusesWhenLivePartExists(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := newQCService(db)
	item, old, mat := seedFailedItem(t, db, qc)

	next, _ := repo.Batch.NextAttempts(mat.ID, []uint{item.ID})
	newBatch := &models.Batch{Code: "#101888", MaterialID: mat.ID, Status: models.StatusPending}
	db.Create(newBatch)
	db.Create(&models.BatchItem{
		BatchID: newBatch.ID, OrderItemID: item.ID, MaterialID: mat.ID,
		Status: models.StatusPending, Attempt: next[item.ID],
	})

	_, err := qc.ReworkInPlace(Actor{ID: 1, Role: models.RoleOwner}, ReworkInPlaceInput{ScanRef: ScanRef{Code: item.InternalCode}})
	if err == nil {
		t.Fatalf("đã có phần sống thì phải từ chối làm lại tại chỗ")
	}
	if !strings.Contains(err.Error(), "đã có phần sản xuất mới") {
		t.Fatalf("thông báo phải nói sản phẩm đã có phần mới, got %v", err)
	}
	var total int64
	db.Model(&models.BatchItem{}).Where("order_item_id = ?", item.ID).Count(&total)
	if total != 2 {
		t.Fatalf("lần từ chối không được tạo thêm phần nào, có %d phần", total)
	}
	var b models.Batch
	db.First(&b, old.BatchID)
	if b.ClosedAt == nil {
		t.Fatalf("batch cũ không được mở lại khi bị từ chối")
	}
}

// TestQCReworkInPlace_RefusesWithoutScrappedPart: hàng chưa từng fail thì không có
// gì để làm lại — hỏng thì phải ghi QC FAIL trước.
func TestQCReworkInPlace_RefusesWithoutScrappedPart(t *testing.T) {
	db := newQCDB(t)
	qc := newQCService(db)
	item, _ := seedQCItem(t, db, 1)

	_, err := qc.ReworkInPlace(Actor{ID: 1, Role: models.RoleOwner}, ReworkInPlaceInput{ScanRef: ScanRef{Code: item.InternalCode}})
	if err == nil {
		t.Fatalf("không có phần đã huỷ thì phải từ chối")
	}
	if !strings.Contains(err.Error(), "không có phần sản xuất nào đã huỷ") {
		t.Fatalf("thông báo phải nói không có phần đã huỷ, got %v", err)
	}
	var total int64
	db.Model(&models.BatchItem{}).Where("order_item_id = ?", item.ID).Count(&total)
	if total != 1 {
		t.Fatalf("không được tạo thêm phần nào, có %d", total)
	}
}

// TestQCReworkInPlace_RequiresAdminRole: cùng bar với hạ QC — QC/Sản xuất dù có
// tick "Thao tác" ở trạm QC cũng không bấm được; Owner/Admin/Kỹ thuật viên thì được.
func TestQCReworkInPlace_RequiresAdminRole(t *testing.T) {
	db := newQCDB(t)
	qc := newQCService(db)
	item, _, _ := seedFailedItem(t, db, qc)
	in := ReworkInPlaceInput{ScanRef: ScanRef{Code: item.InternalCode}}

	for _, role := range []models.Role{models.RoleQC, models.RoleProduction, models.RoleOps} {
		_, err := qc.ReworkInPlace(Actor{ID: 9, Role: role}, in)
		if err == nil {
			t.Fatalf("%s không được bấm làm lại tại chỗ", role)
		}
		if ae, ok := apperr.As(err); !ok || ae.Status != http.StatusForbidden {
			t.Fatalf("%s: phải là 403 Forbidden, got %v", role, err)
		}
	}
	// Admin có tick qc.manage nhưng bị gỡ (tick riêng) thì cũng không được.
	_, err := qc.ReworkInPlace(Actor{ID: 8, Role: models.RoleAdmin, Perms: map[string]bool{"qc.view": true}}, in)
	if ae, ok := apperr.As(err); !ok || ae.Status != http.StatusForbidden {
		t.Fatalf("Admin thiếu qc.manage phải bị 403, got %v", err)
	}
	var total int64
	db.Model(&models.BatchItem{}).Where("order_item_id = ?", item.ID).Count(&total)
	if total != 1 {
		t.Fatalf("các lần bị từ chối không được tạo phần nào, có %d", total)
	}

	// Kỹ thuật viên (super) thì được.
	if _, err := qc.ReworkInPlace(Actor{ID: 2, Role: models.RoleTechnician}, in); err != nil {
		t.Fatalf("kỹ thuật viên phải bấm được: %v", err)
	}
}

// TestQCReworkInPlace_ComboReworksOnlyTheScrappedMaterial: combo hỏng một mặt thì
// chỉ mặt đó có phần lần 2; mặt còn tốt giữ nguyên, và PASS cả sản phẩm sau đó.
func TestQCReworkInPlace_ComboReworksOnlyTheScrappedMaterial(t *testing.T) {
	db := newQCDB(t)
	qc := newQCService(db)
	item, parts := seedQCItem(t, db, 1, 2) // gỗ + mica, mỗi NVL một batch
	wood, mica := parts[0], parts[1]

	if _, err := qc.Fail(Actor{ID: 5, Role: models.RoleQC}, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode}, BatchItemID: &mica.ID, DefectCode: "MATERIAL_DEFECT",
	}); err != nil {
		t.Fatalf("QC fail phần mica: %v", err)
	}

	res, err := qc.ReworkInPlace(Actor{ID: 1, Role: models.RoleAdmin}, ReworkInPlaceInput{ScanRef: ScanRef{Code: item.InternalCode}})
	if err != nil {
		t.Fatalf("làm lại tại chỗ combo: %v", err)
	}
	if len(res.Batches) != 3 {
		t.Fatalf("want 3 phần (gỗ + mica huỷ + mica lần 2), got %d", len(res.Batches))
	}

	var woodParts, micaParts []models.BatchItem
	db.Where("order_item_id = ? AND material_id = ?", item.ID, 1).Find(&woodParts)
	db.Where("order_item_id = ? AND material_id = ?", item.ID, 2).Order("attempt asc").Find(&micaParts)
	if len(woodParts) != 1 || woodParts[0].ID != wood.ID || woodParts[0].ScrappedAt != nil {
		t.Fatalf("phần gỗ còn tốt phải giữ nguyên, got %+v", woodParts)
	}
	if len(micaParts) != 2 || micaParts[1].Attempt != 2 || micaParts[1].BatchID != mica.BatchID || micaParts[1].Status != models.StatusCut {
		t.Fatalf("mica phải có lần 2 đã cắt trong batch cũ, got %+v", micaParts)
	}

	if _, err := qc.Pass(Actor{ID: 5, Role: models.RoleQC}, QCDecisionInput{ScanRef: ScanRef{Code: item.InternalCode}}); err != nil {
		t.Fatalf("PASS cả sản phẩm sau khi làm lại mica: %v", err)
	}
	var reloaded models.OrderItem
	db.First(&reloaded, item.ID)
	if reloaded.InternalStatus != models.StatusQCPassed {
		t.Fatalf("sản phẩm phải QC_PASSED, got %s", reloaded.InternalStatus)
	}
}

// TestQCReworkInPlace_BlockedOnceOrderStartedPacking: cùng ranh giới với QC fail /
// hạ QC — đơn đã lên bàn đóng gói thì không kéo gì về trạm QC nữa.
func TestQCReworkInPlace_BlockedOnceOrderStartedPacking(t *testing.T) {
	db := newQCDB(t)
	qc := newQCService(db)
	item, _, _ := seedFailedItem(t, db, qc)
	order := seedPackageFor(t, db, item.ID)

	_, err := qc.ReworkInPlace(Actor{ID: 1, Role: models.RoleOwner}, ReworkInPlaceInput{ScanRef: ScanRef{Code: item.InternalCode}})
	if err == nil {
		t.Fatalf("đơn đã bắt đầu đóng gói thì phải chặn")
	}
	if !strings.Contains(err.Error(), order.InternalCode) {
		t.Fatalf("thông báo phải gọi tên đơn, got %v", err)
	}
}
