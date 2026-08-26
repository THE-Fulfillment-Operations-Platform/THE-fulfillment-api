package services

import (
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// UndoQCInput gỡ một lần QC pass bấm nhầm.
type UndoQCInput struct {
	ScanRef
	// Reason là ghi chú ngắn cho lịch sử QC (tuỳ chọn, tối đa 60 ký tự).
	Reason string `json:"reason"`
}

// UndoQCResult là những gì một lần hạ QC đã làm.
type UndoQCResult struct {
	ItemID         uint                  `json:"item_id"`
	ItemCode       string                `json:"item_code"`
	Parts          int                   `json:"parts"`
	InternalStatus models.InternalStatus `json:"internal_status"`
}

// UndoPass hạ kết luận "đã QC" của một sản phẩm về lại "đã cắt".
//
// Đây KHÔNG phải huỷ hàng. Nó dành cho đúng một tình huống: bấm nhầm. Hàng vẫn
// nguyên vẹn trên bàn, không tấm nào bị vứt, nên vùng ảnh hưởng nhỏ hơn hẳn huỷ
// batch — không đụng phần đã huỷ, không tăng attempt, không gỡ link sản xuất,
// không đẩy sản phẩm vào hàng chờ thiết kế, không tăng rework_count. Dùng huỷ
// batch để sửa một cú bấm nhầm sẽ ghi vào sổ một lần huỷ vật lý không hề xảy ra:
// rework_count sai, KPI làm lại sai, và tấm vật liệu "đã tiêu" thực ra vẫn còn.
//
// Ngược lại, khi hàng hỏng thật thì đây là công cụ SAI: hạ QC không ghi nhận gì
// về tấm đã hỏng và sản phẩm vẫn coi như đã sản xuất xong ở batch cũ.
//
// Chỉ OWNER/ADMIN được gọi (chặn ở tầng route): nó mở lại một cửa đã đóng.
func (s *QCService) UndoPass(actor Actor, in UndoQCInput) (*UndoQCResult, error) {
	item, err := s.resolveItem(in.ScanRef)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = "Bấm nhầm QC pass"
	}
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		return nil, apperr.BadRequest("Lý do tối đa " + itoa(maxReasonRunes) + " ký tự")
	}

	var (
		partIDs  []uint
		batchIDs []uint
	)
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		if err := txRepo.OrderItem.LockForUpdate([]uint{item.ID}); err != nil {
			return apperr.Internal("Không khoá được sản phẩm để hạ QC").Wrap(err)
		}
		// Đơn đã lên bàn đóng gói: người đóng gói đang cầm hàng, không kéo ngược
		// trạng thái ra dưới tay họ được nữa.
		if err := assertPackingNotStarted(txRepo, []uint{item.OrderID}, "hạ QC"); err != nil {
			return err
		}

		live, err := txRepo.Batch.LiveBatchItemsForOrderItem(item.ID)
		if err != nil {
			return apperr.Internal("Không đọc được phần sản xuất của item").Wrap(err)
		}
		seenBatch := map[uint]bool{}
		history := make([]models.StatusHistory, 0, len(live))
		records := make([]models.QCRecord, 0, len(live))
		for i := range live {
			p := &live[i]
			if p.Status != models.StatusQCPassed {
				continue
			}
			partIDs = append(partIDs, p.ID)
			id := p.ID
			records = append(records, models.QCRecord{
				OrderItemID: item.ID, BatchItemID: &id, Result: models.QCUndo,
				MockupURL: item.MockupURL, Note: reason, CheckedByID: actor.IDPtr(),
			})
			history = append(history, models.StatusHistory{
				EntityType: models.EntityBatchItem, EntityID: p.ID,
				FromStatus: string(models.StatusQCPassed), ToStatus: string(models.StatusCut),
				ChangedByID: actor.IDPtr(), Note: "Hạ QC (" + reason + ")",
			})
			if p.BatchID != 0 && !seenBatch[p.BatchID] {
				seenBatch[p.BatchID] = true
				batchIDs = append(batchIDs, p.BatchID)
			}
		}
		if len(partIDs) == 0 {
			return apperr.Unprocessable("Sản phẩm này chưa ở trạng thái 'đã QC' nên không có gì để hạ.")
		}
		if err := txRepo.Batch.UpdateBatchItemStatuses(partIDs, models.StatusCut); err != nil {
			return err
		}
		if err := txRepo.QC.CreateBulk(records); err != nil {
			return err
		}
		return txRepo.Status.CreateBulk(history)
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, ae
		}
		return nil, apperr.Internal("Không hạ được kết quả QC").Wrap(err)
	}

	final, _ := recomputeOrderItemStatus(s.repo, item.ID, actor)
	_ = recomputeBatchStatuses(s.repo, batchIDs, actor)

	s.audit.Log(actor, "QC_UNDO", "order_item", &item.ID,
		"Hạ QC cho sản phẩm "+item.InternalCode+" ("+reason+")",
		models.JSONMap{"parts": len(partIDs), "reason": reason})
	return &UndoQCResult{
		ItemID: item.ID, ItemCode: item.InternalCode,
		Parts: len(partIDs), InternalStatus: final,
	}, nil
}
