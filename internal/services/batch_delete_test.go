package services

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// TestBatchDelete_ReleasesItemsForRebatch is the core promise AND the unique-index
// regression test: deleting a PENDING batch must hard-delete its parts, blank the
// stamped print/cut file, drop the item back to PENDING — and, crucially, let the
// very same items be batched again. A soft delete of batch_items would pass every
// other assertion and die here on idx_item_material_attempt (no deleted_at
// predicate) because NextAttempts never sees soft-deleted rows.
func TestBatchDelete_ReleasesItemsForRebatch(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	actor := Actor{ID: 1, Role: models.RoleDesigner}
	_, ids := seedSplit(t, db, 0, 2)

	batchAll, _, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})

	batch := firstBatch(t, batchAll, err)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.SetBatchLink(actor, batch.ID, SetBatchLinkInput{Kind: "PRINT", URL: "https://files.example.com/print-1.pdf"}); err != nil {
		t.Fatalf("set link: %v", err)
	}

	if err := svc.Delete(actor, batch.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Header soft-deleted (gone from queries, still there for audit)…
	if err := db.First(&models.Batch{}, batch.ID).Error; err != gorm.ErrRecordNotFound {
		t.Fatalf("batch should be soft-deleted, got err=%v", err)
	}
	var kept int64
	db.Unscoped().Model(&models.Batch{}).Where("id = ?", batch.ID).Count(&kept)
	if kept != 1 {
		t.Fatalf("batch header should survive unscoped, got %d", kept)
	}
	// …parts hard-deleted: even Unscoped finds nothing.
	var parts int64
	db.Unscoped().Model(&models.BatchItem{}).Where("batch_id = ?", batch.ID).Count(&parts)
	if parts != 0 {
		t.Fatalf("batch items must be hard-deleted, %d rows remain", parts)
	}

	// Items are back in the pool: PENDING, stamped print file gone.
	var items []models.OrderItem
	if err := db.Find(&items, ids).Error; err != nil {
		t.Fatalf("load items: %v", err)
	}
	for _, it := range items {
		if it.InternalStatus != models.StatusPending {
			t.Fatalf("item %d should be PENDING, got %s", it.ID, it.InternalStatus)
		}
		if it.PrintFileURL != "" {
			t.Fatalf("item %d should have print_file_url cleared, got %q", it.ID, it.PrintFileURL)
		}
	}

	// The same items regroup into a fresh batch with nothing skipped.
	againAll, skipped, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	again := firstBatch(t, againAll, err)
	if err != nil {
		t.Fatalf("re-batch after delete: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("re-batch skipped %v, want none", skipped)
	}
	var newParts int64
	db.Model(&models.BatchItem{}).Where("batch_id = ?", again.ID).Count(&newParts)
	if newParts != 2 {
		t.Fatalf("new batch should hold 2 parts, got %d", newParts)
	}
}

// TestBatchDelete_BlockedOnceProductionStarted: an advanced header refuses, and —
// header lag aside — so does a single touched part under a still-PENDING header.
func TestBatchDelete_BlockedOnceProductionStarted(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	actor := Actor{ID: 1, Role: models.RoleDesigner}
	_, ids := seedSplit(t, db, 0, 2)

	batchAll, _, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})

	batch := firstBatch(t, batchAll, err)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// One part touched by production while the header still says PENDING.
	var part models.BatchItem
	if err := db.Where("batch_id = ?", batch.ID).First(&part).Error; err != nil {
		t.Fatalf("load part: %v", err)
	}
	if err := db.Model(&part).Update("status", models.StatusPrinted).Error; err != nil {
		t.Fatalf("advance part: %v", err)
	}
	if err := svc.Delete(actor, batch.ID); err == nil {
		t.Fatal("delete should refuse a batch with a produced part")
	}

	// Header advanced too: still refused, batch and parts untouched.
	if err := db.Model(&models.Batch{}).Where("id = ?", batch.ID).Update("status", models.StatusPrinted).Error; err != nil {
		t.Fatalf("advance header: %v", err)
	}
	if err := svc.Delete(actor, batch.ID); err == nil {
		t.Fatal("delete should refuse a PRINTED batch")
	}
	var parts int64
	db.Model(&models.BatchItem{}).Where("batch_id = ?", batch.ID).Count(&parts)
	if parts != 2 {
		t.Fatalf("refused delete must leave parts intact, got %d", parts)
	}
}

