package services

import (
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// newScrapDB is newSplitDB plus the tables the scrap flow touches: ghi chú, bản
// ghi QC và kiện đóng gói (cửa "đơn đã bắt đầu đóng gói chưa").
func newScrapDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.User{}, &models.Seller{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{}, &models.ItemAsset{},
		&models.Batch{}, &models.BatchItem{}, &models.BatchLink{},
		&models.StatusHistory{}, &models.AuditLog{}, &models.Note{}, &models.QCRecord{},
		&models.Package{}, &models.PackageItem{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// advanceBatch đẩy header + mọi phần của batch tới một trạng thái sản xuất, bỏ
// qua bảng sản xuất (các test ở đây quan tâm tới việc huỷ, không tới cửa link).
func advanceBatch(t *testing.T, db *gorm.DB, batchID uint, status models.InternalStatus) {
	t.Helper()
	if err := db.Model(&models.Batch{}).Where("id = ?", batchID).Update("status", status).Error; err != nil {
		t.Fatalf("advance batch: %v", err)
	}
	if err := db.Model(&models.BatchItem{}).Where("batch_id = ?", batchID).Update("status", status).Error; err != nil {
		t.Fatalf("advance parts: %v", err)
	}
}

func loadBatch(t *testing.T, db *gorm.DB, id uint) models.Batch {
	t.Helper()
	var b models.Batch
	if err := db.First(&b, id).Error; err != nil {
		t.Fatalf("load batch %d: %v", id, err)
	}
	return b
}

// TestScrapBatch_WritesOffProducedPartsAndClosesBatch là lời hứa chính của lệnh
// huỷ: tấm đã in/cắt hỏng được ghi bỏ KÈM lý do và người huỷ, batch đóng lại, và
// sản phẩm quay về hàng chờ gom batch để làm lại ở một batch mới.
func TestScrapBatch_WritesOffProducedPartsAndClosesBatch(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	designer := Actor{ID: 1, Role: models.RoleDesigner}
	prod := Actor{ID: 3, Role: models.RoleProduction}
	_, ids := seedSplit(t, db, 0, 2)

	batchAll, _, err := svc.Create(designer, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})

	batch := firstBatch(t, batchAll, err)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	advanceBatch(t, db, batch.ID, models.StatusCut)

	res, err := svc.Scrap(prod, batch.ID, ScrapBatchInput{Reason: "cắt lệch cả tấm"})
	if err != nil {
		t.Fatalf("scrap: %v", err)
	}
	if res.ScrappedParts != 2 {
		t.Fatalf("scrapped parts = %d, want 2", res.ScrappedParts)
	}

	var parts []models.BatchItem
	if err := db.Where("batch_id = ?", batch.ID).Find(&parts).Error; err != nil {
		t.Fatalf("load parts: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("phần sản xuất phải ở lại batch làm bằng chứng, còn %d", len(parts))
	}
	for _, p := range parts {
		if p.ScrappedAt == nil || p.ScrapReason != "cắt lệch cả tấm" {
			t.Fatalf("phần %d phải được ghi bỏ kèm lý do, got %+v", p.ID, p)
		}
		if p.ScrappedByID == nil || *p.ScrappedByID != prod.ID {
			t.Fatalf("phần %d phải ghi ai huỷ", p.ID)
		}
	}

	closed := loadBatch(t, db, batch.ID)
	if closed.ClosedAt == nil {
		t.Fatal("batch phải được đóng — không còn gì để sản xuất")
	}
	if !strings.Contains(closed.CloseReason, "cắt lệch cả tấm") {
		t.Fatalf("close_reason phải nhắc lý do, got %q", closed.CloseReason)
	}

	var items []models.OrderItem
	db.Find(&items, ids)
	for _, it := range items {
		if it.InternalStatus != models.StatusPending {
			t.Fatalf("sản phẩm %d phải về PENDING để gom batch lại, got %s", it.ID, it.InternalStatus)
		}
		if it.ReworkCount != 1 {
			t.Fatalf("sản phẩm %d rework_count = %d, want 1", it.ID, it.ReworkCount)
		}
		if it.DesignStatus != models.DesignReady {
			t.Fatalf("lỗi sản xuất không được đẩy sản phẩm về hàng chờ thiết kế, got %s", it.DesignStatus)
		}
	}

	// Đúng MỘT ghi chú cho cả lần huỷ — một tấm hỏng là một sự kiện, không phải
	// hai (hay bốn mươi) việc cần xử lý giống hệt nhau.
	var notes []models.Note
	db.Find(&notes)
	if len(notes) != 1 {
		t.Fatalf("want 1 note cho cả lần huỷ, got %d", len(notes))
	}
	if notes[0].EntityType != models.EntityBatch || notes[0].EntityID == nil || *notes[0].EntityID != batch.ID {
		t.Fatalf("ghi chú phải gắn vào batch, got %s/%v", notes[0].EntityType, notes[0].EntityID)
	}
	if notes[0].OwnerRole != models.RoleProduction || !notes[0].IsRequiredAttention {
		t.Fatalf("ghi chú phải là việc cần xử lý của sản xuất, got %+v", notes[0])
	}

	// Huỷ lần hai không được ghi đè gì.
	if _, err := svc.Scrap(prod, batch.ID, ScrapBatchInput{Reason: "lại lỗi"}); err == nil {
		t.Fatal("huỷ một batch đã đóng phải bị từ chối")
	}

	// Và đây là điểm cuối: hàng làm lại được ở batch MỚI.
	againAll, skipped, err := svc.Create(designer, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	again := firstBatch(t, againAll, err)
	if err != nil {
		t.Fatalf("gom batch lại sau khi huỷ: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("gom lại bị bỏ qua %v, want none", skipped)
	}
	var attempts []int
	db.Model(&models.BatchItem{}).Where("batch_id = ?", again.ID).Pluck("attempt", &attempts)
	for _, a := range attempts {
		if a != 2 {
			t.Fatalf("lần sản xuất mới phải là attempt 2, got %d", a)
		}
	}
}

// TestScrapBatch_RequiresAReason: một lần huỷ vật lý không lý do thì tháng sau
// không ai tra được vì sao mất tấm đó.
func TestScrapBatch_RequiresAReason(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 1)
	batchAll, _, batchErr := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, batchErr)
	advanceBatch(t, db, batch.ID, models.StatusCut)
	actor := Actor{ID: 3, Role: models.RoleProduction}

	if _, err := svc.Scrap(actor, batch.ID, ScrapBatchInput{Reason: "   "}); err == nil {
		t.Fatal("huỷ không lý do phải bị từ chối")
	}
	if _, err := svc.Scrap(actor, batch.ID, ScrapBatchInput{Reason: strings.Repeat("a", 61)}); err == nil {
		t.Fatal("lý do dài hơn 60 ký tự phải bị từ chối")
	}
	if _, err := svc.Scrap(actor, batch.ID, ScrapBatchInput{Reason: strings.Repeat("a", 60)}); err != nil {
		t.Fatalf("lý do đúng 60 ký tự phải chấp nhận: %v", err)
	}
}

// TestScrapBatch_SendsUntouchedBatchToDelete: batch chưa ai đụng thì XOÁ, không
// huỷ — huỷ ghi nhận vật liệu đã tiêu và đẩy rework_count lên, cả hai đều sai
// khi chưa có tấm nào được in.
func TestScrapBatch_SendsUntouchedBatchToDelete(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 2)
	batchAll, _, batchErr := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, batchErr)
	_, err := svc.Scrap(Actor{ID: 3, Role: models.RoleProduction}, batch.ID, ScrapBatchInput{Reason: "gom nhầm"})
	if err == nil {
		t.Fatal("batch chưa sản xuất phải được chỉ sang lệnh xoá")
	}
	if !strings.Contains(err.Error(), "Xoá batch") {
		t.Fatalf("thông báo phải chỉ đường sang Xoá batch, got %v", err)
	}
	// Và lệnh được chỉ sang phải thật sự chạy được.
	if err := svc.Delete(Actor{ID: 1, Role: models.RoleDesigner}, batch.ID); err != nil {
		t.Fatalf("xoá batch chưa sản xuất: %v", err)
	}
}

// TestScrapBatch_PendingBatchWithAScrappedPartIsNotStuck đóng đúng cái kẽ giữa
// hai guard: batch còn PENDING nhưng đã có phần bị huỷ ở QC thì XOÁ từ chối (có
// phần đã huỷ) — nếu HUỶ cũng từ chối (vì header còn PENDING) thì batch kẹt
// vĩnh viễn, không xoá được mà cũng không huỷ được.
func TestScrapBatch_PendingBatchWithAScrappedPartIsNotStuck(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	repo := repositories.New(db)
	_, ids := seedSplit(t, db, 0, 2)
	batchAll, _, batchErr := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, batchErr)
	var first models.BatchItem
	db.Where("batch_id = ?", batch.ID).Order("id asc").First(&first)
	if _, err := repo.Batch.ScrapBatchItems([]uint{first.ID}, "QC fail", nil, time.Now()); err != nil {
		t.Fatalf("seed scrapped part: %v", err)
	}

	if err := svc.Delete(Actor{ID: 1, Role: models.RoleDesigner}, batch.ID); err == nil {
		t.Fatal("xoá phải từ chối khi batch đã có phần bị huỷ")
	}
	if _, err := svc.Scrap(Actor{ID: 3, Role: models.RoleProduction}, batch.ID, ScrapBatchInput{Reason: "bỏ nốt tấm"}); err != nil {
		t.Fatalf("huỷ phải chạy được, nếu không batch kẹt giữa hai cửa: %v", err)
	}
	if loadBatch(t, db, batch.ID).ClosedAt == nil {
		t.Fatal("batch phải đóng sau khi huỷ")
	}
}

