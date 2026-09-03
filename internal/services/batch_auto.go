package services

import (
	"fmt"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// MaxAutoBatchItemsPerMaterial is the safety cap for one material in one
// auto-create run. A pool this size means something upstream is off; the
// material is reported as skipped with an explicit reason instead of being
// silently truncated.
const MaxAutoBatchItemsPerMaterial = 2000

// AutoCreatedBatch reports what auto-create built for one material: the root
// batch (flat, or the parent when the quota split kicked in) plus the codes of
// the actual production batches (children, or the flat batch itself).
type AutoCreatedBatch struct {
	MaterialID     uint     `json:"material_id"`
	MaterialCode   string   `json:"material_code"`
	MaterialName   string   `json:"material_name"`
	BatchID        uint     `json:"batch_id"`
	BatchCode      string   `json:"batch_code"`
	IsParent       bool     `json:"is_parent"`
	ChildCount     int      `json:"child_count"`
	BatchCodes     []string `json:"batch_codes"`
	ItemCount      int      `json:"item_count"`
	SkippedItemIDs []uint   `json:"skipped_item_ids"`
}

// AutoCreateSkip names a material the run could NOT batch, and why — the rule
// is to report loudly, never to guess or half-create.
type AutoCreateSkip struct {
	MaterialCode string `json:"material_code"`
	MaterialName string `json:"material_name"`
	Reason       string `json:"reason"`
}

// AutoCreateBatchesResult sums up one auto-create run.
type AutoCreateBatchesResult struct {
	Created      []AutoCreatedBatch `json:"created"`
	Skipped      []AutoCreateSkip   `json:"skipped"`
	TotalBatches int                `json:"total_batches"`
	TotalItems   int                `json:"total_items"`
}

// AutoCreateBatches batches the ENTIRE design-ready pool in one deliberate
// action: per material it reads every eligible item (approved order,
// design-ready, not cancelled, not already scheduled — a part scrapped at QC
// comes back for rework), splits by the material's products_per_unit quota and
// creates the batches with system-generated codes. Nobody picks rows or types
// batch names.
//
// Each material runs through the existing Create flow — its own transaction —
// so one material's failure rolls back only that material and is reported in
// Skipped while the others still land. The service is deliberately independent
// of any trigger: today a button calls it, a scheduler could later call it
// unchanged once the customer settles WHEN it should run.
func (s *BatchService) AutoCreateBatches(actor Actor) (*AutoCreateBatchesResult, error) {
	buckets, err := s.repo.OrderItem.MaterialBuckets()
	if err != nil {
		return nil, apperr.Internal("could not read design-ready pool").Wrap(err)
	}

	res := &AutoCreateBatchesResult{Created: []AutoCreatedBatch{}, Skipped: []AutoCreateSkip{}}
	skip := func(bucket repositories.MaterialBucket, reason string) {
		res.Skipped = append(res.Skipped, AutoCreateSkip{
			MaterialCode: bucket.MaterialCode, MaterialName: bucket.MaterialName, Reason: reason,
		})
	}
	for _, bucket := range buckets {
		// Check the cap from the bucket's own count FIRST: refusing after loading
		// the rows would mean paying for the very read the cap exists to prevent.
		if bucket.ItemCount > int64(MaxAutoBatchItemsPerMaterial) {
			skip(bucket, fmt.Sprintf("pool đang có %d sản phẩm — vượt giới hạn an toàn %d của một lần tự gom, hãy gom thủ công theo từng phần", bucket.ItemCount, MaxAutoBatchItemsPerMaterial))
			continue
		}
		items, total, err := s.repo.OrderItem.DesignReadyItemsForMaterial(
			bucket.MaterialID, repositories.Page{PageSize: repositories.PageSizeAll}.Normalize(), "", "")
		if err != nil {
			skip(bucket, "không đọc được pool sản phẩm chờ gom")
			continue
		}
		if len(items) == 0 {
			continue // the bucket emptied between the count and the read — nothing to do
		}
		// The pool can also grow between the bucket count and this read.
		if total > MaxAutoBatchItemsPerMaterial {
			skip(bucket, fmt.Sprintf("pool đang có %d sản phẩm — vượt giới hạn an toàn %d của một lần tự gom, hãy gom thủ công theo từng phần", total, MaxAutoBatchItemsPerMaterial))
			continue
		}
		ids := make([]uint, 0, len(items))
		for i := range items {
			ids = append(ids, items[i].ID)
		}
		batch, skippedIDs, err := s.Create(actor, CreateBatchInput{MaterialID: bucket.MaterialID, OrderItemIDs: ids})
		if err != nil {
			reason := "không gom được batch"
			if ae, ok := apperr.As(err); ok && ae.Message != "" {
				reason = ae.Message
			}
			skip(bucket, reason)
			continue
		}
		entry := AutoCreatedBatch{
			MaterialID: bucket.MaterialID, MaterialCode: bucket.MaterialCode, MaterialName: bucket.MaterialName,
			BatchID: batch.ID, BatchCode: batch.Code, IsParent: batch.IsParent, ChildCount: batch.ChildCount,
			BatchCodes:     []string{},
			SkippedItemIDs: skippedIDs,
		}
		if entry.SkippedItemIDs == nil {
			entry.SkippedItemIDs = []uint{}
		}
		if batch.IsParent {
			for _, child := range batch.ChildBatches {
				entry.BatchCodes = append(entry.BatchCodes, child.Code)
				entry.ItemCount += len(child.Items)
			}
		} else {
			entry.BatchCodes = append(entry.BatchCodes, batch.Code)
			entry.ItemCount = len(batch.Items)
		}
		res.Created = append(res.Created, entry)
		res.TotalBatches += len(entry.BatchCodes)
		res.TotalItems += entry.ItemCount
	}

	// One summary entry per run (Create already audits each batch). An empty
	// run writes nothing — checking an empty pool is not an event.
	if len(res.Created) > 0 || len(res.Skipped) > 0 {
		codes := make([]string, 0, len(res.Created))
		for _, c := range res.Created {
			codes = append(codes, c.BatchCode)
		}
		s.audit.Log(actor, "BATCH_AUTO_CREATE", "batch", nil,
			fmt.Sprintf("Tự tạo batch: %d batch sản xuất, %d sản phẩm, %d NVL bỏ qua", res.TotalBatches, res.TotalItems, len(res.Skipped)),
			models.JSONMap{"batch_codes": codes, "total_batches": res.TotalBatches, "total_items": res.TotalItems, "skipped": res.Skipped})
	}
	return res, nil
}
