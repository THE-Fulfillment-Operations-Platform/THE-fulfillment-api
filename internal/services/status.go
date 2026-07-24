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
	// A child batch's change rolls up to its parent's aggregate status.
	if batch.ParentBatchID != nil {
		return recomputeParentBatchStatus(repo, *batch.ParentBatchID, actor)
	}
	return nil
}

// recomputeParentBatchStatus recalculates and persists a parent batch's status as
// the least-advanced status across its child batches — the parent reaches
// QC_PASSED only when every child has. Called whenever a child's status changes
// (QC roll-up or the production board cascade). A batch with no children is left
// untouched, so it is safe to call on any batch id.
func recomputeParentBatchStatus(repo *repositories.Repositories, parentID uint, actor Actor) error {
	children, err := repo.Batch.ChildBatchesFor(parentID)
	if err != nil {
		return err
	}
	if len(children) == 0 {
		return nil
	}
	newStatus := models.StatusQCPassed
	for _, c := range children {
		if c.Status.Rank() < newStatus.Rank() {
			newStatus = c.Status
		}
	}
	parent, err := repo.Batch.FindLite(parentID)
	if err != nil {
		return err
	}
	if newStatus != parent.Status {
		old := string(parent.Status)
		if err := repo.Batch.UpdateStatusColumn(parent.ID, newStatus); err != nil {
			return err
		}
		_ = recordStatus(repo, models.EntityBatch, parent.ID, old, string(newStatus), actor, "derived from child batches")
	}
	return nil
}

// recomputeOrderItemStatus recalculates and persists one item's internal status
// from its batch parts — the single-item convenience over the bulk version.
func recomputeOrderItemStatus(repo *repositories.Repositories, itemID uint, actor Actor) error {
	return recomputeOrderItemStatuses(repo, []uint{itemID}, actor)
}

// recomputeOrderItemStatuses recalculates and persists the internal status of
// many items at once: one query for every item's batch parts, one for the
// items' current statuses, then a targeted status update per changed item and a
// single bulk history insert. Replaces the per-item recompute that used to load
// each item with five association preloads.
func recomputeOrderItemStatuses(repo *repositories.Repositories, itemIDs []uint, actor Actor) error {
	if len(itemIDs) == 0 {
		return nil
	}
	partStatuses, err := repo.Batch.BatchItemStatusesForOrderItems(itemIDs)
	if err != nil {
		return err
	}
	current, err := repo.OrderItem.InternalStatusByIDs(itemIDs)
	if err != nil {
		return err
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
			return err
		}
	}
	return repo.Status.CreateBulk(history)
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