// TestScrapBatch_BlockedOnceOrderStartedPacking: quét món đầu tiên ở trạm đóng
// gói đã mở kiện cho CẢ đơn, nên từ đó không kéo hàng về "chờ làm lại" được nữa
// — và thông báo phải gọi tên ĐƠN, vì ranh giới là của đơn chứ không của món.
func TestScrapBatch_BlockedOnceOrderStartedPacking(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 2)
	batchAll, _, batchErr := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, batchErr)
	advanceBatch(t, db, batch.ID, models.StatusCut)

	var order models.Order
	db.First(&order)
	if err := db.Create(&models.Package{Code: "PKG-000001", OrderID: order.ID, Status: models.PackageOpen}).Error; err != nil {
		t.Fatalf("seed package: %v", err)
	}

	_, err := svc.Scrap(Actor{ID: 3, Role: models.RoleProduction}, batch.ID, ScrapBatchInput{Reason: "cắt hỏng"})
	if err == nil {
		t.Fatal("huỷ phải bị chặn khi đơn đã bắt đầu đóng gói")
	}
	if !strings.Contains(err.Error(), order.InternalCode) || !strings.Contains(err.Error(), "đóng gói") {
		t.Fatalf("thông báo phải gọi tên đơn đang chặn, got %v", err)
	}
	if loadBatch(t, db, batch.ID).ClosedAt != nil {
		t.Fatal("lần huỷ bị chặn không được đóng batch")
	}
}

