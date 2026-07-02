package services

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
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
	MaterialID   uint       `json:"material_id" binding:"required"`
	OrderItemIDs []uint     `json:"order_item_ids" binding:"required,min=1"`
	Priority     string     `json:"priority"`
	DueDate      *time.Time `json:"due_date"`
	Note         string     `json:"note"`
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

	var batch *models.Batch
	var skipped []uint

	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		batch = &models.Batch{
			MaterialID: material.ID, Status: models.StatusPending, Priority: priority,
			DueDate: in.DueDate, Note: in.Note, CreatedByID: actor.IDPtr(),
		}
		if err := txRepo.Batch.Create(batch); err != nil {
			return err
		}
		batch.Code = fmt.Sprintf("#%d", 101000+batch.ID)
		if err := txRepo.Batch.Update(batch); err != nil {
			return err
		}

		var batchItems []models.BatchItem
		for _, itemID := range in.OrderItemIDs {
			item, err := txRepo.OrderItem.FindByID(itemID)
			if err != nil {
				skipped = append(skipped, itemID)
				continue
			}
			// The item's SKU must include this material.
			if !skuHasMaterial(item.SKU, material.ID) {
				skipped = append(skipped, itemID)
				continue
			}
			// Skip if already scheduled for this material.
			if existing[itemID] != nil && existing[itemID][material.ID] {
				skipped = append(skipped, itemID)
				continue
			}
			batchItems = append(batchItems, models.BatchItem{
				BatchID: batch.ID, OrderItemID: item.ID, MaterialID: material.ID, Status: models.StatusPending,
			})
		}
		if len(batchItems) == 0 {
			return apperr.Unprocessable("No eligible items for this material (items must be design-ready, include the material, and not already batched)")
		}
		if err := txRepo.Batch.CreateItems(batchItems); err != nil {
			return err
		}
		for _, bi := range batchItems {
			_ = recordStatus(txRepo, models.EntityBatchItem, bi.ID, "", string(models.StatusPending), actor, "added to batch "+batch.Code)
		}
		_ = recordStatus(txRepo, models.EntityBatch, batch.ID, "", string(models.StatusPending), actor, "batch created")
		return nil
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, nil, ae
		}
		return nil, nil, apperr.Internal("could not create batch").Wrap(err)
	}

	// Recompute affected items' internal status outside the create transaction.
	for _, itemID := range in.OrderItemIDs {
		_, _ = recomputeOrderItemStatus(s.repo, itemID, actor)
	}

	full, _ := s.repo.Batch.FindByID(batch.ID)
	s.audit.Log(actor, "BATCH_CREATE", "batch", &batch.ID,
		fmt.Sprintf("Created batch %s (material=%s)", batch.Code, material.Code), models.JSONMap{"skipped": skipped})
	return full, skipped, nil
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
	batch, err := s.Get(batchID)
	if err != nil {
		return nil, err
	}

	affectedItems := map[uint]bool{}
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		items, err := txRepo.Batch.BatchItemsForBatch(batch.ID)
		if err != nil {
			return err
		}
		for i := range items {
			bi := &items[i]
			if bi.Status == newStatus {
				continue
			}
			old := string(bi.Status)
			bi.Status = newStatus
			if err := txRepo.Batch.UpdateBatchItem(bi); err != nil {
				return err
			}
			_ = recordStatus(txRepo, models.EntityBatchItem, bi.ID, old, string(newStatus), actor, in.Note)
			affectedItems[bi.OrderItemID] = true
		}
		oldBatch := string(batch.Status)
		batch.Status = newStatus
		if err := txRepo.Batch.Update(batch); err != nil {
			return err
		}
		_ = recordStatus(txRepo, models.EntityBatch, batch.ID, oldBatch, string(newStatus), actor, in.Note)
		return nil
	})
	if err != nil {
		return nil, apperr.Internal("could not update batch status").Wrap(err)
	}

	for itemID := range affectedItems {
		_, _ = recomputeOrderItemStatus(s.repo, itemID, actor)
	}

	s.audit.Log(actor, "BATCH_STATUS_UPDATE", "batch", &batch.ID,
		fmt.Sprintf("Batch %s -> %s", batch.Code, newStatus), nil)
	return s.Get(batch.ID)
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
