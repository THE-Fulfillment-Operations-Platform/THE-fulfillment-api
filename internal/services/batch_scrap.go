package services

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// ScrapBatchInput huỷ một batch đã sản xuất: cả tấm hỏng, vứt đi, làm lại.
type ScrapBatchInput struct {
	// Reason là lý do huỷ do người vận hành gõ, tối đa 60 ký tự. Bắt buộc.
	Reason string `json:"reason"`
	// Route quyết định hàng quay về đâu: PRODUCTION (mặc định — file đúng, chỉ
	// cần làm lại) hoặc DESIGN (cả tấm sai vì file sai, phải sửa file trước).
	Route string `json:"route"`
}

// ScrapBatchResult tóm tắt một lần huỷ để trạm hiển thị trong một dòng.
type ScrapBatchResult struct {
	BatchCodes    []string     `json:"batch_codes"`
	ScrappedParts int          `json:"scrapped_parts"`
	ItemIDs       []uint       `json:"item_ids"`
	UnQCedParts   int          `json:"unqced_parts"`
	Route         string       `json:"route"`
	Note          *models.Note `json:"note,omitempty"`
}

// Scrap huỷ toàn bộ phần còn sống của một batch ĐÃ SẢN XUẤT và trả các sản phẩm
// về hàng chờ làm lại — cái mà "xoá batch" cố tình không làm.
//
// Xoá batch là undo của lệnh gom batch: chỉ chạy khi chưa ai đụng vào, và nó xoá
// sạch dấu vết. Huỷ batch là chuyện ngược lại — tấm vật liệu đã được in, đã bị
// cắt hỏng, tiền đã tiêu. Bản ghi phải ở lại: từng phần được đánh dấu huỷ kèm lý
// do và người huỷ, batch được đóng (không còn gì để làm ra nữa, hàng làm lại
// nằm ở batch MỚI), và sản phẩm quay lại hàng chờ gom batch hoặc hàng chờ
// thiết kế.
//
// Huỷ được TỪNG batch con, vì mỗi con là một tấm vật lý riêng và lỗi xảy ra theo
// tấm. Bắt huỷ cả cụm thì một con đã QC ngon, đơn của nó đã bắt đầu đóng gói, sẽ
// chặn luôn việc huỷ con bị hỏng. Huỷ batch mẹ = huỷ mọi con chưa đóng; và khi
// con cuối cùng đóng thì mẹ tự đóng theo (roll-up).
func (s *BatchService) Scrap(actor Actor, batchID uint, in ScrapBatchInput) (*ScrapBatchResult, error) {
	reason, err := normalizeReason(in.Reason)
	if err != nil {
		return nil, err
	}
	route := reworkRoute(in.Route, "")

	batch, err := s.repo.Batch.FindLite(batchID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Không tìm thấy batch")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	if batch.ClosedAt != nil {
		return nil, apperr.Unprocessable("Batch " + batch.Code + " đã đóng rồi.")
	}

	targetIDs := []uint{batch.ID}

	var (
		result  = &ScrapBatchResult{Route: route}
		itemIDs []uint
		now     = time.Now()
	)
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		// Khoá các batch đích rồi mới đọc lại trạng thái: hai người cùng bấm huỷ,
		// hoặc một người huỷ trong khi người kia đang đẩy trạng thái ở bảng sản
		// xuất, phải xếp hàng chứ không được cùng thấy "chưa đóng".
		rows, err := txRepo.Batch.FindLiteManyForUpdate(targetIDs)
		if err != nil {
			return err
		}
		if len(rows) != len(targetIDs) {
			return apperr.NotFound("Không tìm thấy batch")
		}
		allPending := true
		for _, b := range rows {
			if b.ClosedAt != nil {
				return apperr.Unprocessable("Batch " + b.Code + " vừa được đóng — tải lại danh sách.")
			}
			if b.Status != models.StatusPending {
				allPending = false
			}
			result.BatchCodes = append(result.BatchCodes, b.Code)
		}

		// Batch chưa vào sản xuất thì XOÁ, không huỷ: huỷ ghi nhận vật liệu đã
		// tiêu và đẩy rework_count của sản phẩm lên, cả hai đều sai khi chưa có
		// tấm nào được in. Nhưng chỉ từ chối khi xoá thật sự làm được — một batch
		// còn PENDING mà đã có phần bị huỷ ở QC thì xoá cũng từ chối, và nếu ở
		// đây cũng từ chối nữa thì nó kẹt giữa hai cửa, không đường ra.
		started, err := txRepo.Batch.StartedPartCount(targetIDs)
		if err != nil {
			return err
		}
		if allPending && started == 0 {
			return apperr.Unprocessable("Batch " + strings.Join(result.BatchCodes, ", ") +
				" chưa vào sản xuất — dùng 'Xoá batch' để trả sản phẩm về hàng chờ gom. Huỷ chỉ dành cho hàng đã làm ra.")
		}

		// Đọc phần sống một lượt để biết đụng tới sản phẩm nào, KHOÁ các sản phẩm
		// đó, rồi đọc lại. Không khoá trước khi đọc thì một QC pass chen vào giữa
		// sẽ đóng dấu "đã QC" lên đúng tấm mình sắp vứt, và roll-up sau đó của nó
		// sẽ dựng lại cái kết luận QC mà mình vừa gỡ.
		probe, err := txRepo.Batch.LiveBatchItemsDetailed(targetIDs)
		if err != nil {
			return err
		}
		if len(probe) == 0 {
			// Không còn gì sống (mọi phần đã bị huỷ lẻ ở QC): chỉ cần đóng batch.
			if _, err := txRepo.Batch.CloseBatches(targetIDs, "Huỷ batch: "+reason, now); err != nil {
				return err
			}
			for _, b := range rows {
				_ = recordStatus(txRepo, models.EntityBatch, b.ID, string(b.Status), string(b.Status), actor,
					"Đóng batch — không còn phần nào để sản xuất")
			}
			return nil
		}
		for i := range probe {
			itemIDs = append(itemIDs, probe[i].OrderItemID)
		}
		if err := txRepo.OrderItem.LockForUpdate(itemIDs); err != nil {
			return apperr.Internal("Không khoá được sản phẩm để huỷ").Wrap(err)
		}
		parts, err := txRepo.Batch.LiveBatchItemsDetailed(targetIDs)
		if err != nil {
			return err
		}
		if len(parts) == 0 {
			return apperr.Conflict("Các phần của batch vừa được xử lý ở nơi khác — tải lại và thử lại.")
		}
		itemIDs = itemIDs[:0]
		partIDs := make([]uint, 0, len(parts))
		seenItem := map[uint]bool{}
		for i := range parts {
			p := &parts[i]
			partIDs = append(partIDs, p.ID)
			if !seenItem[p.OrderItemID] {
				seenItem[p.OrderItemID] = true
				itemIDs = append(itemIDs, p.OrderItemID)
			}
		}

		if err := assertPackingNotStarted(txRepo, orderIDsOf(parts), "huỷ batch"); err != nil {
			return err
		}

		n, err := txRepo.Batch.ScrapBatchItems(partIDs, reason, actor.IDPtr(), now)
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.Conflict("Các phần của batch vừa được huỷ ở nơi khác — tải lại và thử lại.")
		}
		result.ScrappedParts = int(n)

		history := make([]models.StatusHistory, 0, len(parts)+len(rows))
		for i := range parts {
			history = append(history, models.StatusHistory{
				EntityType: models.EntityBatchItem, EntityID: parts[i].ID,
				FromStatus: string(parts[i].Status), ToStatus: "SCRAPPED",
				ChangedByID: actor.IDPtr(), Note: "Huỷ batch (" + reason + ")",
			})
		}
		for _, b := range rows {
			history = append(history, models.StatusHistory{
				EntityType: models.EntityBatch, EntityID: b.ID,
				FromStatus: string(b.Status), ToStatus: string(b.Status),
				ChangedByID: actor.IDPtr(), Note: "Đóng batch — huỷ toàn bộ (" + reason + ")",
			})
		}
		if err := txRepo.Status.CreateBulk(history); err != nil {
			return err
		}

		// Combo nhiều NVL: mở lại cửa QC cho cả sản phẩm, không chỉ phần vừa huỷ.
		unQCedBatches, err := unQCSiblingParts(txRepo, itemIDs, partIDs, actor,
			"Huỷ batch "+strings.Join(result.BatchCodes, ", ")+" — mở lại QC cho cả sản phẩm")
		if err != nil {
			return err
		}
		result.UnQCedParts = len(unQCedBatches)

		fields := map[string]any{"rework_count": gorm.Expr("rework_count + 1")}
		if route == ReworkToDesign {
			fields["design_status"] = models.DesignMissing
		}
		if err := tx.Model(&models.OrderItem{}).Where("id IN ?", itemIDs).Updates(fields).Error; err != nil {
			return err
		}

		// Gỡ link in/cắt của tấm đã chết khỏi sản phẩm, trước khi chúng quay lại
		// hàng chờ — nếu không, batch mới sẽ được sản xuất từ đúng file đã hỏng.
		if err := unstampProductionFiles(txRepo, targetIDs, nil); err != nil {
			return err
		}

		if _, err := txRepo.Batch.CloseBatches(targetIDs, "Huỷ batch: "+reason, now); err != nil {
			return err
		}

		note, err := s.scrapNote(txRepo, actor, batch, result.BatchCodes, parts, reason, route)
		if err != nil {
			return err
		}
		result.Note = note

		// Batch anh em vừa bị hạ QC cũng phải tính lại trạng thái — trong cùng
		// transaction, để không có khoảnh khắc nào batch nói "đã QC" còn phần của
		// nó thì không.
		return recomputeBatchStatuses(txRepo, unQCedBatches, actor)
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, ae
		}
		return nil, apperr.Internal("Không huỷ được batch").Wrap(err)
	}

	result.ItemIDs = itemIDs
	// Sản phẩm mất phần sống của NVL này nên tụt về trạng thái của những phần còn
	// lại (thường là PENDING) và hiện lại ở hàng chờ gom batch; batch đã đóng
	// không còn phần sống nên roll-up bỏ qua.
	_, _ = recomputeOrderItemStatuses(s.repo, itemIDs, actor)

	s.audit.Log(actor, "BATCH_SCRAP", "batch", &batch.ID,
		"Huỷ batch "+strings.Join(result.BatchCodes, ", ")+" ("+reason+") → làm lại theo hướng "+route,
		models.JSONMap{
			"reason": reason, "route": route, "batch_codes": result.BatchCodes,
			"scrapped_parts": result.ScrappedParts, "item_ids": itemIDs,
		})
	return result, nil
}

