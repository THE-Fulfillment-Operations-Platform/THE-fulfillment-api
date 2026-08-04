package services

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// seedQCItem builds an approved order + one produced item sitting in a batch,
// i.e. exactly what QC scans: a finished piece waiting for the pass/fail call.
func seedQCItem(t *testing.T, db *gorm.DB, materialIDs ...uint) (*models.OrderItem, []*models.BatchItem) {
	t.Helper()
	order := &models.Order{
		InternalCode: "ORD-1", StoreOrderID: "US-1", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	item := &models.OrderItem{
		OrderID: order.ID, LineNo: 1, InternalCode: "ORD-1_1/1", SKUCode: "SKU-1", Quantity: 1,
		MockupURL: "https://example.com/m.png", InternalStatus: models.StatusCut,
		DesignStatus: models.DesignReady,
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	parts := make([]*models.BatchItem, 0, len(materialIDs))
	for i, mid := range materialIDs {
		b := &models.Batch{Code: "B-10100" + string(rune('1'+i)), MaterialID: mid, Status: models.StatusCut}
		if err := db.Create(b).Error; err != nil {
			t.Fatalf("seed batch: %v", err)
		}
		bi := &models.BatchItem{
			BatchID: b.ID, OrderItemID: item.ID, MaterialID: mid,
			Status: models.StatusCut, Attempt: 1,
		}
		if err := db.Create(bi).Error; err != nil {
			t.Fatalf("seed batch item: %v", err)
		}
		bi.Batch = b
		parts = append(parts, bi)
	}
	return item, parts
}

// TestQCFail_ScrapsPartAndReturnsItemForRework is the core of the rework flow.
//
// Before this existed a QC fail only wrote a note: the defective part stayed in
// its batch, so (a) the batch could never reach QC_PASSED and sat on the board
// forever, and (b) the item still counted as "already batched" and could never be
// produced again. Both must now be true instead: the batch closes, and the item
// is back in the pool waiting to be put into a NEW batch.
func TestQCFail_ScrapsPartAndReturnsItemForRework(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}
	actor := Actor{ID: 7}

	item, parts := seedQCItem(t, db, 1)
	failedPart := parts[0]

	res, err := qc.Fail(actor, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode}, DefectCode: "CUT_WRONG", Note: "Mẻ cạnh",
	})
	if err != nil {
		t.Fatalf("QC fail: %v", err)
	}
	if res.Route != ReworkToProduction {
		t.Fatalf("lỗi cắt là lỗi sản xuất → route = %s, want PRODUCTION", res.Route)
	}
	if res.Note == nil || !res.Note.IsRequiredAttention || res.Note.OwnerRole != models.RoleProduction {
		t.Fatalf("phải mở note cần chú ý giao cho sản xuất, got %+v", res.Note)
	}

	// 1. Phần hỏng bị huỷ nhưng VẪN nằm ở batch đã làm ra nó.
	var scrapped models.BatchItem
	if err := db.First(&scrapped, failedPart.ID).Error; err != nil {
		t.Fatalf("load part: %v", err)
	}
	if scrapped.ScrappedAt == nil || scrapped.ScrapReason != "CUT_WRONG" {
		t.Fatalf("phần hỏng phải được đánh dấu huỷ kèm lý do, got %+v", scrapped)
	}
	if scrapped.BatchID != failedPart.BatchID {
		t.Fatalf("phần hỏng phải ở nguyên batch cũ để truy vết được")
	}

	// 2. Batch cũ không còn bị treo: phần đã huỷ không tính vào roll-up nữa.
	live, err := repo.Batch.BatchItemsForBatch(failedPart.BatchID)
	if err != nil {
		t.Fatalf("live parts: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("phần đã huỷ không được tính vào trạng thái batch, còn %d", len(live))
	}

	// 3. Item quay lại "chưa sản xuất" nên không thể lọt sang đóng gói.
	var reloaded models.OrderItem
	db.First(&reloaded, item.ID)
	if reloaded.InternalStatus == models.StatusQCPassed {
		t.Fatalf("item vừa QC fail không được ở trạng thái QC_PASSED")
	}
	if reloaded.ReworkCount != 1 {
		t.Fatalf("rework_count = %d, want 1", reloaded.ReworkCount)
	}
	// Lỗi sản xuất thì file design giữ nguyên — không bắt designer làm lại.
	if reloaded.DesignStatus != models.DesignReady {
		t.Fatalf("lỗi sản xuất không được đẩy item về hàng chờ thiết kế, got %s", reloaded.DesignStatus)
	}

	// 4. Lần sản xuất mới không đụng ràng buộc unique (item, material) cũ.
	next, err := repo.Batch.NextAttempts(1, []uint{item.ID})
	if err != nil {
		t.Fatalf("next attempt: %v", err)
	}
	if next[item.ID] != 2 {
		t.Fatalf("lần sản xuất tiếp theo = %d, want 2", next[item.ID])
	}
	newBatch := &models.Batch{Code: "B-999", MaterialID: 1, Status: models.StatusPending}
	db.Create(newBatch)
	if err := db.Create(&models.BatchItem{
		BatchID: newBatch.ID, OrderItemID: item.ID, MaterialID: 1,
		Status: models.StatusPending, Attempt: next[item.ID],
	}).Error; err != nil {
		t.Fatalf("phải tạo được phần sản xuất lần 2 cho cùng item+NVL: %v", err)
	}
}

