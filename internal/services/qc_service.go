package services

import (
	"errors"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// QCService implements quality control: scanning an item tem to pull up its
// mockup, then confirming pass (matches mockup) or fail (rework).
type QCService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// ScanRef identifies an item by its tem code or internal id.
type ScanRef struct {
	Code   string `json:"code"`
	ItemID *uint  `json:"item_id"`
}

func (s *QCService) resolveItem(ref ScanRef) (*models.OrderItem, error) {
	if ref.ItemID != nil {
		return s.findItemByID(*ref.ItemID)
	}
	code := strings.TrimSpace(ref.Code)
	if code == "" {
		return nil, apperr.BadRequest("Hãy quét hoặc nhập mã item")
	}
	item, err := s.repo.OrderItem.FindByCode(code)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Không tìm thấy item nào khớp mã vừa quét")
		}
		return nil, apperr.Internal("Không tra cứu được dữ liệu").Wrap(err)
	}
	if itemCancelled(item.CancellationStatus) {
		return nil, apperr.Conflict("Sản phẩm đã huỷ, không thể QC")
	}
	return item, nil
}

func (s *QCService) findItemByID(id uint) (*models.OrderItem, error) {
	item, err := s.repo.OrderItem.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Không tìm thấy item")
		}
		return nil, apperr.Internal("Không tra cứu được dữ liệu").Wrap(err)
	}
	if itemCancelled(item.CancellationStatus) {
		return nil, apperr.Conflict("Sản phẩm đã huỷ, không thể QC")
	}
	return item, nil
}

// QCScanResult is everything the QC station needs to compare product vs mockup.
type QCScanResult struct {
	ItemID         uint                  `json:"item_id"`
	ItemCode       string                `json:"item_code"`
	OrderCode      string                `json:"order_code"`
	StoreOrderID   string                `json:"store_order_id"`
	SKUCode        string                `json:"sku_code"`
	ProductName    string                `json:"product_name"`
	Quantity       int                   `json:"quantity"`
	MaterialName   string                `json:"material_name"`  // Loại VL
	QCDescription  string                `json:"qc_description"` // Mô tả SP để QC
	ImageCode      string                `json:"image_code"`     // Mã ảnh
	EngraveText    string                `json:"engrave_text"`
	DesignURL      string                `json:"design_url"` // Link ảnh / design
	MockupURL      string                `json:"mockup_url"`
	PrintFileURL   string                `json:"print_file_url"`
	CutFileURL     string                `json:"cut_file_url"`
	InternalStatus models.InternalStatus `json:"internal_status"`
	Batches        []QCScanBatch         `json:"batches"`
}

// QCScanBatch is one production part of the item.
type QCScanBatch struct {
	BatchItemID  uint                  `json:"batch_item_id"`
	BatchCode    string                `json:"batch_code"`
	MaterialCode string                `json:"material_code"`
	Status       models.InternalStatus `json:"status"`
}

// Scan returns item/order/sku/batch info plus the seller mockup to compare against.
func (s *QCService) Scan(actor Actor, ref ScanRef) (*QCScanResult, error) {
	item, err := s.resolveItem(ref)
	if err != nil {
		return nil, err
	}
	res := &QCScanResult{
		ItemID: item.ID, ItemCode: item.InternalCode, SKUCode: item.SKUCode,
		ProductName: item.ProductName, Quantity: item.Quantity,
		QCDescription: item.QCDescription, ImageCode: item.ImageCode,
		EngraveText: item.EngraveText, DesignURL: item.DesignURL, MockupURL: item.MockupURL,
		PrintFileURL: item.PrintFileURL, CutFileURL: item.CutFileURL,
		InternalStatus: item.InternalStatus,
	}
	if item.Order != nil {
		res.OrderCode = item.Order.InternalCode
		res.StoreOrderID = item.Order.StoreOrderID
	}
	// Loại VL: the material(s) this item is produced in. Prefer the batch parts
	// (the concrete production material); fall back to the SKU's mapped materials.
	res.MaterialName = itemMaterialNames(item)
	for _, bi := range item.BatchItems {
		b := QCScanBatch{BatchItemID: bi.ID, Status: bi.Status}
		if bi.Batch != nil {
			b.BatchCode = bi.Batch.Code
		}
		if bi.Material != nil {
			b.MaterialCode = bi.Material.Code
		}
		res.Batches = append(res.Batches, b)
	}
	s.audit.Log(actor, "QC_SCAN", "order_item", &item.ID, "Scanned item "+item.InternalCode+" for QC", nil)
	return res, nil
}

// itemMaterialNames returns a comma-separated list of the distinct material
// names an item is produced in — the batch parts' materials if it is batched,
// otherwise the SKU's mapped materials. Requires BatchItems.Material and/or
// SKU.Materials.Material to be preloaded.
func itemMaterialNames(item *models.OrderItem) string {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, bi := range item.BatchItems {
		if bi.Material != nil {
			add(bi.Material.Name)
		}
	}
	if len(names) == 0 && item.SKU != nil {
		for _, sm := range item.SKU.Materials {
			add(sm.Material.Name)
		}
	}
	return strings.Join(names, ", ")
}

// QCDecisionInput confirms a QC outcome for an item.
type QCDecisionInput struct {
	ScanRef
	BatchItemID *uint  `json:"batch_item_id"` // optional: QC a single material part
	DefectCode  string `json:"defect_code"`
	Note        string `json:"note"`
}

