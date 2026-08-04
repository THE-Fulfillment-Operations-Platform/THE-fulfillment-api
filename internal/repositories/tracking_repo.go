package repositories

import (
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"the-fulfillment/backend/internal/models"
)

// TrackingRepository stores the shipment timeline mirrored from the tracking
// provider, and answers "which orders should be polled next".
type TrackingRepository struct{ db *gorm.DB }

// SaveEvents inserts scans, ignoring any already stored. The provider returns
// the WHOLE timeline on every poll, so the fingerprint unique index plus
// DO NOTHING is what makes a re-sync idempotent without a read-compare-write
// round-trip per event.
func (r *TrackingRepository) SaveEvents(events []models.OrderTrackingEvent) error {
	if len(events) == 0 {
		return nil
	}
	return r.db.
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "fingerprint"}}, DoNothing: true}).
		CreateInBatches(&events, 100).Error
}

// EventsByOrder returns the timeline of ONE parcel on an order, newest scan
// first.
//
// The tracking number is part of the query, not an afterthought: a journey
// belongs to a PARCEL, not to an order. When a tracking number is corrected (a
// typo) or genuinely replaced (parcel returned and re-sent, carrier re-labelled
// it), the order ends up owning the scans of more than one parcel. Reading them
// all back would splice two shipments into one nonsensical history.
//
// Scans of previous numbers stay in the table on purpose — a failed first
// delivery is the evidence you need when claiming against the carrier — they
// just are not shown next to the parcel currently in flight.
//
// Events are ordered by the parsed timestamp when available; rows whose provider
// date could not be parsed fall back to insertion order, which preserves the
// provider's own newest-first sequence.
func (r *TrackingRepository) EventsByOrder(orderID uint, trackingNumber string) ([]models.OrderTrackingEvent, error) {
	trackingNumber = strings.TrimSpace(trackingNumber)
	if trackingNumber == "" {
		// No parcel on the order ⇒ nothing to show, even if history exists.
		return []models.OrderTrackingEvent{}, nil
	}
	var rows []models.OrderTrackingEvent
	err := r.db.
		Where("order_id = ? AND tracking_number = ?", orderID, trackingNumber).
		Order("event_at desc nulls last").
		Order("id desc").
		Find(&rows).Error
	return rows, err
}

// DeleteEventsForNumber drops the stored timeline of one tracking number on an
// order. Used when an order's tracking number is corrected: the scans of the
// wrong parcel must not linger on the order's journey.
func (r *TrackingRepository) DeleteEventsForNumber(orderID uint, trackingNumber string) error {
	return r.db.Unscoped().
		Where("order_id = ? AND tracking_number = ?", orderID, trackingNumber).
		Delete(&models.OrderTrackingEvent{}).Error
}

// DueForSync returns the orders whose parcel should be re-checked, least
// recently synced first, so a fixed per-run budget rotates fairly across every
// live shipment instead of starving the tail of the list.
//
// Only handed-over orders qualify. Production and shipping are two separate
// halves of an order's life: until someone presses "bàn giao cho THE" the parcel
// does not physically exist for the carrier, so asking the provider about it can
// only burn rate limit and write a misleading "Not Found" onto the order.
//
// Terminal parcels (delivered/expired/cancelled) are excluded too: the carrier
// will never move them again.
func (r *TrackingRepository) DueForSync(limit int, staleBefore time.Time) ([]models.Order, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []models.Order
	err := r.db.
		Where("seller_status IN ?", models.HandedOverStatuses).
		Where("tracking_number <> ''").
		Where("tracking_status NOT IN ?", []string{
			string(models.TrackingDelivered),
			string(models.TrackingExpired),
			string(models.TrackingCancelled),
		}).
		Where("tracking_synced_at IS NULL OR tracking_synced_at < ?", staleBefore).
		Order("tracking_synced_at asc nulls first").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

// SellerStatusLagging returns orders whose seller status has fallen behind what
// their own tracking status already says.
//
// This exists because DueForSync deliberately never re-reads a delivered parcel:
// once tracking_status is terminal the order drops out of the sweep for good. An
// order that reached DELIVERED while nothing advanced its seller status is then
// frozen — the seller sees "Đã bàn giao" next to a tracking badge reading "Đã
// giao", and no future pass will ever reconcile them. This query finds exactly
// those rows, and costs no provider quota to fix: the answer is already in our
// own columns.
//
// The pairs below mirror sellerStatusFor in the service. Listing only the
// combinations that WILL advance keeps already-consistent orders out of the
// result, so a fixed budget is never spent re-reading rows that do nothing.
func (r *TrackingRepository) SellerStatusLagging(limit int) ([]models.Order, error) {
	if limit <= 0 {
		limit = 100
	}
	moving := []string{
		string(models.TrackingInTransit),
		string(models.TrackingOutForDelivery),
		string(models.TrackingPickUp),
	}
	var rows []models.Order
	err := r.db.
		Where("review_status <> ?", string(models.ReviewCancelled)).
		Where(
			r.db.Where("tracking_status = ? AND seller_status IN ?",
				string(models.TrackingDelivered),
				[]string{string(models.SellerStatusHandedOff), string(models.SellerStatusShipped)},
			).Or("tracking_status IN ? AND seller_status = ?",
				moving, string(models.SellerStatusHandedOff),
			),
		).
		Order("id asc").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

// AwaitingTrackingNumber returns handed-over orders that still have no tracking
// number, so the reverse lookup can ask the provider whether a parcel was
// registered under their store order id.
//
// The handed-over condition matters as much here as in DueForSync: an order
// still in production has no parcel to find, and asking about every such order
// on every pass would spend the whole budget on orders that cannot match.
//
// resolveAfter bounds the work by age — an order that has been sitting without a
// tracking number for months is not going to grow one, and re-asking about it
// forever would crowd out fresh orders.
func (r *TrackingRepository) AwaitingTrackingNumber(limit int, resolveAfter time.Time) ([]models.Order, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []models.Order
	err := r.db.
		Where("seller_status IN ?", models.HandedOverStatuses).
		Where("tracking_number = ''").
		Where("created_at >= ?", resolveAfter).
		Where("cancellation_status <> ?", string(models.CancellationApproved)).
		Where("tracking_synced_at IS NULL OR tracking_synced_at < ?", resolveAfter).
		Order("tracking_synced_at asc nulls first").
		Order("id desc").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}