// TestQCFail_DesignDefectGoesBackToDesign: lỗi từ file thì in lại y hệt sẽ hỏng
// y hệt — item phải quay về hàng chờ thiết kế trước khi được gom batch lại.
func TestQCFail_DesignDefectGoesBackToDesign(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}

	item, _ := seedQCItem(t, db, 1)
	res, err := qc.Fail(Actor{ID: 1}, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode}, DefectCode: "ENGRAVE_WRONG",
	})
	if err != nil {
		t.Fatalf("QC fail: %v", err)
	}
	if res.Route != ReworkToDesign {
		t.Fatalf("khắc sai nội dung là lỗi file → route = %s, want DESIGN", res.Route)
	}
	if res.Note.OwnerRole != models.RoleDesigner {
		t.Fatalf("note phải giao cho designer, got %s", res.Note.OwnerRole)
	}
	var reloaded models.OrderItem
	db.First(&reloaded, item.ID)
	if reloaded.DesignStatus == models.DesignReady {
		t.Fatalf("item phải rời trạng thái design ready để designer sửa file")
	}
}

// TestQCFail_ComboRequiresChoosingTheDefectivePart: combo hỏng một mặt thì chỉ
// làm lại mặt đó — không được âm thầm huỷ cả bộ (tốn gấp đôi vật tư).
func TestQCFail_ComboRequiresChoosingTheDefectivePart(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}

	item, parts := seedQCItem(t, db, 1, 2)

	if _, err := qc.Fail(Actor{ID: 1}, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode}, DefectCode: "MATERIAL_DEFECT",
	}); err == nil {
		t.Fatalf("combo nhiều NVL mà không chỉ rõ phần hỏng thì phải bị chặn")
	}

	mica := parts[1]
	if _, err := qc.Fail(Actor{ID: 1}, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode}, BatchItemID: &mica.ID, DefectCode: "MATERIAL_DEFECT",
	}); err != nil {
		t.Fatalf("chọn đúng phần hỏng thì phải chạy: %v", err)
	}

	var wood, scrappedMica models.BatchItem
	db.First(&wood, parts[0].ID)
	db.First(&scrappedMica, mica.ID)
	if wood.ScrappedAt != nil {
		t.Fatalf("phần đạt không được huỷ theo")
	}
	if scrappedMica.ScrappedAt == nil {
		t.Fatalf("phần hỏng phải bị huỷ")
	}
}