// targetBatchItems returns the batch items a decision applies to.
func (s *QCService) targetBatchItems(item *models.OrderItem, batchItemID *uint) ([]models.BatchItem, error) {
	if batchItemID != nil {
		for _, bi := range item.BatchItems {
			if bi.ID == *batchItemID {
				return []models.BatchItem{bi}, nil
			}
		}
		return nil, apperr.BadRequest("Mã batch item không thuộc item này")
	}
	if len(item.BatchItems) == 0 {
		return nil, apperr.Unprocessable("Item chưa được đưa vào batch sản xuất nên chưa thể QC")
	}
	return item.BatchItems, nil
}

// Pass records a QC PASS: the produced item matches the seller's mockup. The
// targeted batch part(s) move to QC_PASSED and the item status is recomputed.
func (s *QCService) Pass(actor Actor, in QCDecisionInput) (*models.OrderItem, error) {
	item, err := s.resolveItem(in.ScanRef)
	if err != nil {
		return nil, err
	}
	if item.Order != nil && item.Order.ReviewStatus != models.ReviewApproved {
		return nil, apperr.Unprocessable("Đơn chưa được duyệt để sản xuất nên chưa thể QC")
	}
	if item.MockupURL == "" {
		return nil, apperr.Unprocessable("Item chưa có link mockup của seller để đối chiếu khi QC")
	}
	targets, err := s.targetBatchItems(item, in.BatchItemID)
	if err != nil {
		return nil, err
	}
	// Already fully QC-passed → refuse a re-scan so we don't write a duplicate QC
	// record; the station surfaces this as "đã QC rồi".
	allPassed := len(targets) > 0
	for i := range targets {
		if targets[i].Status != models.StatusQCPassed {
			allPassed = false
			break
		}
	}
	if allPassed {
		return nil, apperr.Unprocessable("Item này đã QC PASS rồi — không cần quét lại")
	}

	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		for i := range targets {
			bi := &targets[i]
			if bi.Status == models.StatusPending {
				return apperr.Unprocessable("Item này còn phần sản xuất chưa hoàn thành (chưa in/cắt) — sản xuất xong mới QC được")
			}
			var bid = bi.ID
			if err := txRepo.QC.Create(&models.QCRecord{
				OrderItemID: item.ID, BatchItemID: &bid, Result: models.QCPass,
				MockupURL: item.MockupURL, Note: in.Note, CheckedByID: actor.IDPtr(),
			}); err != nil {
				return err
			}
			if bi.Status != models.StatusQCPassed {
				old := string(bi.Status)
				bi.Status = models.StatusQCPassed
				if err := txRepo.Batch.UpdateBatchItem(bi); err != nil {
					return err
				}
				_ = recordStatus(txRepo, models.EntityBatchItem, bi.ID, old, string(models.StatusQCPassed), actor, "QC pass")
			}
		}
		return nil
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, ae
		}
		return nil, apperr.Internal("Không ghi được kết quả QC PASS").Wrap(err)
	}

	updated, _ := recomputeOrderItemStatus(s.repo, item.ID, actor)
	// Roll the change up to each affected batch: a batch follows its items, so a
	// batch whose items are all QC_PASSED becomes QC_PASSED too. A combo item's
	// parts can live in different (per-material) batches, so recompute each.
	seenBatch := map[uint]bool{}
	for _, bi := range targets {
		if bi.BatchID != 0 && !seenBatch[bi.BatchID] {
			seenBatch[bi.BatchID] = true
			_ = recomputeBatchStatus(s.repo, bi.BatchID, actor)
		}
	}
	s.audit.Log(actor, "QC_PASS", "order_item", &item.ID, "QC pass for item "+item.InternalCode, nil)
	if updated != nil {
		return s.findItemByID(item.ID)
	}
	return s.findItemByID(item.ID)
}

// Fail records a QC FAIL and opens a required-attention note for rework. The
// batch part status is left unchanged (it must be reworked, not advanced).
func (s *QCService) Fail(actor Actor, in QCDecisionInput) (*models.Note, error) {
	item, err := s.resolveItem(in.ScanRef)
	if err != nil {
		return nil, err
	}
	reason := in.DefectCode
	if reason == "" {
		reason = "QC_FAILED_MINOR"
	}

	var note *models.Note
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		if err := txRepo.QC.Create(&models.QCRecord{
			OrderItemID: item.ID, BatchItemID: in.BatchItemID, Result: models.QCFail,
			MockupURL: item.MockupURL, DefectCode: reason, Note: in.Note, CheckedByID: actor.IDPtr(),
		}); err != nil {
			return err
		}
		body := in.Note
		if body == "" {
			body = "Sản phẩm không khớp mockup, cần remake/rework."
		}
		note = &models.Note{
			Title:               "QC Fail: " + item.InternalCode,
			Body:                body,
			ReasonCode:          reason,
			Severity:            models.SeverityHigh,
			Status:              models.NoteOpen,
			IsRequiredAttention: true,
			EntityType:          models.EntityOrderItem,
			EntityID:            &item.ID,
			OwnerRole:           models.RoleProduction,
			CreatedByID:         actor.IDPtr(),
		}
		return txRepo.Note.Create(note)
	})
	if err != nil {
		return nil, apperr.Internal("Không ghi được kết quả QC FAIL").Wrap(err)
	}
	s.audit.Log(actor, "QC_FAIL", "order_item", &item.ID, "QC fail for item "+item.InternalCode+" ("+reason+")", nil)
	return note, nil
}
