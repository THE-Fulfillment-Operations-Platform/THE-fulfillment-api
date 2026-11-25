package services

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Keys of the timeline steps. The web maps them onto its stepper.
const (
	StepImport   = "import"
	StepApproved = "approved"
	StepBatched  = "batched"
	StepPrinted  = "printed"
	StepCut      = "cut"
	StepQCPassed = "qc_passed"
	StepHandoff  = "handed_off"
	StepCreated  = "created" // batch only
)

// TimelineStep is when one stage finished and who did it. At is nil while the
// stage is not done yet (or the history that would tell is missing).
type TimelineStep struct {
	Key    string     `json:"key"`
	At     *time.Time `json:"at"`
	ByID   *uint      `json:"by_id,omitempty"`
	ByName string     `json:"by_name,omitempty"`
}

// Timeline is the answer of GET /api/orders/:id/timeline and
// GET /api/batches/:id/timeline.
type Timeline struct {
	EntityID uint           `json:"entity_id"`
	Steps    []TimelineStep `json:"steps"`
}

// firstReached returns the first history row (oldest) whose to-status is the
// target or further along the print -> cut -> QC line. "Further along" matters:
// a batch can jump PENDING -> CUT, and the print stage was then passed at that
// same moment. Rank 0 (PENDING) is never a "reached" stage for these steps.
func firstReached(rows []models.StatusHistory, target models.InternalStatus) *models.StatusHistory {
	for i := range rows {
		if models.InternalStatus(rows[i].ToStatus).Rank() >= target.Rank() {
			return &rows[i]
		}
	}
	return nil
}

func historyByEntity(rows []models.StatusHistory) map[uint][]models.StatusHistory {
	out := map[uint][]models.StatusHistory{}
	for _, r := range rows {
		out[r.EntityID] = append(out[r.EntityID], r)
	}
	return out
}

// latestStep folds per-item moments into one order-level moment: the order has
// finished a stage when its LAST live item did. ok=false when any item has not.
type stepAcc struct {
	at   time.Time
	by   *uint
	done bool
	miss bool
}

func (a *stepAcc) add(at *time.Time, by *uint) {
	if at == nil {
		a.miss = true
		return
	}
	if !a.done || at.After(a.at) {
		a.at, a.by = *at, by
	}
	a.done = true
}

func (a *stepAcc) step(key string) TimelineStep {
	if a.miss || !a.done {
		return TimelineStep{Key: key}
	}
	at := a.at
	return TimelineStep{Key: key, At: &at, ByID: a.by}
}