// TestBatchDelete_OneSheetOfMany: a pool that fills several sheets becomes
// several flat batches; each is deleted on its own, the others stay, and the
// released items regroup into a fresh batch.
func TestBatchDelete_OneSheetOfMany(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	actor := Actor{ID: 1, Role: models.RoleDesigner}
	_, ids := seedSplit(t, db, 2, 5) // quota 2, 5 products → 3 flat batches

	batches, _, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(batches) != 3 {
		t.Fatalf("want 3 flat batches, got %d", len(batches))
	}
	first := batches[0]
	firstItems, err := svc.repo.Batch.OrderItemIDsForBatches([]uint{first.ID})
	if err != nil {
		t.Fatalf("items of first: %v", err)
	}

	if err := svc.Delete(actor, first.ID); err != nil {
		t.Fatalf("delete one sheet: %v", err)
	}
	var live int64
	db.Model(&models.Batch{}).Count(&live)
	if live != 2 {
		t.Fatalf("the other two sheets must stay, got %d live batches", live)
	}
	var parts int64
	db.Unscoped().Model(&models.BatchItem{}).Where("batch_id = ?", first.ID).Count(&parts)
	if parts != 0 {
		t.Fatalf("deleted sheet's parts should be hard-deleted, %d remain", parts)
	}

	// The released items are back to PENDING and regroup on their own.
	again, skipped, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: firstItems})
	if err != nil {
		t.Fatalf("re-batch after delete: %v", err)
	}
	if len(skipped) != 0 || len(again) != 1 || len(again[0].Items) != len(firstItems) {
		t.Fatalf("re-batch: skipped %v, batches %d", skipped, len(again))
	}
}

// ---------- Xoá batch chỉ còn hàng đã duyệt huỷ (anh Kiên 11/11) ----------

// batchDeleteMeta là phần metadata BATCH_DELETE mà các test dưới đọc lại.
type batchDeleteMeta struct {
	ReleasedItemIDs   []uint `json:"released_item_ids"`
	OnlyCancelled     bool   `json:"only_cancelled_items"`
	ProducedCancelled int    `json:"produced_cancelled_parts"`
	StatusKeptItemIDs []uint `json:"status_kept_item_ids"`
	Parts             []struct {
		BatchItemID        uint   `json:"batch_item_id"`
		OrderItemID        uint   `json:"order_item_id"`
		ItemCode           string `json:"item_code"`
		Status             string `json:"status"`
		Scrapped           bool   `json:"scrapped"`
		CancellationStatus string `json:"cancellation_status"`
	} `json:"parts"`
	Links []struct {
		Kind string `json:"kind"`
		URL  string `json:"url"`
	} `json:"links"`
}

// createDeleteFixture gom n sản phẩm vào MỘT batch PENDING và trả batch cùng
// phần của từng sản phẩm (theo thứ tự ids).
func createDeleteFixture(t *testing.T, db *gorm.DB, svc *BatchService, actor Actor, n int) (*models.Batch, []uint, map[uint]models.BatchItem) {
	t.Helper()
	_, ids := seedSplit(t, db, 0, n)
	batchAll, _, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, err)
	var parts []models.BatchItem
	if err := db.Where("batch_id = ?", batch.ID).Find(&parts).Error; err != nil {
		t.Fatalf("load parts: %v", err)
	}
	byItem := map[uint]models.BatchItem{}
	for _, p := range parts {
		byItem[p.OrderItemID] = p
	}
	if len(byItem) != n {
		t.Fatalf("fixture: want %d parts, got %d", n, len(byItem))
	}
	return batch, ids, byItem
}

// produce đưa phần và sản phẩm tới một trạng thái sản xuất — như xưởng đã in /
// cắt / QC thật — mà KHÔNG đụng header batch (dữ liệu prod: header vẫn PENDING).
func produce(t *testing.T, db *gorm.DB, part models.BatchItem, status models.InternalStatus) {
	t.Helper()
	if err := db.Model(&models.BatchItem{}).Where("id = ?", part.ID).Update("status", status).Error; err != nil {
		t.Fatalf("advance part %d: %v", part.ID, err)
	}
	if err := db.Model(&models.OrderItem{}).Where("id = ?", part.OrderItemID).Update("internal_status", status).Error; err != nil {
		t.Fatalf("advance item %d: %v", part.OrderItemID, err)
	}
}

