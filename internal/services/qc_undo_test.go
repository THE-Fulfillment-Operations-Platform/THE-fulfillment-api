package services

import (
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newQCService(db *gorm.DB) *QCService {
	repo := repositories.New(db)
	return &QCService{repo: repo, audit: &AuditService{repo: repo}}
}

// markQCPassed đẩy sản phẩm + mọi phần + mọi batch của nó lên "đã QC", đúng như
// một lần QC pass để lại.
func markQCPassed(t *testing.T, db *gorm.DB, itemID uint) {
	t.Helper()
	if err := db.Model(&models.BatchItem{}).Where("order_item_id = ?", itemID).
		Update("status", models.StatusQCPassed).Error; err != nil {
		t.Fatalf("mark parts: %v", err)
	}
	if err := db.Model(&models.OrderItem{}).Where("id = ?", itemID).
		Update("internal_status", models.StatusQCPassed).Error; err != nil {
		t.Fatalf("mark item: %v", err)
	}
	if err := db.Model(&models.Batch{}).Where("id > 0").
		Update("status", models.StatusQCPassed).Error; err != nil {
		t.Fatalf("mark batches: %v", err)
	}
}

// seedPackageFor mở kiện cho đơn của một sản phẩm — đúng thứ lần quét đầu tiên ở
// trạm đóng gói để lại.
func seedPackageFor(t *testing.T, db *gorm.DB, itemID uint) *models.Order {
	t.Helper()
	var item models.OrderItem
	if err := db.First(&item, itemID).Error; err != nil {
		t.Fatalf("load item: %v", err)
	}
	var order models.Order
	if err := db.First(&order, item.OrderID).Error; err != nil {
		t.Fatalf("load order: %v", err)
	}
	if err := db.Create(&models.Package{
		Code: "PKG-000001", OrderID: order.ID, Status: models.PackageOpen,
	}).Error; err != nil {
		t.Fatalf("seed package: %v", err)
	}
	return &order
}

// TestQCUndo_ReopensTheGateWithoutScrappingAnything: bấm nhầm thì hàng vẫn nguyên
// vẹn trên bàn. Hạ QC chỉ mở lại cửa — không tấm nào bị ghi bỏ, không lần làm lại
// nào được đếm, không sản phẩm nào bị đẩy về hàng chờ thiết kế.
func TestQCUndo_ReopensTheGateWithoutScrappingAnything(t *testing.T) {
	db := newQCDB(t)
	qc := newQCService(db)
	item, parts := seedQCItem(t, db, 1)
	markQCPassed(t, db, item.ID)

	res, err := qc.UndoPass(Actor{ID: 2, Role: models.RoleOwner}, UndoQCInput{
		ScanRef: ScanRef{Code: item.InternalCode}, Reason: "bấm nhầm sang sản phẩm bên cạnh",
	})
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if res.Parts != 1 || res.InternalStatus != models.StatusCut {
		t.Fatalf("undo result = %+v, want 1 phần và trạng thái CUT", res)
	}

	var part models.BatchItem
	db.First(&part, parts[0].ID)
	if part.Status != models.StatusCut {
		t.Fatalf("phần sản xuất phải về CUT, got %s", part.Status)
	}
	if part.ScrappedAt != nil {
		t.Fatal("hạ QC không được ghi bỏ tấm nào — hàng vẫn tốt")
	}

	var reloaded models.OrderItem
	db.First(&reloaded, item.ID)
	if reloaded.InternalStatus != models.StatusCut {
		t.Fatalf("sản phẩm phải về CUT, got %s", reloaded.InternalStatus)
	}
	if reloaded.ReworkCount != 0 {
		t.Fatalf("bấm nhầm không phải một lần làm lại, rework_count = %d", reloaded.ReworkCount)
	}
	if reloaded.DesignStatus != models.DesignReady {
		t.Fatalf("hạ QC không đụng tới design, got %s", reloaded.DesignStatus)
	}

	var b models.Batch
	db.First(&b, parts[0].BatchID)
	if b.Status != models.StatusCut {
		t.Fatalf("batch phải theo phần của nó về CUT, got %s", b.Status)
	}

	// Lịch sử QC phải nói đúng chuyện đã xảy ra: một lần gỡ, không phải một lần FAIL.
	var rec models.QCRecord
	if err := db.Where("result = ?", models.QCUndo).First(&rec).Error; err != nil {
		t.Fatalf("phải có bản ghi QC UNDO: %v", err)
	}
	if !strings.Contains(rec.Note, "bấm nhầm") {
		t.Fatalf("bản ghi phải giữ lý do, got %q", rec.Note)
	}
	var fails int64
	db.Model(&models.QCRecord{}).Where("result = ?", models.QCFail).Count(&fails)
	if fails != 0 {
		t.Fatal("hạ QC không được ghi thành QC FAIL — FAIL đồng nghĩa với huỷ hàng")
	}
	var notes int64
	db.Model(&models.Note{}).Count(&notes)
	if notes != 0 {
		t.Fatalf("bấm nhầm không đẻ ra việc cần xử lý, got %d ghi chú", notes)
	}

	// Và sau khi sửa xong thì QC pass lại được bình thường.
	if _, err := qc.Pass(Actor{ID: 2, Role: models.RoleQC}, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode},
	}); err != nil {
		t.Fatalf("QC pass lại sau khi hạ: %v", err)
	}
}

