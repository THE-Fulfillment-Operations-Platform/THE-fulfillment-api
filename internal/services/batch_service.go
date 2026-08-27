package services

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/apptypes"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// BatchService creates production batches (grouped by material) and drives the
// internal production status machine.
type BatchService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// CreateBatchInput creates one batch for a single material from selected items.
// A combo item that needs several materials is handled by creating one batch per
// material (call this endpoint once per material).
type CreateBatchInput struct {
	MaterialID   uint           `json:"material_id" binding:"required"`
	OrderItemIDs []uint         `json:"order_item_ids" binding:"required,min=1"`
	Priority     string         `json:"priority"`
	DueDate      *apptypes.Date `json:"due_date"`
	Note         string         `json:"note"`
}

// Create builds a batch and its batch items. Items whose SKU does not include the
// material, or that are already scheduled for that material, are skipped and
// reported back so the caller knows exactly what was batched.
func (s *BatchService) Create(actor Actor, in CreateBatchInput) (*models.Batch, []uint, error) {
	material, err := s.repo.Material.FindByID(in.MaterialID)
	if err != nil {
		return nil, nil, apperr.BadRequest("material_id does not reference an existing material")
	}

	existing, err := s.repo.Batch.ExistingActiveItemMaterial(in.OrderItemIDs)
	if err != nil {
		return nil, nil, apperr.Internal("could not check existing batch items").Wrap(err)
	}

	priority := models.Priority(in.Priority)
	switch priority {
	case models.PriorityNormal, models.PriorityHigh, models.PriorityUrgent:
	default:
		priority = models.PriorityNormal
	}

	// Production quota of this material (0 = unlimited → never split).
	quota := 0
	if material.ProductsPerUnit != nil {
		quota = *material.ProductsPerUnit
	}

	var rootBatch *models.Batch // the batch returned to the caller (flat batch or parent)
	var skipped []uint

	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		// Bulk-load every selected item (order + SKU materials) in one query set —
		// the per-item checks below then run entirely in memory instead of issuing
		// a fully-preloaded FindByID per item.
		itemsByID, err := txRepo.OrderItem.FindForBatching(in.OrderItemIDs)
		if err != nil {
			return err
		}

		// Resolve eligible items in the caller's order (so the quota split groups them
		// in that same order); everything ineligible is reported back as skipped —
		// WITH the reason, because "không có sản phẩm hợp lệ" on its own leaves the
		// user staring at a list the screen just offered them.
		var eligible []*models.OrderItem
		reasons := map[uint]string{} // itemID → vì sao bị loại
		label := func(itemID uint) string {
			if it := itemsByID[itemID]; it != nil && it.InternalCode != "" {
				return it.InternalCode
			}
			return fmt.Sprintf("#%d", itemID)
		}
		skip := func(itemID uint, reason string) {
			skipped = append(skipped, itemID)
			reasons[itemID] = reason
		}
		for _, itemID := range in.OrderItemIDs {
			item, ok := itemsByID[itemID]
			if !ok {
				skip(itemID, "không tìm thấy sản phẩm")
				continue
			}
			// Only approved orders may enter production.
			if item.Order == nil || item.Order.ReviewStatus != models.ReviewApproved {
				skip(itemID, "đơn chưa được duyệt")
				continue
			}
			if itemCancelled(item.CancellationStatus) {
				skip(itemID, "sản phẩm đã huỷ")
				continue
			}
			// The item's SKU must include this material.
			if !skuHasMaterial(item.SKU, material.ID) {
				skip(itemID, "SKU không dùng NVL "+material.Code)
				continue
			}
			// Skip if already scheduled for this material. Parts scrapped after a QC
			// fail don't count — that is what lets a re-made product be batched again.
			if existing[itemID] != nil && existing[itemID][material.ID] {
				skip(itemID, "đã nằm trong một batch khác của NVL này")
				continue
			}
			eligible = append(eligible, item)
		}
		if len(eligible) == 0 {
			return apperr.Unprocessable("Không gom được sản phẩm nào vào batch: " + describeSkips(in.OrderItemIDs, reasons, label))
		}

		groups := planBatchSplit(eligible, quota)

		// newBatch builds a batch carrying the shared attributes; createWithItems
		// persists a batch, stamps its code, attaches items and records history.
		newBatch := func() *models.Batch {
			return &models.Batch{
				MaterialID: material.ID, Status: models.StatusPending, Priority: priority,
				DueDate: in.DueDate.TimePtr(), Note: in.Note, CreatedByID: actor.IDPtr(),
			}
		}
		attachItems := func(batch *models.Batch, items []*models.OrderItem) error {
			// A product can be produced more than once (QC fail → re-make), so each
			// new part carries the next attempt number for its (item, material). One
			// query for the whole batch; first-time items simply get attempt 1.
			itemIDs := make([]uint, 0, len(items))
			for _, item := range items {
				itemIDs = append(itemIDs, item.ID)
			}
			nextAttempt, err := txRepo.Batch.NextAttempts(material.ID, itemIDs)
			if err != nil {
				return err
			}
			batchItems := make([]models.BatchItem, 0, len(items))
			for _, item := range items {
				attempt := nextAttempt[item.ID]
				if attempt < 1 {
					attempt = 1
				}
				batchItems = append(batchItems, models.BatchItem{
					BatchID: batch.ID, OrderItemID: item.ID, MaterialID: material.ID,
					Status: models.StatusPending, Attempt: attempt,
				})
			}
			if err := txRepo.Batch.CreateItems(batchItems); err != nil {
				return err
			}
			history := make([]models.StatusHistory, 0, len(batchItems))
			for _, bi := range batchItems {
				history = append(history, models.StatusHistory{
					EntityType: models.EntityBatchItem, EntityID: bi.ID,
					ToStatus: string(models.StatusPending), ChangedByID: actor.IDPtr(),
					Note: "added to batch " + batch.Code,
				})
			}
			return txRepo.Status.CreateBulk(history)
		}

		// Within quota (or unlimited): one flat batch — identical to legacy behaviour.
		if len(groups) <= 1 {
			batch := newBatch()
			if err := txRepo.Batch.Create(batch); err != nil {
				return err
			}
			batch.Code = fmt.Sprintf("#%d", 101000+batch.ID)
			if err := txRepo.Batch.Update(batch); err != nil {
				return err
			}
			if err := attachItems(batch, eligible); err != nil {
				return err
			}
			_ = recordStatus(txRepo, models.EntityBatch, batch.ID, "", string(models.StatusPending), actor, "batch created")
			rootBatch = batch
			return nil
		}

		// Over quota: a parent batch (holds no items) + one child batch per group,
		// each capped at the material's quota. Codes: parent "#<n>", child "#<n>-<seq>".
		parent := newBatch()
		parent.IsParent = true
		parent.ChildCount = len(groups)
		if err := txRepo.Batch.Create(parent); err != nil {
			return err
		}
		parent.Code = fmt.Sprintf("#%d", 101000+parent.ID)
		if err := txRepo.Batch.Update(parent); err != nil {
			return err
		}
		parentID := parent.ID
		for i, group := range groups {
			child := newBatch()
			child.ParentBatchID = &parentID
			child.Sequence = i + 1
			if err := txRepo.Batch.Create(child); err != nil {
				return err
			}
			child.Code = fmt.Sprintf("%s-%d", parent.Code, i+1)
			if err := txRepo.Batch.Update(child); err != nil {
				return err
			}
			if err := attachItems(child, group); err != nil {
				return err
			}
			_ = recordStatus(txRepo, models.EntityBatch, child.ID, "", string(models.StatusPending), actor, "child batch created under "+parent.Code)
		}
		_ = recordStatus(txRepo, models.EntityBatch, parent.ID, "", string(models.StatusPending), actor, "parent batch created")
		rootBatch = parent
		return nil
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, nil, ae
		}
		return nil, nil, apperr.Internal("could not create batch").Wrap(err)
	}

	// Recompute affected items' internal status outside the create transaction.
	_, _ = recomputeOrderItemStatuses(s.repo, in.OrderItemIDs, actor)

	full, _ := s.repo.Batch.FindByID(rootBatch.ID)
	s.audit.Log(actor, "BATCH_CREATE", "batch", &rootBatch.ID,
		fmt.Sprintf("Created batch %s (material=%s)", rootBatch.Code, material.Code),
		models.JSONMap{"skipped": skipped, "children": rootBatch.ChildCount})
	return full, skipped, nil
}