// setCancellation đặt trạng thái huỷ của dòng đơn như luồng duyệt huỷ để lại.
func setCancellation(t *testing.T, db *gorm.DB, itemID uint, status models.CancellationStatus) {
	t.Helper()
	if err := db.Model(&models.OrderItem{}).Where("id = ?", itemID).Updates(map[string]any{
		"cancellation_status": status,
		"cancel_stage":        models.CancelStageInProduction,
		"cancel_billable":     true,
	}).Error; err != nil {
		t.Fatalf("cancel item %d: %v", itemID, err)
	}
}

func itemStatuses(t *testing.T, db *gorm.DB, ids []uint) map[uint]models.InternalStatus {
	t.Helper()
	var items []models.OrderItem
	if err := db.Find(&items, ids).Error; err != nil {
		t.Fatalf("load items: %v", err)
	}
	out := map[uint]models.InternalStatus{}
	for _, it := range items {
		out[it.ID] = it.InternalStatus
	}
	return out
}

func lastBatchDeleteAudit(t *testing.T, db *gorm.DB, batchID uint) (models.AuditLog, batchDeleteMeta) {
	t.Helper()
	var log models.AuditLog
	if err := db.Where("action = ? AND entity_id = ?", "BATCH_DELETE", batchID).Order("id desc").First(&log).Error; err != nil {
		t.Fatalf("BATCH_DELETE audit missing: %v", err)
	}
	var meta batchDeleteMeta
	if err := json.Unmarshal(log.Metadata, &meta); err != nil {
		t.Fatalf("decode audit metadata: %v", err)
	}
	return log, meta
}

// TestBatchDelete_OnlyApprovedCancelledParts là đúng ca #102178/#102183: hàng đã
// in, cắt, QC thật rồi đơn bị huỷ và huỷ đã duyệt. Header vẫn PENDING, màn chi
// tiết thấy "0 item". Xoá phải được; mỗi item GIỮ NGUYÊN trạng thái đã đạt; nhật
// ký ghi từng phần và cờ chỉ còn hàng đã huỷ.
func TestBatchDelete_OnlyApprovedCancelledParts(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	actor := Actor{ID: 1, Role: models.RoleDesigner}
	batch, ids, parts := createDeleteFixture(t, db, svc, actor, 3)
	if _, err := svc.SetBatchLink(actor, batch.ID, SetBatchLinkInput{Kind: "CUT", URL: "https://files.example.com/cut-1.dxf"}); err != nil {
		t.Fatalf("set link: %v", err)
	}

	want := map[uint]models.InternalStatus{
		ids[0]: models.StatusPrinted,
		ids[1]: models.StatusCut,
		ids[2]: models.StatusQCPassed,
	}
	for id, st := range want {
		produce(t, db, parts[id], st)
		setCancellation(t, db, id, models.CancellationApproved)
	}

	if err := svc.Delete(actor, batch.ID); err != nil {
		t.Fatalf("delete batch of approved-cancelled goods: %v", err)
	}

	// Header soft-deleted, parts hard-deleted — same mechanics as a PENDING delete.
	if err := db.First(&models.Batch{}, batch.ID).Error; err != gorm.ErrRecordNotFound {
		t.Fatalf("batch should be soft-deleted, got err=%v", err)
	}
	var left int64
	db.Unscoped().Model(&models.BatchItem{}).Where("batch_id = ?", batch.ID).Count(&left)
	if left != 0 {
		t.Fatalf("parts must be hard-deleted, %d remain", left)
	}

	// The proof of production stays on the items.
	got := itemStatuses(t, db, ids)
	for id, st := range want {
		if got[id] != st {
			t.Fatalf("item %d: status must stay %s, got %s", id, st, got[id])
		}
	}
	var rolled int64
	db.Model(&models.StatusHistory{}).
		Where("entity_type = ? AND entity_id IN ? AND to_status = ?", models.EntityOrderItem, ids, models.StatusPending).
		Count(&rolled)
	if rolled != 0 {
		t.Fatalf("no item may be rolled back to PENDING, %d history rows say otherwise", rolled)
	}

	log, meta := lastBatchDeleteAudit(t, db, batch.ID)
	if !meta.OnlyCancelled {
		t.Fatalf("only_cancelled_items must be true, summary %q", log.Summary)
	}
	if !strings.Contains(log.Summary, batch.Code) {
		t.Fatalf("summary should name the batch, got %q", log.Summary)
	}
	if meta.ProducedCancelled != 3 || len(meta.StatusKeptItemIDs) != 3 {
		t.Fatalf("want 3 produced cancelled parts / 3 kept items, got %d / %v", meta.ProducedCancelled, meta.StatusKeptItemIDs)
	}
	if len(meta.Parts) != 3 {
		t.Fatalf("audit must list every deleted part, got %d", len(meta.Parts))
	}
	for _, p := range meta.Parts {
		if p.Status != string(want[p.OrderItemID]) || p.CancellationStatus != string(models.CancellationApproved) ||
			p.BatchItemID != parts[p.OrderItemID].ID || p.ItemCode == "" || p.Scrapped {
			t.Fatalf("audit part wrong: %+v (want status %s)", p, want[p.OrderItemID])
		}
	}
	if len(meta.Links) != 1 || meta.Links[0].Kind != "CUT" {
		t.Fatalf("audit should keep the batch's cut link, got %+v", meta.Links)
	}

	// Đường vòng: một màn tạo batch cũ gửi lại chính các item đã huỷ. Create bỏ
	// chúng (đã huỷ) rồi vẫn chạy roll-up — roll-up cũng không được hạ chúng.
	if _, _, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids}); err == nil {
		t.Fatal("cancelled items must not be batched again")
	}
	got = itemStatuses(t, db, ids)
	for id, st := range want {
		if got[id] != st {
			t.Fatalf("after re-batch attempt, item %d must stay %s, got %s", id, st, got[id])
		}
	}
}