// TestQCUndo_BlockedOnceOrderStartedPacking: người đóng gói đang cầm hàng của cả
// đơn, không kéo trạng thái ra dưới tay họ được.
func TestQCUndo_BlockedOnceOrderStartedPacking(t *testing.T) {
	db := newQCDB(t)
	qc := newQCService(db)
	item, _ := seedQCItem(t, db, 1)
	markQCPassed(t, db, item.ID)
	order := seedPackageFor(t, db, item.ID)

	_, err := qc.UndoPass(Actor{ID: 2, Role: models.RoleOwner}, UndoQCInput{ScanRef: ScanRef{Code: item.InternalCode}})
	if err == nil {
		t.Fatal("hạ QC phải bị chặn khi đơn đã bắt đầu đóng gói")
	}
	if !strings.Contains(err.Error(), order.InternalCode) {
		t.Fatalf("thông báo phải gọi tên đơn, got %v", err)
	}
	var reloaded models.OrderItem
	db.First(&reloaded, item.ID)
	if reloaded.InternalStatus != models.StatusQCPassed {
		t.Fatalf("lần hạ bị chặn không được đổi gì, got %s", reloaded.InternalStatus)
	}
}

// TestQCUndo_RefusesWhenNothingWasPassed: không có cửa nào đang đóng thì không có
// gì để mở.
func TestQCUndo_RefusesWhenNothingWasPassed(t *testing.T) {
	db := newQCDB(t)
	qc := newQCService(db)
	item, _ := seedQCItem(t, db, 1)

	if _, err := qc.UndoPass(Actor{ID: 2, Role: models.RoleOwner},
		UndoQCInput{ScanRef: ScanRef{Code: item.InternalCode}}); err == nil {
		t.Fatal("sản phẩm chưa QC pass thì không có gì để hạ")
	}
}

// TestQCFail_ComboReopensQCForTheWholeProduct: cùng lỗ hổng combo như huỷ batch,
// nhưng qua đường QC fail — API này gọi được, chỉ có nút trên web là bị chặn.
func TestQCFail_ComboReopensQCForTheWholeProduct(t *testing.T) {
	db := newQCDB(t)
	qc := newQCService(db)
	item, parts := seedQCItem(t, db, 1, 2) // gỗ + mica, mỗi NVL một batch
	markQCPassed(t, db, item.ID)

	if _, err := qc.Fail(Actor{ID: 5, Role: models.RoleQC}, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode}, BatchItemID: &parts[0].ID, DefectCode: "CUT_WRONG",
	}); err != nil {
		t.Fatalf("QC fail phần gỗ: %v", err)
	}

	var wood, mica models.BatchItem
	db.First(&wood, parts[0].ID)
	db.First(&mica, parts[1].ID)
	if wood.ScrappedAt == nil {
		t.Fatal("phần bị fail phải được ghi bỏ")
	}
	if mica.ScrappedAt != nil {
		t.Fatal("phần còn tốt không được ghi bỏ theo")
	}
	if mica.Status != models.StatusCut {
		t.Fatalf("phần còn tốt phải bị hạ khỏi 'đã QC' về CUT, got %s", mica.Status)
	}
	var reloaded models.OrderItem
	db.First(&reloaded, item.ID)
	if reloaded.InternalStatus == models.StatusQCPassed {
		t.Fatal("sản phẩm còn phần đang làm lại thì KHÔNG được ở QC_PASSED")
	}
}

