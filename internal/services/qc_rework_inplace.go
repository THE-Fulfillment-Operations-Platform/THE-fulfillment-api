package services

import (
	"sort"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// ReworkInPlaceInput là "Làm lại tại chỗ": sản phẩm QC fail đã được xưởng in/cắt
// LẺ bằng tay, không gom batch mới. Nhận mã tem hoặc id, resolve y như Scan.
type ReworkInPlaceInput struct {
	ScanRef
}

// reworkInPlaceSource là nhãn nguồn ghi vào lịch sử trạng thái / nhật ký của
// phần làm lại tại chỗ, để sau này tra ra tấm này không đi qua bảng sản xuất.
const reworkInPlaceSource = "in lẻ tại trạm QC"

// ReworkInPlace tạo phần sản xuất mới (lần kế tiếp, đã cắt) NGAY TRONG batch cũ
// cho hàng QC fail đã in lẻ xong — đường thứ hai, song song với gom batch mới.
//
// Luồng chuẩn sau QC fail: sản phẩm về hàng chờ gom batch → batch mới → Đã in →
// Đã cắt → quét lại tem cũ. Nhưng xưởng hay in lại LẺ một tấm bằng tay — không
// ai gom batch cho một món — rồi mang thẳng ra trạm QC, và trạm chặn "phần sản
// xuất đã bị huỷ, chưa QC được". Nút này ghi nhận đúng chuyện đã xảy ra: với mỗi
// NVL mà phần mới nhất đã huỷ và CHƯA có phần sống thay thế, thêm một dòng
// batch_items attempt kế tiếp, status CUT, vào CÙNG batch của phần đã huỷ (tấm
// làm lại thuộc về lệnh sản xuất cũ, không phải một tấm mới). Batch đó nếu đã
// đóng (hết phần sống) thì mở lại — nó lại có việc, và roll-up hiện có đưa nó lên
// QC_PASSED sau khi PASS như mọi batch khác; phần mới lại hỏng thì QC fail đóng
// nó lần nữa như cũ.
//
// Không tăng rework_count (QC FAIL đã đếm lần làm lại này), không mở ghi chú mới
// (ghi chú QC fail đang mở tự đóng khi PASS), không đụng luồng gom batch. Sản
// phẩm rời hàng chờ gom batch ngay: hàng chờ định nghĩa bằng "không còn phần sống
// của (sản phẩm, NVL)", mà giờ đã có.
//
// Quyền: cùng tick "Thao tác" ở trạm QC với PASS/FAIL, cộng vai trò Owner/Admin/
// Kỹ thuật viên như hạ QC — nó cũng mở lại một cửa đã đóng, cần người chịu trách
// nhiệm nói "tấm này đã in lẻ thật". Route đã chặn; kiểm lại ở đây để gọi thẳng
// service (test, công cụ nội bộ) cũng không lách được.
func (s *QCService) ReworkInPlace(actor Actor, in ReworkInPlaceInput) (*QCScanResult, error) {
	if !actor.Role.HasAdminRights() {
		return nil, apperr.Forbidden("Chỉ Owner/Admin/Kỹ thuật viên được bấm 'Làm lại tại chỗ' — hàng in lẻ phải có người chịu trách nhiệm xác nhận")
	}
	if !actor.Can(models.Manage(models.FeatQC)) {
		return nil, apperr.Forbidden("Tài khoản chưa được cấp quyền thao tác ở trạm QC")
	}
	// Sản phẩm đã huỷ → resolveItem đã từ chối ("Sản phẩm đã huỷ, không thể QC").
	item, err := s.resolveItem(in.ScanRef)
	if err != nil {
		return nil, err
	}
	if item.Order != nil {
		if item.Order.ReviewStatus == models.ReviewCancelled || itemCancelled(item.Order.CancellationStatus) {
			return nil, apperr.Conflict("Đơn " + item.Order.InternalCode + " đã huỷ — không làm lại sản phẩm này nữa")
		}
		if item.Order.ReviewStatus != models.ReviewApproved {
			return nil, apperr.Unprocessable("Đơn chưa được duyệt để sản xuất nên chưa thể làm lại")
		}
	}

	// reworkPlan là một NVL sẽ được làm lại: phần huỷ mới nhất + attempt kế tiếp.
	type reworkPlan struct {
		old  models.BatchItem
		next int
	}
	var (
		plan     []reworkPlan
		created  []models.BatchItem
		batchIDs []uint
		reopened []string
	)
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		// Thứ tự khoá giống huỷ batch: BATCH trước, SẢN PHẨM sau — hai luồng cùng
		// khoá hai bảng theo một thứ tự thì không khoá chéo nhau. Chưa khoá thì chưa
		// biết batch nào, nên dò một lượt (không khoá) để lấy tập batch của các phần
		// đã huỷ, khoá tập đó, rồi mới khoá sản phẩm và đọc lại cho chắc.
		probe, err := txRepo.Batch.BatchItemsForOrderItem(item.ID)
		if err != nil {
			return apperr.Internal("Không đọc được phần sản xuất của sản phẩm").Wrap(err)
		}
		probeBatchIDs := make([]uint, 0, len(probe))
		for i := range probe {
			if probe[i].Scrapped() {
				probeBatchIDs = append(probeBatchIDs, probe[i].BatchID)
			}
		}
		if len(probeBatchIDs) == 0 {
			return apperr.Unprocessable("Sản phẩm này không có phần sản xuất nào đã huỷ — không có gì để làm lại tại chỗ. Hàng hỏng thì ghi QC FAIL trước.")
		}
		batches, err := txRepo.Batch.FindLiteManyForUpdate(probeBatchIDs)
		if err != nil {
			return apperr.Internal("Không khoá được batch của phần đã huỷ").Wrap(err)
		}
		lockedBatch := make(map[uint]models.Batch, len(batches))
		for _, b := range batches {
			lockedBatch[b.ID] = b
		}
		if err := txRepo.OrderItem.LockForUpdate([]uint{item.ID}); err != nil {
			return apperr.Internal("Không khoá được sản phẩm để làm lại").Wrap(err)
		}
		// Đơn đã lên bàn đóng gói thì không còn gì để làm lại ở trạm QC.
		if err := assertPackingNotStarted(txRepo, []uint{item.OrderID}, "làm lại tại chỗ"); err != nil {
			return err
		}
		// Đọc lại SAU khi khoá: ảnh chụp lúc dò có thể đã cũ (một lần gom batch hay
		// QC fail khác chen vào giữa).
		parts, err := txRepo.Batch.BatchItemsForOrderItem(item.ID)
		if err != nil {
			return apperr.Internal("Không đọc được phần sản xuất của sản phẩm").Wrap(err)
		}
		latestScrapped := map[uint]*models.BatchItem{}
		live := map[uint]*models.BatchItem{}
		for i := range parts {
			p := &parts[i]
			if !p.Scrapped() {
				if cur := live[p.MaterialID]; cur == nil || p.Attempt > cur.Attempt {
					live[p.MaterialID] = p
				}
				continue
			}
			if cur := latestScrapped[p.MaterialID]; cur == nil || p.Attempt > cur.Attempt {
				latestScrapped[p.MaterialID] = p
			}
		}
		if len(latestScrapped) == 0 {
			return apperr.Unprocessable("Sản phẩm này không có phần sản xuất nào đã huỷ — không có gì để làm lại tại chỗ. Hàng hỏng thì ghi QC FAIL trước.")
		}
		// NVL đã có phần sống (đã gom batch mới / đã làm lại rồi) thì không làm nữa:
		// làm lại tại chỗ không được đẻ ra hai phần sống cho cùng một NVL.
		var alreadyReworked []string
		for mid, old := range latestScrapped {
			if l := live[mid]; l != nil {
				alreadyReworked = append(alreadyReworked, partLabel(l))
				continue
			}
			if _, ok := lockedBatch[old.BatchID]; !ok {
				return apperr.Conflict("Phần sản xuất của sản phẩm vừa thay đổi ở nơi khác — quét lại rồi thử lại.")
			}
			next, err := txRepo.Batch.NextAttempt(item.ID, mid)
			if err != nil {
				return apperr.Internal("Không tính được lần sản xuất kế tiếp").Wrap(err)
			}
			plan = append(plan, reworkPlan{old: *old, next: next})
		}
		if len(plan) == 0 {
			sort.Strings(alreadyReworked)
			return apperr.Unprocessable("Sản phẩm đã có phần sản xuất mới thay cho phần đã huỷ (" +
				strings.Join(alreadyReworked, "; ") + ") — QC theo phần đó, không cần làm lại tại chỗ.")
		}
		// Map không có thứ tự; sắp theo NVL để lịch sử và nhật ký ổn định.
		sort.Slice(plan, func(i, j int) bool { return plan[i].old.MaterialID < plan[j].old.MaterialID })

		for _, p := range plan {
			created = append(created, models.BatchItem{
				BatchID: p.old.BatchID, OrderItemID: item.ID, MaterialID: p.old.MaterialID,
				Status: models.StatusCut, Attempt: p.next,
			})
		}
		if err := txRepo.Batch.CreateItems(created); err != nil {
			return apperr.Internal("Không tạo được phần sản xuất làm lại").Wrap(err)
		}

		history := make([]models.StatusHistory, 0, len(created)+len(batches))
		seenBatch := map[uint]bool{}
		for i := range created {
			old := plan[i].old
			reason := strings.TrimSpace(old.ScrapReason)
			if reason == "" {
				reason = "không ghi lý do"
			}
			history = append(history, models.StatusHistory{
				EntityType: models.EntityBatchItem, EntityID: created[i].ID,
				ToStatus: string(models.StatusCut), ChangedByID: actor.IDPtr(),
				Note: "Làm lại tại chỗ — " + reworkInPlaceSource + " (lần " + itoa(created[i].Attempt) +
					", thay phần đã huỷ: " + reason + ")",
			})
			if !seenBatch[old.BatchID] {
				seenBatch[old.BatchID] = true
				batchIDs = append(batchIDs, old.BatchID)
			}
		}
		// Batch đã đóng vì hết phần sống: mở lại, vì nó vừa có lại một phần sống.
		var closedIDs []uint
		for _, id := range batchIDs {
			b := lockedBatch[id]
			if b.ClosedAt == nil {
				continue
			}
			closedIDs = append(closedIDs, b.ID)
			reopened = append(reopened, b.Code)
			history = append(history, models.StatusHistory{
				EntityType: models.EntityBatch, EntityID: b.ID,
				FromStatus: string(b.Status), ToStatus: string(b.Status), ChangedByID: actor.IDPtr(),
				Note: "Mở lại batch — làm lại tại chỗ (" + reworkInPlaceSource + ") sản phẩm " + item.InternalCode,
			})
		}
		if _, err := txRepo.Batch.ReopenBatches(closedIDs); err != nil {
			return apperr.Internal("Không mở lại được batch").Wrap(err)
		}
		return txRepo.Status.CreateBulk(history)
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, ae
		}
		return nil, apperr.Internal("Không ghi nhận được phần làm lại tại chỗ").Wrap(err)
	}

	// Như Pass/Fail: roll-up ngoài transaction. Sản phẩm lại có phần sống đã cắt
	// → về CUT; batch cũ tính lại theo phần sống (có thêm phần CUT).
	_, _ = recomputeOrderItemStatus(s.repo, item.ID, actor)
	_ = recomputeBatchStatuses(s.repo, batchIDs, actor)

	lines := make([]string, 0, len(plan))
	reasons := make([]string, 0, len(plan))
	materials := make([]uint, 0, len(plan))
	attempts := make([]int, 0, len(plan))
	batchCodes := make([]string, 0, len(plan))
	for _, p := range plan {
		lines = append(lines, partLabel(&p.old)+" → lần "+itoa(p.next)+" (thay phần đã huỷ: "+p.old.ScrapReason+")")
		reasons = append(reasons, p.old.ScrapReason)
		materials = append(materials, p.old.MaterialID)
		attempts = append(attempts, p.next)
		if p.old.Batch != nil {
			batchCodes = append(batchCodes, p.old.Batch.Code)
		}
	}
	s.audit.Log(actor, "QC_REWORK_INPLACE", "order_item", &item.ID,
		"Làm lại tại chỗ ("+reworkInPlaceSource+") sản phẩm "+item.InternalCode+": "+strings.Join(lines, "; "),
		models.JSONMap{
			"source": reworkInPlaceSource, "batch_codes": batchCodes, "material_ids": materials,
			"attempts": attempts, "scrap_reasons": reasons, "reopened_batches": reopened,
		})

	// Trả về đúng màn quét mới để web vẽ lại: phần làm lại đã ở "Đã cắt", PASS mở.
	fresh, err := s.findItemByID(item.ID)
	if err != nil {
		return nil, err
	}
	return s.scanResult(fresh), nil
}

// partLabel gọi tên một phần sản xuất cho thông báo: "NVL MICA · batch #101001 ·
// lần 2". Material/Batch có thể chưa preload — khi đó lùi về id.
func partLabel(p *models.BatchItem) string {
	mat := "NVL #" + itoa(int(p.MaterialID))
	if p.Material != nil && strings.TrimSpace(p.Material.Code) != "" {
		mat = "NVL " + p.Material.Code
	}
	batch := "batch #" + itoa(int(p.BatchID))
	if p.Batch != nil && p.Batch.Code != "" {
		batch = "batch " + p.Batch.Code
	}
	return mat + " · " + batch + " · lần " + itoa(maxInt(p.Attempt, 1))
}