// OrderTimeline answers "when did each stage of this order finish, and who".
//
//   - import: the order's created_at.
//   - approved: first ORDER -> APPROVED.
//   - batched: the moment the LAST live item first entered a batch.
//   - printed / cut / qc_passed: per live item the FIRST time it reached (or
//     passed) the status, then the latest of those across the items — an order
//     is through a stage when its last item is. A QC fail that sends a product
//     back does not move the time: it is the first time that counts.
//   - handed_off: first ORDER -> HANDED_OFF, else orders.handed_over_at.
//
// Cancelled lines do not count. A stage with an item that never got there stays
// empty. Four indexed reads, whatever the order's size.
func (s *OrderService) OrderTimeline(id uint) (*Timeline, error) {
	order, err := s.repo.Order.FindBare(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Order not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	all, err := s.repo.OrderItem.ItemsOfOrder(id)
	if err != nil {
		return nil, apperr.Internal("timeline items failed").Wrap(err)
	}
	var itemIDs []uint
	for _, it := range all {
		if !itemCancelled(it.CancellationStatus) {
			itemIDs = append(itemIDs, it.ID)
		}
	}

	orderHist, err := s.repo.Status.ListByEntities(models.EntityOrder, []uint{id})
	if err != nil {
		return nil, apperr.Internal("timeline history failed").Wrap(err)
	}
	itemHist, err := s.repo.Status.ListByEntities(models.EntityOrderItem, itemIDs)
	if err != nil {
		return nil, apperr.Internal("timeline history failed").Wrap(err)
	}
	parts, err := s.repo.Batch.BatchPartsOfItems(itemIDs)
	if err != nil {
		return nil, apperr.Internal("timeline batch parts failed").Wrap(err)
	}
	partIDs := make([]uint, 0, len(parts))
	for _, p := range parts {
		partIDs = append(partIDs, p.ID)
	}
	partHist, err := s.repo.Status.ListByEntities(models.EntityBatchItem, partIDs)
	if err != nil {
		return nil, apperr.Internal("timeline history failed").Wrap(err)
	}

	steps := []TimelineStep{{Key: StepImport, At: ptrTime(order.CreatedAt), ByID: order.CreatedByID}}

	// approved + handed_off from the order's own history.
	var approved, handed *models.StatusHistory
	for i := range orderHist {
		switch orderHist[i].ToStatus {
		case string(models.ReviewApproved):
			if approved == nil {
				approved = &orderHist[i]
			}
		case string(models.SellerStatusHandedOff):
			if handed == nil {
				handed = &orderHist[i]
			}
		}
	}
	steps = append(steps, histStep(StepApproved, approved))

	// batched: per item, the first time it entered any batch.
	firstPart := map[uint]*models.BatchItem{}
	for i := range parts {
		if cur, ok := firstPart[parts[i].OrderItemID]; !ok || parts[i].CreatedAt.Before(cur.CreatedAt) {
			firstPart[parts[i].OrderItemID] = &parts[i]
		}
	}
	partByID := historyByEntity(partHist)
	var batched stepAcc
	for _, itemID := range itemIDs {
		bp := firstPart[itemID]
		if bp == nil {
			batched.add(nil, nil)
			continue
		}
		var by *uint
		if h := partByID[bp.ID]; len(h) > 0 {
			by = h[0].ChangedByID
		}
		at := bp.CreatedAt
		batched.add(&at, by)
	}
	steps = append(steps, batched.step(StepBatched))

	byItem := historyByEntity(itemHist)
	for _, st := range []struct {
		key    string
		status models.InternalStatus
	}{{StepPrinted, models.StatusPrinted}, {StepCut, models.StatusCut}, {StepQCPassed, models.StatusQCPassed}} {
		var acc stepAcc
		for _, itemID := range itemIDs {
			if h := firstReached(byItem[itemID], st.status); h != nil {
				at := h.CreatedAt
				acc.add(&at, h.ChangedByID)
			} else {
				acc.add(nil, nil)
			}
		}
		steps = append(steps, acc.step(st.key))
	}

	switch {
	case handed != nil:
		steps = append(steps, histStep(StepHandoff, handed))
	case order.HandedOverAt != nil:
		steps = append(steps, TimelineStep{Key: StepHandoff, At: order.HandedOverAt})
	default:
		steps = append(steps, TimelineStep{Key: StepHandoff})
	}

	nameTimelineActors(s.repo, steps)
	return &Timeline{EntityID: id, Steps: steps}, nil
}

// BatchTimeline answers "when was this batch created / printed / cut / QC'd, and
// by whom", from the batch's own history: the first time it reached each status.
func (s *BatchService) BatchTimeline(id uint) (*Timeline, error) {
	b, err := s.repo.Batch.FindBare(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Batch not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	hist, err := s.repo.Status.ListByEntities(models.EntityBatch, []uint{id})
	if err != nil {
		return nil, apperr.Internal("timeline history failed").Wrap(err)
	}
	steps := []TimelineStep{{Key: StepCreated, At: ptrTime(b.CreatedAt), ByID: b.CreatedByID}}
	for _, st := range []struct {
		key    string
		status models.InternalStatus
	}{{StepPrinted, models.StatusPrinted}, {StepCut, models.StatusCut}, {StepQCPassed, models.StatusQCPassed}} {
		steps = append(steps, histStep(st.key, firstReached(hist, st.status)))
	}
	nameTimelineActors(s.repo, steps)
	return &Timeline{EntityID: id, Steps: steps}, nil
}

func ptrTime(t time.Time) *time.Time { return &t }

func histStep(key string, h *models.StatusHistory) TimelineStep {
	if h == nil {
		return TimelineStep{Key: key}
	}
	return TimelineStep{Key: key, At: ptrTime(h.CreatedAt), ByID: h.ChangedByID}
}

// nameTimelineActors fills ByName from users, one query for all steps. A user
// who has since been deleted just shows no name (the time is still right).
func nameTimelineActors(repo *repositories.Repositories, steps []TimelineStep) {
	seen := map[uint]bool{}
	var ids []uint
	for _, st := range steps {
		if st.ByID != nil && !seen[*st.ByID] {
			seen[*st.ByID] = true
			ids = append(ids, *st.ByID)
		}
	}
	users, err := repo.User.FindByIDs(ids)
	if err != nil {
		return // names are decoration; the times stand without them
	}
	names := map[uint]string{}
	for _, u := range users {
		names[u.ID] = u.FullName
	}
	for i := range steps {
		if steps[i].ByID != nil {
			steps[i].ByName = names[*steps[i].ByID]
		}
	}
}