// planBatchSplit partitions items into groups, each holding at most `quota`
// products (sum of Quantity), never splitting a single item across groups (an
// item whose own quantity exceeds the quota gets its own over-quota group).
// quota ≤ 0 means unlimited → a single group. Mirrors the web app's
// utils/batch.ts planBatchSplit so the split preview and the created batches
// always agree.
func planBatchSplit(items []*models.OrderItem, quota int) [][]*models.OrderItem {
	if len(items) == 0 {
		return nil
	}
	if quota <= 0 {
		return [][]*models.OrderItem{items}
	}
	var groups [][]*models.OrderItem
	var current []*models.OrderItem
	count := 0
	for _, it := range items {
		q := it.Quantity
		if q < 1 {
			q = 1
		}
		// Start a new group when the current one is non-empty and adding this item
		// would exceed the quota.
		if len(current) > 0 && count+q > quota {
			groups = append(groups, current)
			current = nil
			count = 0
		}
		current = append(current, it)
		count += q
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

func (s *BatchService) Get(id uint) (*models.Batch, error) {
	b, err := s.repo.Batch.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Batch not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return b, nil
}

func (s *BatchService) List(f repositories.BatchFilter) ([]models.Batch, int64, error) {
	f.Page = f.Page.Normalize()
	return s.repo.Batch.List(f)
}

// UpdateStatusInput sets a new production status on a batch.
type UpdateStatusInput struct {
	Status string `json:"status" binding:"required"`
	Note   string `json:"note"`
}

// UpdateStatus moves a batch (and all its batch items) to a new internal status
// and recomputes the affected items. Used by the production board / batch detail
// for Pending → Đã in → Đã cắt → Đã QC.
func (s *BatchService) UpdateStatus(actor Actor, batchID uint, in UpdateStatusInput) (*models.Batch, error) {
	newStatus := models.InternalStatus(in.Status)
	if !newStatus.Valid() {
		return nil, apperr.BadRequest("Invalid status (PENDING|PRINTED|CUT|QC_PASSED)")
	}
	// The production board only advances fabrication stages (PENDING/PRINTED/CUT).
	// QC_PASSED is a product-level gate set exactly once at the QC station when the
	// whole finished product (every material part) is done — not per-material batch.
	// A batch still reaches QC_PASSED, but only by rolling up from its QC-passed items.
	if newStatus == models.StatusQCPassed {
		return nil, apperr.Unprocessable("Không đặt 'Đã QC' ở bảng sản xuất. QC được thực hiện 1 lần ở trạm QC khi cả sản phẩm (mọi NVL) đã sản xuất xong.")
	}
	// The guards below need the batch row and (for fabrication stages) which links
	// exist — not the full detail payload with every association preloaded, which
	// cost a handful of round trips to the remote DB before being thrown away.
	batch, err := s.repo.Batch.FindLite(batchID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Batch not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	// A QC-passed batch is finished; the board never edits it (undoing QC is a
	// deliberate QC-station action, not a status click). Mirrors the FE lock.
	if batch.Status == models.StatusQCPassed {
		return nil, apperr.Unprocessable("Batch đã QC — không cập nhật trạng thái ở bảng sản xuất nữa.")
	}
	// Batch đã đóng (huỷ cả tấm, hoặc mọi phần đã bị huỷ ở QC) không còn gì để
	// sản xuất — hàng làm lại nằm ở batch mới.
	if batch.ClosedAt != nil {
		return nil, apperr.Unprocessable("Batch " + batch.Code + " đã đóng — không cập nhật trạng thái nữa. Hàng làm lại nằm ở batch mới.")
	}
	// Production moves forward: a batch never regresses to an earlier stage, which
	// protects the QC gate (regressing a CUT batch would un-finish its items). The
	// one exception is OWNER, who may step a batch back to fix a mistaken advance.
	// Rework is remake + a note, not a status rollback.
	if newStatus.Rank() < batch.Status.Rank() && actor.Role != models.RoleOwner {
		return nil, apperr.Unprocessable("Sản xuất chỉ tiến, không lùi: batch đang ở '" + string(batch.Status) + "', không thể hạ về '" + string(newStatus) + "'. (Chỉ OWNER được sửa khi bấm nhầm.)")
	}
	// Entering fabrication requires the batch's shared production files. The print
	// and cut files are ganged once per batch, so nobody can have printed or cut
	// without them on record — and marking a stage done without them would strand
	// the batch in the design queue, which only clears once both links exist.
	// Parent batches are skipped: they hold no items and carry no links (their
	// status is rolled up from their children, each of which is guarded here).
	if !batch.IsParent && (newStatus == models.StatusPrinted || newStatus == models.StatusCut) {
		kinds, err := s.repo.Batch.LinkKindsForBatch(batch.ID)
		if err != nil {
			return nil, apperr.Internal("could not read batch links").Wrap(err)
		}
		var hasPrint, hasCut bool
		for _, k := range kinds {
			switch k {
			case models.BatchLinkPrint:
				hasPrint = true
			case models.BatchLinkCut:
				hasCut = true
			}
		}
		if !hasPrint || !hasCut {
			var missing []string
			if !hasPrint {
				missing = append(missing, "link in")
			}
			if !hasCut {
				missing = append(missing, "link cắt")
			}
			return nil, apperr.Unprocessable("Batch chưa có " + strings.Join(missing, " và ") +
				". Thêm link sản xuất dùng chung trước khi chuyển sang '" + string(newStatus) + "'.")
		}
	}

	affectedItems := map[uint]bool{}
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		// Đọc lại batch có khoá dòng, ngay trong transaction. Mọi guard ở trên chạy
		// trên bản đọc trước transaction, nên giữa lúc render màn hình và lúc ghi,
		// batch có thể đã bị huỷ (đóng) hoặc đã được đẩy trạng thái ở nơi khác —
		// và không có vòng này thì lệnh vẫn ghi đè lên trạng thái mới đó.
		locked, err := txRepo.Batch.FindLiteManyForUpdate([]uint{batch.ID})
		if err != nil {
			return err
		}
		if len(locked) == 0 {
			return apperr.NotFound("Batch not found")
		}
		fresh := locked[0]
		if fresh.ClosedAt != nil {
			return apperr.Unprocessable("Batch " + fresh.Code + " vừa bị huỷ/đóng — tải lại bảng sản xuất.")
		}
		if fresh.Status == models.StatusQCPassed {
			return apperr.Unprocessable("Batch đã QC — không cập nhật trạng thái ở bảng sản xuất nữa.")
		}
		if newStatus.Rank() < fresh.Status.Rank() && actor.Role != models.RoleOwner {
			return apperr.Unprocessable("Sản xuất chỉ tiến, không lùi: batch đang ở '" + string(fresh.Status) + "', không thể hạ về '" + string(newStatus) + "'. (Chỉ OWNER được sửa khi bấm nhầm.)")
		}
		batch.Status = fresh.Status
		// Cancelled parts are filtered out by the query itself — the cascade never
		// touches them, so there is no reason to fetch them and their cancellation
		// status in a second statement.
		items, err := txRepo.Batch.ActiveBatchItemsForBatch(batch.ID)
		if err != nil {
			return err
		}
		var history []models.StatusHistory
		moved := make([]uint, 0, len(items))
		for i := range items {
			bi := &items[i]
			// Never let a board change touch an already QC-passed part. In a mixed
			// batch (one order QC-passed while another still in production) this stops
			// a batch move — especially an OWNER regression — from silently un-QC'ing it.
			if bi.Status == models.StatusQCPassed {
				continue
			}
			if bi.Status == newStatus {
				continue
			}
			old := string(bi.Status)
			bi.Status = newStatus
			moved = append(moved, bi.ID)
			history = append(history, models.StatusHistory{
				EntityType: models.EntityBatchItem, EntityID: bi.ID,
				FromStatus: old, ToStatus: string(newStatus),
				ChangedByID: actor.IDPtr(), Note: in.Note,
			})
			affectedItems[bi.OrderItemID] = true
		}
		// Every moved part lands on the same status, so it is one UPDATE regardless
		// of batch size (this used to be one statement per item).
		if err := txRepo.Batch.UpdateBatchItemStatuses(moved, newStatus); err != nil {
			return err
		}
		oldBatch := string(batch.Status)
		batch.Status = newStatus
		if err := txRepo.Batch.UpdateStatusColumn(batch.ID, newStatus); err != nil {
			return err
		}
		history = append(history, models.StatusHistory{
			EntityType: models.EntityBatch, EntityID: batch.ID,
			FromStatus: oldBatch, ToStatus: string(newStatus),
			ChangedByID: actor.IDPtr(), Note: in.Note,
		})
		return txRepo.Status.CreateBulk(history)
	})
	if err != nil {
		return nil, apperr.Internal("could not update batch status").Wrap(err)
	}

	itemIDs := make([]uint, 0, len(affectedItems))
	for itemID := range affectedItems {
		itemIDs = append(itemIDs, itemID)
	}
	_, _ = recomputeOrderItemStatuses(s.repo, itemIDs, actor)

	// If this is a child batch, roll the change up into its parent's status.
	if batch.ParentBatchID != nil {
		_ = recomputeParentBatchStatus(s.repo, *batch.ParentBatchID, actor)
	}

	s.audit.Log(actor, "BATCH_STATUS_UPDATE", "batch", &batch.ID,
		fmt.Sprintf("Batch %s -> %s", batch.Code, newStatus), nil)
	return s.Get(batch.ID)
}

// Delete removes a batch that production has not touched yet, releasing its
// items back to the batching pool (they reappear on the create-batch screen and
// can be re-grouped). Deleting a split parent removes the whole child tree in
// one go; a child is never deleted on its own — the split is quota-derived, so
// removing one child would leave the parent's ChildCount and sequence lying.
//
// The guard is deliberately narrow: every batch in the tree must still be
// PENDING (and not closed), and every part untouched (PENDING, not scrapped).
// A batch that was printed or cut represents physical goods and spent material —
// that is the scrap/close flow's job, not delete's. Any tem QR printed for the
// deleted batch becomes waste paper; the replacement batch prints its own.
func (s *BatchService) Delete(actor Actor, batchID uint) error {
	batch, err := s.repo.Batch.FindLite(batchID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperr.NotFound("Batch not found")
		}
		return apperr.Internal("lookup failed").Wrap(err)
	}
	if batch.ParentBatchID != nil {
		return apperr.Unprocessable("Đây là batch con trong cụm chia theo định mức — không xoá riêng lẻ. Hãy xoá batch cha để xoá cả cụm.")
	}

	var itemIDs []uint
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		ids := []uint{batch.ID}
		if batch.IsParent {
			children, err := txRepo.Batch.ChildBatchesFor(batch.ID)
			if err != nil {
				return err
			}
			for _, c := range children {
				ids = append(ids, c.ID)
			}
		}

		// Re-read inside the transaction: the board may have advanced the batch
		// between the screen render and the click, and the header alone can lag
		// (a PENDING batch whose part a QC action already moved), so the guard
		// checks both the headers and every part.
		rows, err := txRepo.Batch.FindLiteMany(ids)
		if err != nil {
			return err
		}
		for _, b := range rows {
			if b.Status != models.StatusPending {
				return apperr.Unprocessable("Batch " + b.Code + " đã vào sản xuất (đang ở '" + string(b.Status) + "') — chỉ xoá được batch chưa sản xuất. Hàng in lỗi/bỏ đi xử lý qua QC (scrap), không qua xoá batch.")
			}
			if b.ClosedAt != nil {
				return apperr.Unprocessable("Batch " + b.Code + " đã đóng — được giữ làm lịch sử sản xuất, không xoá.")
			}
		}
		started, err := txRepo.Batch.StartedPartCount(ids)
		if err != nil {
			return err
		}
		if started > 0 {
			return apperr.Unprocessable("Batch có phần đã được sản xuất hoặc đã huỷ tại QC — chỉ xoá được batch chưa sản xuất.")
		}

		itemIDs, err = txRepo.Batch.OrderItemIDsForBatches(ids)
		if err != nil {
			return err
		}

		// Un-stamp the batch's shared print/cut file from its items BEFORE the
		// parts go away (the item set is derived from batch_items). Otherwise the
		// stale URL survives on the item and the production export's item-level
		// fallback would offer the deleted batch's file to the next batch.
		for _, id := range ids {
			links, err := txRepo.Batch.LinksForBatch(id)
			if err != nil {
				return err
			}
			for _, link := range links {
				column := "print_file_url"
				if link.Kind == models.BatchLinkCut {
					column = "cut_file_url"
				}
				if _, err := txRepo.OrderItem.ClearProductionFileForBatch(id, column, link.URL); err != nil {
					return err
				}
			}
		}

		// Parts are hard-deleted (idx_item_material_attempt has no deleted_at
		// predicate — see HardDeleteBatchItems); headers and links soft-delete so
		// the batch stays traceable. Status history is append-only and stays.
		if err := txRepo.Batch.HardDeleteBatchItems(ids); err != nil {
			return err
		}
		if err := txRepo.Batch.SoftDeleteLinks(ids); err != nil {
			return err
		}
		return txRepo.Batch.SoftDeleteBatches(ids)
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return ae
		}
		return apperr.Internal("could not delete batch").Wrap(err)
	}

	// The released items derive their status from whatever parts remain in other
	// batches — none left means back to PENDING and the batching pool.
	_, _ = recomputeOrderItemStatuses(s.repo, itemIDs, actor)

	s.audit.Log(actor, "BATCH_DELETE", "batch", &batch.ID,
		fmt.Sprintf("Deleted batch %s", batch.Code),
		models.JSONMap{"released_item_ids": itemIDs, "children": batch.ChildCount})
	return nil
}

