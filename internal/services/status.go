package services

import (
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// recordStatus appends a status-history row. Errors are intentionally ignored at
// call sites that treat history as best-effort; callers that care check the return.
func recordStatus(repo *repositories.Repositories, entityType models.EntityType, entityID uint, from, to string, actor Actor, note string) error {
	return repo.Status.Create(&models.StatusHistory{
		EntityType:  entityType,
		EntityID:    entityID,
		FromStatus:  from,
		ToStatus:    to,
		ChangedByID: actor.IDPtr(),
		Note:        note,
	})
}

// deriveItemStatusFromBatchItems computes an order item's internal status as the
// least-advanced status across its batch parts. An item with no batch parts is
// PENDING; an item is QC_PASSED only when every part is QC_PASSED.
func deriveItemStatusFromBatchItems(parts []models.BatchItem) models.InternalStatus {
	if len(parts) == 0 {
		return models.StatusPending
	}
	min := models.StatusQCPassed
	for _, p := range parts {
		if p.Status.Rank() < min.Rank() {
			min = p.Status
		}
	}
	return min
}

// recomputeBatchStatus recalculates and persists a batch's production status as
// the least-advanced status across its batch items — the mirror image of the
// item-level roll-up. A batch reaches QC_PASSED only when every one of its items
// is QC_PASSED. This keeps the batch header in sync after a QC scan advances an
// item outside the top-down board cascade (BatchService.UpdateStatus). It records
// a status-history row when the status actually changes; a batch with no items is
// left untouched. Loads the batch WITHOUT association preloads and writes only
// the status column, so the roll-up stays cheap and can't clobber concurrent
// edits to other batch fields.
func recomputeBatchStatus(repo *repositories.Repositories, batchID uint, actor Actor) error {
	items, err := repo.Batch.BatchItemsForBatch(batchID)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	newStatus := deriveItemStatusFromBatchItems(items)
	batch, err := repo.Batch.FindLite(batchID)
	if err != nil {
		return err
	}
	if newStatus != batch.Status {
		old := string(batch.Status)
		if err := repo.Batch.UpdateStatusColumn(batch.ID, newStatus); err != nil {
			return err
		}
		_ = recordStatus(repo, models.EntityBatch, batch.ID, old, string(newStatus), actor, "derived from batch items")
	}
	return nil
}

// recomputeBatchStatuses is recomputeBatchStatus over a set of batches, at a
// fixed cost instead of one roll-up each.
//
// A combo product's parts live in one batch per material, so a single QC scan can
// move two or three batches at once; the per-batch version spent four round trips
// apiece and the operator paid for all of them before the screen came back. Here
// the whole set costs one read of the parts, one read of the batches, one update
// per distinct resulting status (at most four) and one history insert.
//
// Semantics match the single version exactly: a batch with no live parts is left
// untouched and a batch that does not move writes no history.
func recomputeBatchStatuses(repo *repositories.Repositories, batchIDs []uint, actor Actor) error {
	if len(batchIDs) == 0 {
		return nil
	}
	partStatuses, err := repo.Batch.BatchItemStatusesForBatches(batchIDs)
	if err != nil {
		return err
	}
	batches, err := repo.Batch.FindLiteMany(batchIDs)
	if err != nil {
		return err
	}

	var history []models.StatusHistory
	moved := map[models.InternalStatus][]uint{}
	for _, b := range batches {
		parts, ok := partStatuses[b.ID]
		if !ok || len(parts) == 0 {
			continue // no live parts — nothing to derive from
		}
		newStatus := deriveItemStatusesFromRanks(parts)
		if newStatus == b.Status {
			continue
		}
		moved[newStatus] = append(moved[newStatus], b.ID)
		history = append(history, models.StatusHistory{
			EntityType: models.EntityBatch, EntityID: b.ID,
			FromStatus: string(b.Status), ToStatus: string(newStatus),
			ChangedByID: actor.IDPtr(), Note: "derived from batch items",
		})
	}
	for status, ids := range moved {
		if err := repo.Batch.UpdateStatusColumns(ids, status); err != nil {
			return err
		}
	}
	return repo.Status.CreateBulk(history)
}

// recomputeOrderItemStatus recalculates and persists one item's internal status
// from its batch parts and returns the status it landed on — the single-item
// convenience over the bulk version. Callers that already hold the item use the
// returned status to refresh it in place instead of re-reading it.
func recomputeOrderItemStatus(repo *repositories.Repositories, itemID uint, actor Actor) (models.InternalStatus, error) {
	final, err := recomputeOrderItemStatuses(repo, []uint{itemID}, actor)
	return final[itemID], err
}

// recomputeOrderItemStatuses recalculates and persists the internal status of
// many items at once: one query for every item's batch parts, one for the
// items' current statuses, then a targeted status update per changed item and a
// single bulk history insert. Replaces the per-item recompute that used to load
// each item with five association preloads.
//
// The returned map carries the status EVERY known item ended on, unchanged ones
// included, so a caller holding the item in memory never has to re-read it just
// to learn what it became. Items missing from the map were not found.
func recomputeOrderItemStatuses(repo *repositories.Repositories, itemIDs []uint, actor Actor) (map[uint]models.InternalStatus, error) {
	final := map[uint]models.InternalStatus{}
	if len(itemIDs) == 0 {
		return final, nil
	}
	partStatuses, err := repo.Batch.BatchItemStatusesForOrderItems(itemIDs)
	if err != nil {
		return final, err
	}
	current, err := repo.OrderItem.InternalStatusByIDs(itemIDs)
	if err != nil {
		return final, err
	}
	var history []models.StatusHistory
	// Items are grouped by the status they land on, so the writes below are one
	// statement per distinct status (at most four) instead of one per item.
	moved := map[models.InternalStatus][]uint{}
	for _, id := range itemIDs {
		cur, ok := current[id]
		if !ok {
			continue // item not found (deleted) — nothing to roll up
		}
		newStatus := deriveItemStatusesFromRanks(partStatuses[id])
		final[id] = newStatus
		if newStatus == cur {
			continue
		}
		moved[newStatus] = append(moved[newStatus], id)
		history = append(history, models.StatusHistory{
			EntityType: models.EntityOrderItem, EntityID: id,
			FromStatus: string(cur), ToStatus: string(newStatus),
			ChangedByID: actor.IDPtr(), Note: "derived from batch parts",
		})
	}
	for status, ids := range moved {
		if err := repo.OrderItem.UpdateInternalStatuses(ids, status); err != nil {
			return final, err
		}
	}
	return final, repo.Status.CreateBulk(history)
}

// deriveItemStatusesFromRanks is deriveItemStatusFromBatchItems over a bare
// status slice (the bulk recompute fetches statuses, not full rows).
func deriveItemStatusesFromRanks(statuses []models.InternalStatus) models.InternalStatus {
	if len(statuses) == 0 {
		return models.StatusPending
	}
	min := models.StatusQCPassed
	for _, s := range statuses {
		if s.Rank() < min.Rank() {
			min = s
		}
	}
	return min
}