// TestQCFail_ThenReworkPass_ClosesTheNote: làm lại xong và QC đạt thì ghi chú
// theo dõi phải tự đóng — để màn Ghi chú chỉ còn việc thật sự cần xử lý.
func TestQCFail_ThenReworkPass_ClosesTheNote(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}
	actor := Actor{ID: 3}

	item, parts := seedQCItem(t, db, 1)
	if _, err := qc.Fail(actor, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode}, DefectCode: "PRINT_WRONG",
	}); err != nil {
		t.Fatalf("QC fail: %v", err)
	}

	// Làm lại: phần sản xuất lần 2 trong một batch mới, đã cắt xong.
	next, _ := repo.Batch.NextAttempts(1, []uint{item.ID})
	newBatch := &models.Batch{Code: "B-REWORK", MaterialID: parts[0].MaterialID, Status: models.StatusCut}
	db.Create(newBatch)
	db.Create(&models.BatchItem{
		BatchID: newBatch.ID, OrderItemID: item.ID, MaterialID: parts[0].MaterialID,
		Status: models.StatusCut, Attempt: next[item.ID],
	})

	if _, err := qc.Pass(actor, QCDecisionInput{ScanRef: ScanRef{Code: item.InternalCode}}); err != nil {
		t.Fatalf("QC pass lần làm lại: %v", err)
	}

	var open int64
	db.Model(&models.Note{}).
		Where("entity_id = ? AND status <> ?", item.ID, models.NoteResolved).Count(&open)
	if open != 0 {
		t.Fatalf("còn %d ghi chú QC fail chưa đóng sau khi làm lại đạt", open)
	}
	// Và batch làm lại đóng được, batch cũ (chỉ còn phần đã huỷ) không kẹt.
	var reworked models.Batch
	db.First(&reworked, newBatch.ID)
	if reworked.Status != models.StatusQCPassed {
		t.Fatalf("batch làm lại phải lên QC_PASSED, got %s", reworked.Status)
	}
}

// TestCreateBatch_AcceptsReworkedItem khoá lại đúng con bug người dùng gặp:
// sau QC fail, màn "tạo batch" LIỆT KÊ sản phẩm (truy vấn bucket đã bỏ qua phần
// đã huỷ) nhưng khi bấm tạo thì API trả "không có sản phẩm hợp lệ" — vì phép
// kiểm tra "đã nằm trong batch" ở đường tạo batch vẫn đếm cả phần đã huỷ. Hai
// định nghĩa "đã batch" phải giống nhau, nếu không màn hình mời một đằng, server
// từ chối một nẻo.
func TestCreateBatch_AcceptsReworkedItem(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}
	batchSvc := &BatchService{repo: repo, audit: &AuditService{repo: repo}}
	actor := Actor{ID: 5}

	mat := &models.Material{Code: "MICA", Name: "Mica"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	item, _ := seedQCItem(t, db, mat.ID)
	// SKU của item phải khai NVL này thì mới gom batch được.
	sku := &models.SKU{Code: "SKU-1", Name: "SKU"}
	db.Create(sku)
	db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mat.ID, QuantityPerUnit: 1})
	db.Model(&models.OrderItem{}).Where("id = ?", item.ID).Update("sku_id", sku.ID)

	if _, err := qc.Fail(actor, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode}, DefectCode: "PRINT_WRONG",
	}); err != nil {
		t.Fatalf("QC fail: %v", err)
	}

	batch, skipped, err := batchSvc.Create(actor, CreateBatchInput{
		MaterialID: mat.ID, OrderItemIDs: []uint{item.ID},
	})
	if err != nil {
		t.Fatalf("phải gom được sản phẩm đã QC fail vào batch mới: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("không được bỏ qua sản phẩm nào, skipped=%v", skipped)
	}

	// Phần mới là lần sản xuất thứ 2, và phần hỏng vẫn nằm ở batch cũ.
	var parts []models.BatchItem
	db.Where("order_item_id = ?", item.ID).Order("attempt asc").Find(&parts)
	if len(parts) != 2 {
		t.Fatalf("want 2 lần sản xuất (1 huỷ + 1 làm lại), got %d", len(parts))
	}
	if parts[0].ScrappedAt == nil || parts[1].ScrappedAt != nil {
		t.Fatalf("lần 1 phải là bản huỷ, lần 2 phải còn hiệu lực: %+v", parts)
	}
	if parts[1].Attempt != 2 || parts[1].BatchID != batch.ID {
		t.Fatalf("phần làm lại phải là attempt 2 trong batch mới, got attempt=%d batch=%d",
			parts[1].Attempt, parts[1].BatchID)
	}
}