// ---------- Batch links (print / cut) ----------

// SetBatchLinkInput sets (or replaces) a batch's print or cut link.
type SetBatchLinkInput struct {
	Kind string `json:"kind" binding:"required"` // PRINT | CUT
	URL  string `json:"url" binding:"required"`
}

// SetBatchLink attaches a print or cut link to a whole batch. The link is entered
// once per batch (shared by every design in it) rather than repeated per item.
// Re-adding the same kind updates the existing row (never creating a duplicate,
// enforced by the partial unique index), recording who changed it and when. The
// URL must be a valid http(s) link; an empty string is rejected.
//
// The link is also stamped onto every item in the batch (print_file_url for PRINT,
// cut_file_url for CUT). A designer gangs many personalised designs onto a single
// sheet, so the resulting print/cut file belongs to the batch, not to any one item
// — this is what saves entering it once per order.
func (s *BatchService) SetBatchLink(actor Actor, batchID uint, in SetBatchLinkInput) (*models.BatchLink, error) {
	kind := models.BatchLinkKind(strings.ToUpper(strings.TrimSpace(in.Kind)))
	if !kind.Valid() {
		return nil, apperr.BadRequest("kind phải là PRINT hoặc CUT")
	}
	rawURL := strings.TrimSpace(in.URL)
	if rawURL == "" {
		return nil, apperr.BadRequest("URL không được để trống")
	}
	if !isValidHTTPURL(rawURL) {
		return nil, apperr.BadRequest("URL không hợp lệ (phải là http hoặc https)")
	}
	batch, err := s.Get(batchID)
	if err != nil {
		return nil, err
	}
	if batch.IsParent {
		return nil, apperr.Unprocessable("Batch mẹ không chứa item sản xuất. Hãy cập nhật link trên từng batch con.")
	}
	if batch.ClosedAt != nil {
		return nil, apperr.Unprocessable("Batch " + batch.Code + " đã đóng — không sửa link sản xuất nữa.")
	}

	now := time.Now()
	// The per-item column this kind fans out to. Fixed literals, never user input.
	column := "print_file_url"
	if kind == models.BatchLinkCut {
		column = "cut_file_url"
	}

	var link *models.BatchLink
	action := "BATCH_LINK_ADD"
	// Saving the link and stamping it onto the items is one unit of work: a batch
	// whose link says one thing while its items say another is worse than neither.
	if err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		// Guard "batch đã đóng" phải nằm trong transaction: một batch bị huỷ ngay
		// sau khi màn hình được render mà vẫn nhận link mới thì lệnh fan-out bên
		// dưới sẽ đóng dấu lại URL lên đúng những sản phẩm vừa được gỡ ra.
		locked, err := txRepo.Batch.FindLiteManyForUpdate([]uint{batchID})
		if err != nil {
			return apperr.Internal("could not lock batch").Wrap(err)
		}
		if len(locked) == 0 {
			return apperr.NotFound("Batch not found")
		}
		if locked[0].ClosedAt != nil {
			return apperr.Unprocessable("Batch " + locked[0].Code + " vừa bị huỷ/đóng — không sửa link sản xuất nữa.")
		}

		existing, err := txRepo.Batch.FindLink(batchID, kind)
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.Internal("could not look up batch link").Wrap(err)
			}
			link = &models.BatchLink{BatchID: batchID, Kind: kind, URL: rawURL, UpdatedByID: actor.IDPtr(), LinkUpdatedAt: now}
			if err := txRepo.Batch.CreateLink(link); err != nil {
				return apperr.Internal("could not add batch link").Wrap(err)
			}
		} else {
			action = "BATCH_LINK_UPDATE"
			existing.URL = rawURL
			existing.UpdatedByID = actor.IDPtr()
			existing.LinkUpdatedAt = now
			if err := txRepo.Batch.UpdateLink(existing); err != nil {
				return apperr.Internal("could not update batch link").Wrap(err)
			}
			link = existing
		}

		// Fan the shared file out onto every item in the batch: the production
		// export, QR labels and QC all read the per-item column, so this is what
		// makes "same batch → same print/cut file" true for them. Replacing a link
		// re-stamps every item, so the batch never ends up half-updated.
		if _, err := txRepo.OrderItem.SetProductionFileForBatch(batchID, column, rawURL); err != nil {
			return apperr.Internal("could not apply batch link to items").Wrap(err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	s.audit.Log(actor, action, "batch", &batchID,
		fmt.Sprintf("Set %s link on batch %d", kind, batchID),
		models.JSONMap{"kind": string(kind), "url": rawURL})

	links, _ := s.repo.Batch.LinksForBatch(batchID)
	for i := range links {
		if links[i].ID == link.ID {
			return &links[i], nil
		}
	}
	return link, nil
}

func skuHasMaterial(sku *models.SKU, materialID uint) bool {
	if sku == nil {
		return false
	}
	for _, m := range sku.Materials {
		if m.MaterialID == materialID {
			return true
		}
	}
	return false
}

// ---------- Legacy production-template export ----------

// productionTemplateHeaders is the exact, ordered legacy production-template
// header row. The workshop's existing spreadsheet relies on this precise column
// order and spelling — note "Mã nội bộ" intentionally appears twice (positions 1
// and 13) for legacy compatibility. Do not reorder or rename.
var productionTemplateHeaders = []string{
	"Mã nội bộ",
	"SỐ Batch",
	"Ngày",
	"Order ID",
	"SKU",
	"Loại VL",
	"Mô tả Sp để QC (Hiện lên phần QC)",
	"Mã ảnh (copy bên TĐN Ctr + Ship + V...)",
	"Số thứ tự",
	"Số lượng",
	"Link ảnh",
	"Mock up",
	"Mã nội bộ",
	"Tên khách",
	"Tên File",
	"Link in",
	"Link cắt",
}

// seqStr renders a production sequence, leaving an unassigned (0) sequence blank
// so the legacy sheet does not show a spurious ordering position.
func seqStr(n int) string {
	if n == 0 {
		return ""
	}
	return itoa(n)
}

// ProductionTemplateGrid builds the legacy production-template grid — the header
// row followed by one row per batch item — for a fully-loaded batch (with
// Items.OrderItem.Order and Items.Material preloaded). It is a pure function so
// the column order and per-field mapping can be unit-tested without a database.
func ProductionTemplateGrid(batch *models.Batch) [][]string {
	date := batch.CreatedAt.Format("2006-01-02")
	grid := make([][]string, 0, len(batch.Items)+1)
	grid = append(grid, productionTemplateHeaders)
	batchPrintURL, batchCutURL := batchProductionLinks(batch)

	for _, bi := range batch.Items {
		it := bi.OrderItem
		if it == nil || itemCancelled(it.CancellationStatus) {
			continue
		}
		// Loại VL comes from the batch's material (batches are material-scoped).
		materialName := batch.Material.Name
		if bi.Material != nil {
			if bi.Material.Name != "" {
				materialName = bi.Material.Name
			} else {
				materialName = bi.Material.Code
			}
		}
		var orderID, customer string
		if it.Order != nil {
			orderID = it.Order.StoreOrderID
			customer = it.Order.ShippingName
		}
		printURL, cutURL := batchPrintURL, batchCutURL
		if printURL == "" {
			printURL = it.PrintFileURL
		}
		if cutURL == "" {
			cutURL = it.CutFileURL
		}
		grid = append(grid, []string{
			it.InternalCode,               // Mã nội bộ
			batch.Code,                    // SỐ Batch
			date,                          // Ngày
			orderID,                       // Order ID
			it.SKUCode,                    // SKU
			materialName,                  // Loại VL
			it.QCDescription,              // Mô tả Sp để QC
			it.ImageCode,                  // Mã ảnh
			seqStr(it.ProductionSequence), // Số thứ tự (blank when unassigned)
			itoa(it.Quantity),             // Số lượng
			it.DesignURL,                  // Link ảnh
			it.MockupURL,                  // Mock up
			it.InternalCode,               // Mã nội bộ (legacy 2nd copy)
			customer,                      // Tên khách
			it.ProductionFileName,         // Tên File
			printURL,                      // Link in (batch link overrides legacy item link)
			cutURL,                        // Link cắt (batch link overrides legacy item link)
		})
	}
	return grid
}

// batchProductionLinks returns the canonical shared links for a batch. Legacy
// item-level links remain as a fallback for batches created before BatchLink.
func batchProductionLinks(batch *models.Batch) (printURL, cutURL string) {
	for _, link := range batch.Links {
		switch link.Kind {
		case models.BatchLinkPrint:
			printURL = link.URL
		case models.BatchLinkCut:
			cutURL = link.URL
		}
	}
	return strings.TrimSpace(printURL), strings.TrimSpace(cutURL)
}

// productionColumnWidths sets sensible Excel column widths (in characters) for the
// 17 production-template columns, keeping URL columns wide and codes/counts compact
// so the sheet is readable without manual resizing.
var productionColumnWidths = []float64{
	16, // A  Mã nội bộ
	10, // B  SỐ Batch
	12, // C  Ngày
	22, // D  Order ID
	16, // E  SKU
	18, // F  Loại VL
	32, // G  Mô tả QC
	22, // H  Mã ảnh
	9,  // I  Số thứ tự
	9,  // J  Số lượng
	42, // K  Link ảnh
	42, // L  Mock up
	16, // M  Mã nội bộ
	20, // N  Tên khách
	22, // O  Tên File
	42, // P  Link in
	42, // Q  Link cắt
}

// GetWithScrapHistory loads a batch for viewing/exporting và, với batch ĐÃ ĐÓNG,
// đọc cả những phần đã huỷ.
//
// Batch detail cố tình chỉ preload phần còn sống — trạng thái phải tính trên cái
// còn làm được. Nhưng batch đã đóng thì theo định nghĩa không còn phần nào sống,
// nên export của nó ra file rỗng: tải bảng sản xuất của một tấm vừa huỷ được một
// file chỉ có dòng tiêu đề, đúng lúc người ta cần nó nhất để đối chiếu xem tấm đó
// đã có những gì. Ở đây phần đã huỷ chính là nội dung.
func (s *BatchService) GetWithScrapHistory(batchID uint) (*models.Batch, error) {
	batch, err := s.Get(batchID)
	if err != nil {
		return nil, err
	}
	if len(batch.Items) > 0 || batch.ClosedAt == nil {
		return batch, nil
	}
	items, err := s.repo.Batch.AllBatchItemsDetailed(batch.ID)
	if err != nil {
		return nil, apperr.Internal("could not load scrapped batch items").Wrap(err)
	}
	batch.Items = items
	return batch, nil
}

// ProductionTemplateXLSX loads a batch and renders the legacy production template as
// a real .xlsx workbook. Unlike CSV, an xlsx always splits into columns cleanly in
// Excel regardless of the machine's locale/list separator, and Vietnamese headers
// need no BOM. The header row is bold on a light fill, frozen, and auto-filtered,
// with per-column widths so it is readable on open.
func (s *BatchService) ProductionTemplateXLSX(batchID uint) ([]byte, string, error) {
	batch, err := s.GetWithScrapHistory(batchID)
	if err != nil {
		return nil, "", err
	}
	grid := ProductionTemplateGrid(batch)

	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	const sheet = "Sản xuất"
	if err := f.SetSheetName("Sheet1", sheet); err != nil {
		return nil, "", apperr.Internal("could not build production workbook").Wrap(err)
	}

	for r, row := range grid {
		cell, _ := excelize.CoordinatesToCellName(1, r+1)
		cells := make([]interface{}, len(row))
		for i, v := range row {
			cells[i] = v
		}
		if err := f.SetSheetRow(sheet, cell, &cells); err != nil {
			return nil, "", apperr.Internal("could not write production rows").Wrap(err)
		}
	}

	lastCol, _ := excelize.ColumnNumberToName(len(productionTemplateHeaders))
	for i, w := range productionColumnWidths {
		name, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, name, name, w)
	}

	if style, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "1F2A44"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"E9EDF5"}},
		Alignment: &excelize.Alignment{Vertical: "center"},
	}); err == nil {
		_ = f.SetCellStyle(sheet, "A1", lastCol+"1", style)
	}
	_ = f.SetRowHeight(sheet, 1, 22)
	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})
	_ = f.AutoFilter(sheet, "A1:"+lastCol+itoa(len(grid)), []excelize.AutoFilterOptions{})

	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, "", apperr.Internal("could not render production workbook").Wrap(err)
	}
	filename := "production-" + strings.ReplaceAll(batch.Code, "#", "") + ".xlsx"
	return buf.Bytes(), filename, nil
}

