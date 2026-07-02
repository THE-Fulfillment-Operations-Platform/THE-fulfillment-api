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

// recomputeOrderItemStatus recalculates and persists an item's internal status
// from its batch parts, recording a status-history row if it changed.
func recomputeOrderItemStatus(repo *repositories.Repositories, itemID uint, actor Actor) (*models.OrderItem, error) {
	item, err := repo.OrderItem.FindByID(itemID)
	if err != nil {
		return nil, err
	}
	parts, err := repo.Batch.BatchItemsForOrderItem(itemID)
	if err != nil {
		return nil, err
	}
	newStatus := deriveItemStatusFromBatchItems(parts)
	if newStatus != item.InternalStatus {
		old := string(item.InternalStatus)
		item.InternalStatus = newStatus
		if err := repo.OrderItem.Update(item); err != nil {
			return nil, err
		}
		_ = recordStatus(repo, models.EntityOrderItem, item.ID, old, string(newStatus), actor, "derived from batch parts")
	}
	return item, nil
}