// TestBatchDelete_BlockedByProducedLivePart: chỉ cần MỘT phần đã sản xuất của
// dòng chưa duyệt huỷ là chặn, câu báo y như cũ, không đụng gì. Yêu cầu huỷ còn
// chờ (REQUESTED) chưa phải huỷ; phần ghi bỏ ở QC của dòng chưa huỷ cũng chặn.
func TestBatchDelete_BlockedByProducedLivePart(t *testing.T) {
	const msg = "Batch có phần đã được sản xuất hoặc đã huỷ tại QC — chỉ xoá được batch chưa sản xuất."
	cases := []struct {
		name string
		live func(t *testing.T, db *gorm.DB, part models.BatchItem)
	}{
		{"printed, not cancelled", func(t *testing.T, db *gorm.DB, p models.BatchItem) {
			produce(t, db, p, models.StatusPrinted)
		}},
		{"cut, cancellation only requested", func(t *testing.T, db *gorm.DB, p models.BatchItem) {
			produce(t, db, p, models.StatusCut)
			setCancellation(t, db, p.OrderItemID, models.CancellationRequested)
		}},
		{"scrapped at QC, not cancelled", func(t *testing.T, db *gorm.DB, p models.BatchItem) {
			if err := db.Model(&models.BatchItem{}).Where("id = ?", p.ID).Update("scrapped_at", time.Now()).Error; err != nil {
				t.Fatalf("scrap part: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newSplitDB(t)
			svc := newBatchService(db)
			actor := Actor{ID: 1, Role: models.RoleDesigner}
			batch, ids, parts := createDeleteFixture(t, db, svc, actor, 3)
			// Two approved-cancelled produced parts that alone would be deletable…
			produce(t, db, parts[ids[0]], models.StatusCut)
			setCancellation(t, db, ids[0], models.CancellationApproved)
			produce(t, db, parts[ids[1]], models.StatusQCPassed)
			setCancellation(t, db, ids[1], models.CancellationApproved)
			// …and one live line production has touched.
			tc.live(t, db, parts[ids[2]])
			before := itemStatuses(t, db, ids)

			err := svc.Delete(actor, batch.ID)
			if err == nil {
				t.Fatal("delete must refuse a batch with a produced part of a live line")
			}
			if ae, ok := apperr.As(err); !ok || ae.Message != msg {
				t.Fatalf("message must stay unchanged, got %v", err)
			}
			var n int64
			db.Model(&models.BatchItem{}).Where("batch_id = ?", batch.ID).Count(&n)
			if n != 3 {
				t.Fatalf("refused delete must leave parts intact, got %d", n)
			}
			if err := db.First(&models.Batch{}, batch.ID).Error; err != nil {
				t.Fatalf("refused delete must leave the batch: %v", err)
			}
			after := itemStatuses(t, db, ids)
			for id, st := range before {
				if after[id] != st {
					t.Fatalf("item %d moved %s → %s on a refused delete", id, st, after[id])
				}
			}
			var audits int64
			db.Model(&models.AuditLog{}).Where("action = ?", "BATCH_DELETE").Count(&audits)
			if audits != 0 {
				t.Fatalf("a refused delete writes no BATCH_DELETE audit, got %d", audits)
			}
		})
	}
}

// TestBatchDelete_AllPendingAsBefore: batch chưa sản xuất xoá như cũ — sản phẩm về
// PENDING qua roll-up (kể cả một dòng mang trạng thái lệch), nhật ký ghi cờ false.
func TestBatchDelete_AllPendingAsBefore(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	actor := Actor{ID: 1, Role: models.RoleDesigner}
	batch, ids, _ := createDeleteFixture(t, db, svc, actor, 2)
	// A stale item status (part still PENDING): the roll-up must still correct a
	// live item — the "keep status" rule is for approved cancellations only.
	if err := db.Model(&models.OrderItem{}).Where("id = ?", ids[0]).Update("internal_status", models.StatusPrinted).Error; err != nil {
		t.Fatalf("stale status: %v", err)
	}

	if err := svc.Delete(actor, batch.ID); err != nil {
		t.Fatalf("delete PENDING batch: %v", err)
	}
	for id, st := range itemStatuses(t, db, ids) {
		if st != models.StatusPending {
			t.Fatalf("item %d should be back to PENDING, got %s", id, st)
		}
	}
	_, meta := lastBatchDeleteAudit(t, db, batch.ID)
	if meta.OnlyCancelled || meta.ProducedCancelled != 0 || len(meta.StatusKeptItemIDs) != 0 {
		t.Fatalf("pending batch: flag must be false and nothing kept, got %+v", meta)
	}
	if len(meta.ReleasedItemIDs) != 2 || len(meta.Parts) != 2 {
		t.Fatalf("audit should list 2 released items / 2 parts, got %v / %d", meta.ReleasedItemIDs, len(meta.Parts))
	}
	for _, p := range meta.Parts {
		if p.Status != string(models.StatusPending) {
			t.Fatalf("pending batch part logged as %s", p.Status)
		}
	}
}

// TestBatchDelete_MixedPendingLiveAndCancelledProduced: phần chưa sản xuất của
// dòng sống + phần đã cắt của dòng đã duyệt huỷ → xoá được; dòng sống về PENDING
// (về hàng chờ gom), dòng đã huỷ giữ CUT; không phải "chỉ còn hàng đã huỷ".
func TestBatchDelete_MixedPendingLiveAndCancelledProduced(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	actor := Actor{ID: 1, Role: models.RoleDesigner}
	batch, ids, parts := createDeleteFixture(t, db, svc, actor, 2)
	produce(t, db, parts[ids[1]], models.StatusCut)
	setCancellation(t, db, ids[1], models.CancellationApproved)

	if err := svc.Delete(actor, batch.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got := itemStatuses(t, db, ids)
	if got[ids[0]] != models.StatusPending || got[ids[1]] != models.StatusCut {
		t.Fatalf("live item → PENDING, cancelled item keeps CUT; got %v", got)
	}
	_, meta := lastBatchDeleteAudit(t, db, batch.ID)
	if meta.OnlyCancelled {
		t.Fatal("a batch with a live line is not 'only cancelled goods'")
	}
	if meta.ProducedCancelled != 1 || len(meta.StatusKeptItemIDs) != 1 || meta.StatusKeptItemIDs[0] != ids[1] {
		t.Fatalf("want 1 produced cancelled part kept on item %d, got %+v", ids[1], meta)
	}
	// The live item is back in the pool and can be batched again.
	if _, skipped, err := svc.Create(actor, CreateBatchInput{MaterialID: 1, OrderItemIDs: []uint{ids[0]}}); err != nil || len(skipped) != 0 {
		t.Fatalf("live item should re-batch: err=%v skipped=%v", err, skipped)
	}
}