// TestScrapBatch_DesignRouteSendsItemsBackToDesign: cả tấm sai vì file sai thì
// in lại y hệt sẽ hỏng y hệt.
func TestScrapBatch_DesignRouteSendsItemsBackToDesign(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 2)
	batchAll, _, batchErr := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, batchErr)
	advanceBatch(t, db, batch.ID, models.StatusPrinted)

	res, err := svc.Scrap(Actor{ID: 5, Role: models.RoleQC}, batch.ID,
		ScrapBatchInput{Reason: "file in sai toàn tấm", Route: ReworkToDesign})
	if err != nil {
		t.Fatalf("scrap: %v", err)
	}
	if res.Route != ReworkToDesign {
		t.Fatalf("route = %s, want DESIGN", res.Route)
	}
	var items []models.OrderItem
	db.Find(&items, ids)
	for _, it := range items {
		if it.DesignStatus != models.DesignMissing {
			t.Fatalf("sản phẩm %d phải về hàng chờ thiết kế, got %s", it.ID, it.DesignStatus)
		}
	}
	var note models.Note
	db.First(&note)
	if note.OwnerRole != models.RoleDesigner {
		t.Fatalf("ghi chú phải giao cho designer, got %s", note.OwnerRole)
	}
}

// TestScrapBatch_ComboReopensQCForTheWholeProduct là lỗ hổng thật của hàng combo:
// QC pass là cửa mức SẢN PHẨM, một lần pass đẩy mọi phần NVL lên QC_PASSED. Huỷ
// riêng batch gỗ mà để phần mica nguyên "đã QC" thì roll-up của sản phẩm (min
// của các phần CÒN SỐNG) vẫn ra QC_PASSED — sản phẩm đi thẳng sang đóng gói
// trong khi phần gỗ còn đang làm lại.
func TestScrapBatch_ComboReopensQCForTheWholeProduct(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	item, wood, mica := seedComboItem(t, db)

	// Cả sản phẩm đã QC pass: mọi phần và mọi batch đều QC_PASSED.
	db.Model(&models.BatchItem{}).Where("order_item_id = ?", item.ID).Update("status", models.StatusQCPassed)
	db.Model(&models.Batch{}).Where("id IN ?", []uint{wood.ID, mica.ID}).Update("status", models.StatusQCPassed)
	db.Model(&models.OrderItem{}).Where("id = ?", item.ID).Update("internal_status", models.StatusQCPassed)

	if _, err := svc.Scrap(Actor{ID: 3, Role: models.RoleProduction}, wood.ID, ScrapBatchInput{Reason: "tấm gỗ nứt"}); err != nil {
		t.Fatalf("huỷ batch gỗ: %v", err)
	}

	var reloaded models.OrderItem
	db.First(&reloaded, item.ID)
	if reloaded.InternalStatus == models.StatusQCPassed {
		t.Fatal("sản phẩm còn phần đang làm lại thì KHÔNG được ở QC_PASSED — nó sẽ lọt sang đóng gói")
	}
	var micaPart models.BatchItem
	db.Where("batch_id = ?", mica.ID).First(&micaPart)
	if micaPart.Status != models.StatusCut {
		t.Fatalf("phần mica phải bị hạ khỏi 'đã QC' về CUT, got %s", micaPart.Status)
	}
	if micaPart.ScrappedAt != nil {
		t.Fatal("phần mica còn tốt, không được ghi bỏ")
	}
	if b := loadBatch(t, db, mica.ID); b.Status != models.StatusCut {
		t.Fatalf("batch mica phải theo phần của nó về CUT, got %s", b.Status)
	}
}