// BatchZipName returns the download filename for a batch asset ZIP, e.g.
// "Batch_101000.zip", derived from the batch code (# stripped).
func BatchZipName(batch *models.Batch) string {
	return "Batch_" + sanitizeZipComponent(batch.Code) + ".zip"
}

// StreamBatchAssetsZip streams a batch's asset URLs into a ZIP archive.
//
// When designOnly is true it bundles ONLY the original design files (front + back)
// into a single "Batch_<code>" folder, each named "SKU_INTERNALCODE_QUANTITY[_SIDE].EXT"
// (see designFileName) and written in SKU order — no mockup, no print/cut —
// matching the "download design" requirement. When false it keeps
// the full production bundle (design, mockup, print, cut) with the legacy flat
// naming plus a manifest, so the existing "Download ZIP" on the batch board is
// unchanged. A single broken/blocked asset URL is skipped (recorded), not fatal.
func (s *BatchService) StreamBatchAssetsZip(ctx context.Context, w io.Writer, batchID uint, designOnly bool) error {
	batch, err := s.GetWithScrapHistory(batchID)
	if err != nil {
		return err
	}

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	// SSRF-safe client: refuses to connect to non-public IPs (see safeurl.go).
	// Asset URLs come from seller import data, so a plain client would be an SSRF sink.
	client := newSafeAssetClient(30 * time.Second)
	manifest := make([]string, 0)
	usedNames := map[string]int{}
	written := 0
	batchPrintURL, batchCutURL := batchProductionLinks(batch)
	// Design-only downloads gather every file into one "Batch_<code>" folder so the
	// designer can tell one batch's pull from another; matches BatchZipName's .zip.
	designFolder := "Batch_" + sanitizeZipComponent(batch.Code)

	if designOnly {
		// Written in SKU order so the archive matches how the extracted folder
		// sorts (see sortDesignItemsBySKU) — batch.Items is in batch-line order.
		ordered := make([]*models.OrderItem, 0, len(batch.Items))
		for _, bi := range batch.Items {
			it := bi.OrderItem
			if it == nil || itemCancelled(it.CancellationStatus) {
				continue
			}
			ordered = append(ordered, it)
		}
		sortDesignItemsBySKU(ordered)

		failed := make([]string, 0)
		for _, it := range ordered {
			for _, a := range designAssetsForItem(it) {
				entryBase := designFileBase(designFolder, it.InternalCode, it.SKUCode, it.Quantity, a.side)
				if err := writeURLToZipEntry(ctx, client, zw, a.url, entryBase, usedNames); err != nil {
					// Skip one broken design file, keep the rest — but list it in the note.
					failed = append(failed, describeAssetFailure(it, a, err))
					continue
				}
				written++
			}
		}

		if written == 0 {
			return apperr.Unprocessable(noDesignFilesMessage(failed))
		}
		if err := writeZipErrorNote(zw, designFolder, failed); err != nil {
			return err
		}
		return zw.Close()
	}

	for _, bi := range batch.Items {
		it := bi.OrderItem
		if it == nil || itemCancelled(it.CancellationStatus) {
			continue
		}

		printURL, cutURL := batchPrintURL, batchCutURL
		if printURL == "" {
			printURL = it.PrintFileURL
		}
		if cutURL == "" {
			cutURL = it.CutFileURL
		}
		assets := []struct {
			url   string
			type_ string
		}{
			{it.DesignURL, "design"},
			{it.BackDesignURL, "design-back"},
			{it.MockupURL, "mockup"},
			{printURL, "print"},
			{cutURL, "cut"},
		}
		code := sanitizeZipComponent(it.InternalCode)
		if code == "" {
			code = sanitizeZipComponent(batch.Code)
		}
		for _, asset := range assets {
			if strings.TrimSpace(asset.url) == "" {
				continue
			}
			if err := writeURLToZipEntry(ctx, client, zw, asset.url, code+"-"+asset.type_, usedNames); err != nil {
				continue
			}
			manifest = append(manifest, fmt.Sprintf("%s,%s,%s", code, asset.type_, asset.url))
			written++
		}
	}

	if written == 0 {
		return apperr.Unprocessable("No assets available for batch ZIP download")
	}

	m, err := zw.Create("manifest.txt")
	if err != nil {
		return apperr.Internal("could not write ZIP manifest").Wrap(err)
	}
	if _, err := io.WriteString(m, strings.Join(manifest, "\n")); err != nil {
		return apperr.Internal("could not write ZIP manifest").Wrap(err)
	}

	return zw.Close()
}