// scrapNote mở MỘT ghi chú cho cả lần huỷ, gắn vào batch.
//
// Một tấm hỏng là một sự kiện vật lý, không phải bốn mươi sự kiện. QC fail mở
// ghi chú theo từng sản phẩm vì nó fail từng sản phẩm một; huỷ batch mà cũng làm
// vậy thì một tấm 40 sản phẩm đẻ ra 40 ghi chú "cần xử lý" giống hệt nhau và màn
// Ghi chú thôi còn nghĩa "cần xử lý". Sản phẩm phải làm lại vẫn tự hiện ở hàng
// chờ gom batch / hàng chờ thiết kế — đó mới là danh sách việc của xưởng.
func (s *BatchService) scrapNote(
	txRepo *repositories.Repositories, actor Actor, batch *models.Batch,
	codes []string, parts []models.BatchItem, reason, route string,
) (*models.Note, error) {
	var b strings.Builder
	b.WriteString("Lý do: " + reason)
	b.WriteString("\n— Batch huỷ: " + strings.Join(codes, ", "))
	b.WriteString("\n— Số phần sản xuất bị huỷ: " + itoa(len(parts)))
	if route == ReworkToDesign {
		b.WriteString("\n— Hướng xử lý: lỗi từ file design → sản phẩm đã về hàng chờ thiết kế, sửa file rồi set ready lại.")
	} else {
		b.WriteString("\n— Hướng xử lý: làm lại sản xuất → sản phẩm đã quay lại hàng chờ gom batch của NVL này.")
	}
	// Liệt kê mã tem để xưởng đối chiếu với tem đã in; cắt ở 20 để ghi chú còn
	// đọc được, phần còn lại tra ở màn batch.
	const maxListed = 20
	var codesList []string
	for i := range parts {
		if it := parts[i].OrderItem; it != nil && it.InternalCode != "" {
			codesList = append(codesList, it.InternalCode)
		}
		if len(codesList) == maxListed {
			break
		}
	}
	if len(codesList) > 0 {
		b.WriteString("\n— Sản phẩm phải làm lại: " + strings.Join(codesList, ", "))
		if len(parts) > len(codesList) {
			b.WriteString(" … (+" + itoa(len(parts)-len(codesList)) + " sản phẩm nữa)")
		}
	}

	owner := models.RoleProduction
	if route == ReworkToDesign {
		owner = models.RoleDesigner
	}
	note := &models.Note{
		Title:               "Huỷ batch: " + strings.Join(codes, ", "),
		Body:                b.String(),
		ReasonCode:          "BATCH_SCRAPPED",
		Severity:            models.SeverityHigh,
		Status:              models.NoteOpen,
		IsRequiredAttention: true,
		EntityType:          models.EntityBatch,
		EntityID:            &batch.ID,
		OwnerRole:           owner,
		CreatedByID:         actor.IDPtr(),
	}
	if err := txRepo.Note.Create(note); err != nil {
		return nil, err
	}
	return note, nil
}