// TestScrapBatch_ClosedBatchRefusesStatusAndLinkEdits: batch đã đóng là lịch sử.
func TestScrapBatch_ClosedBatchRefusesStatusAndLinkEdits(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 1)
	batchAll, _, batchErr := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, batchErr)
	advanceBatch(t, db, batch.ID, models.StatusPrinted)
	if _, err := svc.Scrap(Actor{ID: 3, Role: models.RoleProduction}, batch.ID, ScrapBatchInput{Reason: "in nhoè"}); err != nil {
		t.Fatalf("scrap: %v", err)
	}

	if _, err := svc.UpdateStatus(Actor{ID: 3, Role: models.RoleProduction}, batch.ID,
		UpdateStatusInput{Status: string(models.StatusCut)}); err == nil {
		t.Fatal("batch đã đóng không được đổi trạng thái nữa")
	}
	if _, err := svc.SetBatchLink(Actor{ID: 1, Role: models.RoleDesigner}, batch.ID,
		SetBatchLinkInput{Kind: "PRINT", URL: "https://files.example.com/p.pdf"}); err == nil {
		t.Fatal("batch đã đóng không được sửa link sản xuất nữa")
	}
}

// TestSetBatchLink_LeavesScrappedPartsAlone: sản phẩm có phần đã bị huỷ đang chờ
// làm lại ở batch khác — sửa link của batch cũ không được đóng dấu URL đó lên nó.
func TestSetBatchLink_LeavesScrappedPartsAlone(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	repo := repositories.New(db)
	_, ids := seedSplit(t, db, 0, 2)
	batchAll, _, batchErr := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, batchErr)
	var scrappedPart models.BatchItem
	db.Where("batch_id = ?", batch.ID).Order("id asc").First(&scrappedPart)
	if _, err := repo.Batch.ScrapBatchItems([]uint{scrappedPart.ID}, "QC fail", nil, time.Now()); err != nil {
		t.Fatalf("seed scrapped part: %v", err)
	}

	if _, err := svc.SetBatchLink(Actor{ID: 1, Role: models.RoleDesigner}, batch.ID,
		SetBatchLinkInput{Kind: "PRINT", URL: "https://files.example.com/print.pdf"}); err != nil {
		t.Fatalf("set link: %v", err)
	}

	var scrappedItem models.OrderItem
	db.First(&scrappedItem, scrappedPart.OrderItemID)
	if scrappedItem.PrintFileURL != "" {
		t.Fatalf("sản phẩm có phần đã huỷ không được nhận lại link của batch cũ, got %q", scrappedItem.PrintFileURL)
	}
	var live int64
	db.Model(&models.OrderItem{}).Where("print_file_url <> ''").Count(&live)
	if live != 1 {
		t.Fatalf("chỉ sản phẩm còn phần sống mới được đóng dấu link, got %d", live)
	}
}