// TestQCFail_RepeatedRoundsStayConsistent là yêu cầu "fail 100 lần vẫn phải chạy
// như fail 1 lần": mỗi vòng huỷ–làm lại phải để hệ thống ở đúng một trạng thái
// như nhau, không tích rác. Cụ thể sau mỗi vòng: đúng MỘT phần còn hiệu lực,
// số lần sản xuất tăng đều, batch cũ tự đóng (không còn nằm chờ trên bảng), và
// sản phẩm luôn quay lại đúng hàng chờ gom batch.
func TestQCFail_RepeatedRoundsStayConsistent(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}
	batchSvc := &BatchService{repo: repo, audit: &AuditService{repo: repo}}
	actor := Actor{ID: 9}

	mat := &models.Material{Code: "MICA", Name: "Mica"}
	db.Create(mat)
	item, parts := seedQCItem(t, db, mat.ID)
	sku := &models.SKU{Code: "SKU-1", Name: "SKU"}
	db.Create(sku)
	db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mat.ID, QuantityPerUnit: 1})
	db.Model(&models.OrderItem{}).Where("id = ?", item.ID).Update("sku_id", sku.ID)

	firstBatchID := parts[0].BatchID
	const rounds = 5
	for round := 1; round <= rounds; round++ {
		if _, err := qc.Fail(actor, QCDecisionInput{
			ScanRef: ScanRef{Code: item.InternalCode}, DefectCode: "PRINT_WRONG",
		}); err != nil {
			t.Fatalf("vòng %d — QC fail: %v", round, err)
		}

		// Batch vừa hỏng phải tự đóng: không còn gì để sản xuất ở đó nữa.
		batchID := firstBatchID
		if round > 1 {
			var last models.BatchItem
			db.Where("order_item_id = ? AND attempt = ?", item.ID, round).First(&last)
			batchID = last.BatchID
		}
		var closed models.Batch
		db.First(&closed, batchID)
		if closed.ClosedAt == nil {
			t.Fatalf("vòng %d — batch %s phải được đóng khi toàn bộ hàng bị huỷ", round, closed.Code)
		}

		// Không được còn phần nào "đang sản xuất" sau khi huỷ.
		var live int64
		db.Model(&models.BatchItem{}).
			Where("order_item_id = ? AND scrapped_at IS NULL", item.ID).Count(&live)
		if live != 0 {
			t.Fatalf("vòng %d — còn %d phần chưa huỷ, phải là 0", round, live)
		}

		// Và luôn gom lại được vào một batch mới — y như vòng đầu tiên.
		batch, skipped, err := batchSvc.Create(actor, CreateBatchInput{
			MaterialID: mat.ID, OrderItemIDs: []uint{item.ID},
		})
		if err != nil || len(skipped) != 0 {
			t.Fatalf("vòng %d — phải gom lại được vào batch mới: err=%v skipped=%v", round, err, skipped)
		}
		// Phần mới là lần sản xuất kế tiếp và là phần DUY NHẤT còn hiệu lực.
		var newPart models.BatchItem
		db.Where("batch_id = ? AND order_item_id = ?", batch.ID, item.ID).First(&newPart)
		if newPart.Attempt != round+1 {
			t.Fatalf("vòng %d — lần sản xuất mới = %d, want %d", round, newPart.Attempt, round+1)
		}
		db.Model(&models.BatchItem{}).
			Where("order_item_id = ? AND scrapped_at IS NULL", item.ID).Count(&live)
		if live != 1 {
			t.Fatalf("vòng %d — phải có đúng 1 phần đang sản xuất, có %d", round, live)
		}
		// Đưa phần mới về trạng thái đã cắt để vòng sau QC được.
		db.Model(&models.BatchItem{}).Where("id = ?", newPart.ID).Update("status", models.StatusCut)
	}

	// Tổng kết: đúng `rounds` lần huỷ + 1 phần đang chạy, và item đếm đủ số lần.
	var scrapped, total int64
	db.Model(&models.BatchItem{}).Where("order_item_id = ? AND scrapped_at IS NOT NULL", item.ID).Count(&scrapped)
	db.Model(&models.BatchItem{}).Where("order_item_id = ?", item.ID).Count(&total)
	if scrapped != rounds || total != rounds+1 {
		t.Fatalf("sau %d vòng: %d phần huỷ / %d phần tổng, want %d/%d", rounds, scrapped, total, rounds, rounds+1)
	}
	var reloaded models.OrderItem
	db.First(&reloaded, item.ID)
	if reloaded.ReworkCount != rounds {
		t.Fatalf("rework_count = %d, want %d", reloaded.ReworkCount, rounds)
	}
	// Và bảng sản xuất chỉ còn đúng batch đang chạy: các batch cũ đều đã đóng.
	var openBatches int64
	db.Model(&models.Batch{}).Where("closed_at IS NULL").Count(&openBatches)
	if openBatches != 1 {
		t.Fatalf("còn %d batch mở, want 1 (chỉ batch đang làm lại)", openBatches)
	}
}
