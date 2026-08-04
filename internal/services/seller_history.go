package services

import (
	"strings"
	"time"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// ---------------------------------------------------------------------------
// Seller-facing order history.
//
// status_histories already records every order-level transition (review decision,
// production hand-off, cancellation, edit) — it was just never readable outside
// the factory. A seller answering "where is my order and what happened to it" had
// only the current badge plus the carrier's scans, so anything that happened
// BEFORE the parcel existed was invisible to them.
//
// Two boundaries this crosses carefully:
//
//  1. Entity scope. Only EntityOrder rows. ORDER_ITEM / BATCH / BATCH_ITEM rows
//     carry the internal print/cut/QC pipeline that SellerStatus deliberately
//     hides, and joining them in would undo that in one line.
//  2. Wording. Notes are free text written by internal code and can name the
//     transport partner ("handed off to <carrier>"), so every note goes through
//     redactPartner — the same filter the tracking journey already uses.
// ---------------------------------------------------------------------------

// SellerHistoryEvent is one entry in the seller-visible order timeline.
type SellerHistoryEvent struct {
	At time.Time `json:"at"`
	// Kind tells the UI which label map to read FromStatus/ToStatus with:
	// "review" for the intake decision statuses, "production" for the seller-facing
	// production phases. Resolved here because the backend owns the enums; sending
	// bare strings would make the UI guess which vocabulary a row belongs to.
	Kind       string `json:"kind"`
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
	// Actor is a ROLE, never a name or email. The seller is owed "who side did
	// this", not the identity of a factory employee. One of: seller / ops / system.
	Actor string `json:"actor"`
	Note  string `json:"note,omitempty"`
}

var sellerReviewStatuses = map[string]bool{
	string(models.ReviewPending):   true,
	string(models.ReviewNeedsFix):  true,
	string(models.ReviewApproved):  true,
	string(models.ReviewRejected):  true,
	string(models.ReviewCancelled): true,
}

// historyKind picks the vocabulary a row was written in. Order-level history is
// written by two subsystems: review decisions store ReviewStatus, packing and
// shipping store SellerStatus. ToStatus decides, falling back to FromStatus for
// the odd row that only records where it came from.
func historyKind(from, to string) string {
	if sellerReviewStatuses[to] || (to == "" && sellerReviewStatuses[from]) {
		return "review"
	}
	return "production"
}

// SellerOrderHistory returns the sanitised order timeline, oldest first.
func (s *OrderService) SellerOrderHistory(sellerID, orderID uint) ([]SellerHistoryEvent, error) {
	owner, found, err := s.repo.Order.OwnerSellerID(orderID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, apperr.NotFound("Order not found")
	}
	if owner != sellerID {
		return nil, apperr.Forbidden("This order does not belong to your seller account")
	}

	rows, err := s.repo.Status.ListForEntity(models.EntityOrder, orderID)
	if err != nil {
		return nil, err
	}

	// Resolve every distinct actor in one query rather than per row.
	actorIDs := make([]uint, 0, len(rows))
	seen := map[uint]bool{}
	for _, r := range rows {
		if r.ChangedByID != nil && !seen[*r.ChangedByID] {
			seen[*r.ChangedByID] = true
			actorIDs = append(actorIDs, *r.ChangedByID)
		}
	}
	users, err := s.repo.User.FindByIDs(actorIDs)
	if err != nil {
		return nil, err
	}
	// A SELLER-role user of THIS seller account reads as "you"; anyone else in the
	// factory is simply "ops". Comparing seller_id (not role alone) matters: a
	// seller must not see another seller's staff described as their own.
	actorOf := make(map[uint]string, len(users))
	for _, u := range users {
		if u.Role == models.RoleSeller && u.SellerID != nil && *u.SellerID == sellerID {
			actorOf[u.ID] = "seller"
		} else {
			actorOf[u.ID] = "ops"
		}
	}

	out := make([]SellerHistoryEvent, 0, len(rows))
	for _, r := range rows {
		actor := "system"
		if r.ChangedByID != nil {
			if a, ok := actorOf[*r.ChangedByID]; ok {
				actor = a
			} else {
				// User row deleted since. Still an internal action, not the system's.
				actor = "ops"
			}
		}
		out = append(out, SellerHistoryEvent{
			At:         r.CreatedAt,
			Kind:       historyKind(r.FromStatus, r.ToStatus),
			FromStatus: r.FromStatus,
			ToStatus:   r.ToStatus,
			Actor:      actor,
			Note:       strings.TrimSpace(redactPartner(r.Note)),
		})
	}
	return out, nil
}