// TestScrapBatch_ExportStillListsWhatWasProduced: batch đã đóng không còn phần
// nào sống, nên export của nó ra file rỗng — đúng lúc người ta cần nó nhất để
// đối chiếu tấm vừa huỷ đã có những gì.
func TestScrapBatch_ExportStillListsWhatWasProduced(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 2)
	batchAll, _, batchErr := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, batchErr)
	advanceBatch(t, db, batch.ID, models.StatusCut)
	if _, err := svc.Scrap(Actor{ID: 3, Role: models.RoleProduction}, batch.ID, ScrapBatchInput{Reason: "cắt hỏng"}); err != nil {
		t.Fatalf("scrap: %v", err)
	}

	exported, err := svc.GetWithScrapHistory(batch.ID)
	if err != nil {
		t.Fatalf("get for export: %v", err)
	}
	grid := ProductionTemplateGrid(exported)
	if len(grid) != 3 { // header + 2 sản phẩm
		t.Fatalf("bảng sản xuất của batch đã huỷ phải còn liệt kê hàng đã làm, got %d dòng", len(grid))
	}
	if _, _, err := svc.ProductionTemplateXLSX(batch.ID); err != nil {
		t.Fatalf("xuất xlsx batch đã huỷ: %v", err)
	}
}

// seedComboItem dựng một sản phẩm combo hai NVL: mỗi NVL một batch riêng (batch
// luôn theo NVL), mỗi batch một phần. Đây đúng là hình dạng làm lộ lỗ hổng un-QC
// — hàng một NVL không bao giờ gặp.
func seedComboItem(t *testing.T, db *gorm.DB) (*models.OrderItem, *models.Batch, *models.Batch) {
	t.Helper()
	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	wood := &models.Material{Code: "WOOD", Name: "Gỗ"}
	mica := &models.Material{Code: "MICA-2", Name: "Mica"}
	must(db.Create(wood).Error, "material gỗ")
	must(db.Create(mica).Error, "material mica")
	sku := &models.SKU{Code: "COMBO-1", Name: "Đèn combo", IsCombo: true}
	must(db.Create(sku).Error, "sku")
	must(db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: wood.ID, QuantityPerUnit: 1}).Error, "sku-gỗ")
	must(db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mica.ID, QuantityPerUnit: 1}).Error, "sku-mica")
	order := &models.Order{
		InternalCode: "100077", StoreOrderID: "Etsy-77", SellerID: 1,
		SellerStatus: models.SellerStatusProduction, ReviewStatus: models.ReviewApproved,
	}
	must(db.Create(order).Error, "order")
	item := &models.OrderItem{
		OrderID: order.ID, LineNo: 1, InternalCode: "100077_1/1", SKUID: &sku.ID, SKUCode: sku.Code,
		Quantity: 1, MockupURL: "https://example.com/mockup.png",
		InternalStatus: models.StatusCut, DesignStatus: models.DesignReady,
	}
	must(db.Create(item).Error, "item")
	mk := func(m *models.Material, code string) *models.Batch {
		b := &models.Batch{Code: code, MaterialID: m.ID, Status: models.StatusCut}
		must(db.Create(b).Error, "batch "+code)
		must(db.Create(&models.BatchItem{
			BatchID: b.ID, OrderItemID: item.ID, MaterialID: m.ID,
			Status: models.StatusCut, Attempt: 1,
		}).Error, "part "+code)
		return b
	}
	return item, mk(wood, "#101901"), mk(mica, "#101902")
}

