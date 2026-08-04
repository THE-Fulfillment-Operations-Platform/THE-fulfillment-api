package services

import (
	"strings"
	"time"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// TrackingProvider is the seam for a future automatic tracking integration
// (e.g. 17TRACK). Today tracking is entered manually via UpdateTracking; when a
// provider is wired in, a background sync job can implement this interface and
// call OrderService.ApplyTrackingSync without changing any callers or the schema.
// Keeping it an interface here (rather than calling a provider inline on every
// render) is what stops per-row provider calls and respects rate limits.
type TrackingProvider interface {
	// Fetch returns the current status for a tracking number, or ErrUnsupported when
	// the provider is a no-op stub.
	Fetch(carrier, trackingNumber string) (TrackingSnapshot, error)
}

// TrackingSnapshot is a normalized provider response.
type TrackingSnapshot struct {
	Status    models.TrackingStatus
	Carrier   string
	URL       string
	UpdatedAt time.Time
	Raw       string // provider payload, for audit
}

// UpdateTrackingInput sets an order's tracking fields manually.
//
// No carrier field: THE is the shipping company on every order, so there is
// nothing to type in. A tracking_carrier in the request body is ignored rather
// than stored.
type UpdateTrackingInput struct {
	TrackingNumber *string `json:"tracking_number"`
	TrackingStatus *string `json:"tracking_status"`
	TrackingURL    *string `json:"tracking_url"`
}

// trackingRoles may edit tracking: internal managers, the packing/shipping
// stations that dispatch parcels, and customer support — CS is who receives the
// tracking number and matches it to a store order.
func canEditTracking(role models.Role) bool {
	switch role {
	case models.RoleOwner, models.RoleAdmin, models.RoleOps,
		models.RolePacking, models.RoleShipping, models.RoleCS:
		return true
	}
	return false
}

// UpdateTracking sets tracking number/status/carrier/url on an order. Status is
// validated against the TrackingStatus enum; a URL, if given, must be http(s).
// It records LastTrackingUpdate and writes an audit entry. This is the manual
// entry path; a provider sync would funnel through ApplyTrackingSync instead.
func (s *OrderService) UpdateTracking(actor Actor, id uint, in UpdateTrackingInput) (*models.Order, error) {
	if !canEditTracking(actor.Role) {
		return nil, apperr.Forbidden("Bạn không có quyền cập nhật tracking")
	}
	order, err := s.GetOrder(id)
	if err != nil {
		return nil, err
	}

	fields := map[string]interface{}{}
	changes := models.JSONMap{}
	// previousNumber drives everything that has to happen when a parcel is
	// REPLACED rather than merely filled in: the carrier state on the order
	// describes the old parcel and must not survive, and the old number is still
	// tagged with this store order id on the provider.
	previousNumber := strings.TrimSpace(order.TrackingNumber)
	numberChanged := false
	if in.TrackingNumber != nil {
		v := strings.TrimSpace(*in.TrackingNumber)
		fields["tracking_number"] = v
		changes["tracking_number"] = []string{order.TrackingNumber, v}
		numberChanged = v != previousNumber

		// A different parcel means the mirrored carrier state belongs to something
		// that is no longer being shipped. Leaving it would show the OLD parcel's
		// "Delivered"/"In Transit" against the new number until the first sync
		// lands — the worst kind of wrong, because it looks authoritative.
		if numberChanged {
			fields["tracking_status"] = models.TrackingPending
			if v == "" {
				fields["tracking_status"] = models.TrackingNone
			}
			fields["tracking_detail"] = ""
			fields["tracking_location"] = ""
			fields["tracking_raw_status"] = ""
			fields["tracking_delivered_at"] = nil
			fields["tracking_synced_at"] = nil
			fields["tracking_sync_error"] = ""
			changes["tracking_status"] = []string{string(order.TrackingStatus), string(models.TrackingPending)}
		}
	}
	if in.TrackingURL != nil {
		v := strings.TrimSpace(*in.TrackingURL)
		if v != "" && !isValidHTTPURL(v) {
			return nil, apperr.BadRequest("Tracking URL không hợp lệ (phải là http/https)")
		}
		fields["tracking_url"] = v
		changes["tracking_url"] = []string{order.TrackingURL, v}
	}
	if in.TrackingStatus != nil {
		st := models.TrackingStatus(strings.ToUpper(strings.TrimSpace(*in.TrackingStatus)))
		if st == "" {
			st = models.TrackingNone
		}
		if !st.Valid() {
			return nil, apperr.BadRequest("Trạng thái tracking không hợp lệ")
		}
		fields["tracking_status"] = st
		changes["tracking_status"] = []string{string(order.TrackingStatus), string(st)}
	}
	if len(fields) == 0 {
		return order, nil
	}
	now := time.Now()
	fields["tracking_updated_at"] = now
	if err := s.repo.Order.UpdateTracking(order.ID, fields); err != nil {
		return nil, apperr.Internal("could not update tracking").Wrap(err)
	}
	s.audit.Log(actor, "ORDER_TRACKING_UPDATE", "order", &order.ID, "Updated tracking for "+order.InternalCode, changes)

	updated, err := s.GetOrder(order.ID)
	if err != nil {
		return nil, err
	}
	// A manually entered number is registered with the provider (tagged with the
	// store order id) so its journey starts being collected. Only when the number
	// actually changed: re-registering on every carrier/URL edit would be noise.
	if numberChanged {
		// Release the previous parcel first. It still carries this order's tag on
		// the provider, and two parcels answering to one store order id is exactly
		// the ambiguity the reverse lookup refuses to guess through.
		s.tracking.ReleaseNumberAsync(order, previousNumber)
		s.tracking.RegisterOrderAsync(updated)
	}
	return updated, nil
}