// TestQCFail_BlockedOnceOrderStartedPacking: quá muộn để trả hàng về "chờ làm lại".
func TestQCFail_BlockedOnceOrderStartedPacking(t *testing.T) {
	db := newQCDB(t)
	qc := newQCService(db)
	item, parts := seedQCItem(t, db, 1)
	markQCPassed(t, db, item.ID)
	order := seedPackageFor(t, db, item.ID)

	_, err := qc.Fail(Actor{ID: 5, Role: models.RoleQC}, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode}, DefectCode: "SCRATCH",
	})
	if err == nil {
		t.Fatal("QC fail phải bị chặn khi đơn đã bắt đầu đóng gói")
	}
	if !strings.Contains(err.Error(), order.InternalCode) {
		t.Fatalf("thông báo phải gọi tên đơn, got %v", err)
	}
	var part models.BatchItem
	db.First(&part, parts[0].ID)
	if part.ScrappedAt != nil {
		t.Fatal("lần fail bị chặn không được ghi bỏ gì")
	}
}

// TestQCPass_IgnoresPartsFromAScrappedAttempt: sau một lần làm lại, sản phẩm có
// hai dòng cho cùng NVL — lần cũ đã huỷ và lần mới. QC chỉ được nhìn lần mới:
// đóng dấu "đã QC" lên một tấm đã ở thùng rác là ghi sai lịch sử, và tệ hơn, lấy
// trạng thái của lần cũ làm bằng chứng "hàng đã cắt xong".
func TestQCPass_IgnoresPartsFromAScrappedAttempt(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := newQCService(db)
	item, parts := seedQCItem(t, db, 1)

	old := parts[0]
	if _, err := repo.Batch.ScrapBatchItems([]uint{old.ID}, "QC fail", nil, time.Now()); err != nil {
		t.Fatalf("scrap lần 1: %v", err)
	}
	newBatch := &models.Batch{Code: "#101999", MaterialID: 1, Status: models.StatusCut}
	db.Create(newBatch)
	fresh := &models.BatchItem{
		BatchID: newBatch.ID, OrderItemID: item.ID, MaterialID: 1,
		Status: models.StatusCut, Attempt: 2,
	}
	db.Create(fresh)

	if _, err := qc.Pass(Actor{ID: 5, Role: models.RoleQC}, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode},
	}); err != nil {
		t.Fatalf("QC pass lần làm lại: %v", err)
	}

	var scrapped, live models.BatchItem
	db.First(&scrapped, old.ID)
	db.First(&live, fresh.ID)
	if scrapped.Status != models.StatusCut {
		t.Fatalf("tấm đã huỷ không được đóng dấu QC, got %s", scrapped.Status)
	}
	if live.Status != models.StatusQCPassed {
		t.Fatalf("tấm làm lại phải QC_PASSED, got %s", live.Status)
	}
	var records int64
	db.Model(&models.QCRecord{}).Where("order_item_id = ?", item.ID).Count(&records)
	if records != 1 {
		t.Fatalf("chỉ ghi QC cho tấm còn sống, got %d bản ghi", records)
	}
}

// TestQCPass_RefusesWhenEveryPartWasScrapped: không còn gì trên bàn để QC.
func TestQCPass_RefusesWhenEveryPartWasScrapped(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := newQCService(db)
	item, parts := seedQCItem(t, db, 1)
	if _, err := repo.Batch.ScrapBatchItems([]uint{parts[0].ID}, "QC fail", nil, time.Now()); err != nil {
		t.Fatalf("scrap: %v", err)
	}

	_, err := qc.Pass(Actor{ID: 5, Role: models.RoleQC}, QCDecisionInput{ScanRef: ScanRef{Code: item.InternalCode}})
	if err == nil {
		t.Fatal("sản phẩm không còn phần sống thì không QC được")
	}
	if !strings.Contains(err.Error(), "huỷ") {
		t.Fatalf("thông báo phải nói rõ phần sản xuất đã bị huỷ, got %v", err)
	}
}