// TestScrapBatch_UnstampsProductionLinks: link in/cắt là của TẤM. Tấm chết thì
// dấu của nó phải rời khỏi sản phẩm, nếu không sản phẩm mang link của tấm đã vứt
// sang batch mới và export batch mới sẽ chỉ thợ in đúng file vừa làm hỏng cả tấm.
func TestScrapBatch_UnstampsProductionLinks(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	designer := Actor{ID: 1, Role: models.RoleDesigner}
	_, ids := seedSplit(t, db, 0, 2)
	batchAll, _, batchErr := svc.Create(designer, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, batchErr)
	if _, err := svc.SetBatchLink(designer, batch.ID, SetBatchLinkInput{
		Kind: "PRINT", URL: "https://files.example.com/print-1.pdf",
	}); err != nil {
		t.Fatalf("set link: %v", err)
	}
	advanceBatch(t, db, batch.ID, models.StatusCut)

	if _, err := svc.Scrap(Actor{ID: 3, Role: models.RoleProduction}, batch.ID,
		ScrapBatchInput{Reason: "cắt hỏng"}); err != nil {
		t.Fatalf("scrap: %v", err)
	}

	var items []models.OrderItem
	db.Find(&items, ids)
	for _, it := range items {
		if it.PrintFileURL != "" {
			t.Fatalf("sản phẩm %d còn giữ link in của tấm đã huỷ: %q", it.ID, it.PrintFileURL)
		}
	}
}

// TestQCFail_UnstampsOnlyTheFailedItem: QC fail huỷ MỘT sản phẩm trong tấm, các
// sản phẩm còn lại vẫn đang được làm ra từ đúng file đó — gỡ link của cả batch
// sẽ cướp mất link hợp lệ của chúng.
func TestQCFail_UnstampsOnlyTheFailedItem(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	repo := repositories.New(db)
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}}
	designer := Actor{ID: 1, Role: models.RoleDesigner}
	_, ids := seedSplit(t, db, 0, 2)
	batchAll, _, batchErr := svc.Create(designer, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, batchErr)
	if _, err := svc.SetBatchLink(designer, batch.ID, SetBatchLinkInput{
		Kind: "PRINT", URL: "https://files.example.com/print-1.pdf",
	}); err != nil {
		t.Fatalf("set link: %v", err)
	}
	advanceBatch(t, db, batch.ID, models.StatusCut)

	failedID := ids[0]
	var failedItem models.OrderItem
	db.First(&failedItem, failedID)
	if _, err := qc.Fail(Actor{ID: 5, Role: models.RoleQC}, QCDecisionInput{
		ScanRef: ScanRef{ItemID: &failedID}, DefectCode: "CUT_WRONG",
	}); err != nil {
		t.Fatalf("QC fail: %v", err)
	}

	var failed, healthy models.OrderItem
	db.First(&failed, ids[0])
	db.First(&healthy, ids[1])
	if failed.PrintFileURL != "" {
		t.Fatalf("sản phẩm bị huỷ phải rời khỏi link của tấm cũ, got %q", failed.PrintFileURL)
	}
	if healthy.PrintFileURL == "" {
		t.Fatal("sản phẩm còn tốt trong cùng tấm phải giữ nguyên link in — nó vẫn đang được làm từ file đó")
	}
}