// writeURLToZipEntry downloads one asset and writes it into the archive as
// entryBase + the extension resolved from the response (see resolveAssetExt).
// entryBase carries NO extension: the real type is only knowable after the fetch,
// and naming an opaque download link ".bin" up front is what left designers with
// files their OS would not open.
func writeURLToZipEntry(ctx context.Context, client *http.Client, zw *zip.Writer, rawURL, entryBase string, usedNames map[string]int) error {
	// A pasted share link points at a viewer page, not at the bytes — rewrite it
	// to the direct-download URL before fetching (see normalizeAssetURL).
	fetchURL := normalizeAssetURL(rawURL)

	// Reject non-http(s) schemes and private/loopback hosts before dialing. The
	// client's dial-time guard is the authoritative SSRF check (also covers
	// redirects + rebinding); this gives an early, clear rejection.
	u, err := validatePublicHTTPURL(fetchURL)
	if err != nil {
		return apperr.Unprocessable("Asset URL not allowed: " + err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return apperr.Internal("could not download asset").Wrap(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return apperr.Internal("could not download asset").Wrap(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apperr.Internal(fmt.Sprintf("asset request failed: %s -> %d", rawURL, resp.StatusCode))
	}
	// Refuse an oversized file outright instead of silently truncating it at the
	// cap and handing over a corrupt half-file.
	if resp.ContentLength > maxAssetBytes {
		return apperr.Unprocessable("file vượt quá giới hạn 100MB")
	}

	// The ORIGINAL url is passed on: it is the one that may carry a real extension
	// (a Dropbox link keeps ".png" in its path) and the one a user must fix if the
	// fetch turns out to be a web page.
	return writeResponseToZipEntry(zw, resp, rawURL, entryBase, usedNames)
}

// writeResponseToZipEntry is the half of the download that needs no network, so
// it can be tested directly: name the entry from what came back, then stream the
// body into it.
func writeResponseToZipEntry(zw *zip.Writer, resp *http.Response, rawURL, entryBase string, usedNames map[string]int) error {
	// Peek at the first bytes so the type can be sniffed when neither the URL nor
	// the headers name it — then put them back in front of the body.
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return apperr.Internal("could not read asset").Wrap(err)
	}
	head = head[:n]

	// A web page is never a design file: putting it in the ZIP is how a Drive
	// viewer page ended up masquerading as artwork. Fail the asset instead.
	if isHTMLResponse(resp.Header, head) {
		return apperr.Unprocessable(assetFetchHint(rawURL))
	}

	entryName := reserveZipName(usedNames, entryBase, resolveAssetExt(rawURL, resp.Header, head))
	entry, err := zw.Create(entryName)
	if err != nil {
		return apperr.Internal("could not write ZIP entry").Wrap(err)
	}

	// Cap per-asset size so a hostile/huge remote file can't exhaust resources.
	body := io.MultiReader(bytes.NewReader(head), resp.Body)
	if _, err = io.Copy(entry, io.LimitReader(body, maxAssetBytes)); err != nil {
		return apperr.Internal("could not stream asset into ZIP").Wrap(err)
	}
	return nil
}

// sniffLen is what http.DetectContentType reads at most.
const sniffLen = 512

func sanitizeZipComponent(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "#", "")
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, " ", "_")
	return value
}

// describeSkips turns the per-item skip reasons into one readable sentence —
// "100001_3/3 (đã nằm trong một batch khác của NVL này); 100002_1/1 (đơn chưa
// được duyệt)" — so the user sees which product failed which rule instead of a
// generic refusal listing every rule at once.
func describeSkips(order []uint, reasons map[uint]string, label func(uint) string) string {
	const maxShown = 3
	parts := make([]string, 0, maxShown)
	shown := 0
	for _, id := range order {
		reason, ok := reasons[id]
		if !ok {
			continue
		}
		if shown < maxShown {
			parts = append(parts, label(id)+" ("+reason+")")
		}
		shown++
	}
	out := strings.Join(parts, "; ")
	if shown > maxShown {
		out += fmt.Sprintf(" … và %d sản phẩm khác", shown-maxShown)
	}
	return out
}
