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
type UpdateTrackingInput struct {
	TrackingNumber *string `json:"tracking_number"`
	TrackingStatus *string `json:"tracking_status"`
	Carrier        *string `json:"tracking_carrier"`
	TrackingURL    *string `json:"tracking_url"`
}

// trackingRoles may edit tracking: internal managers plus the packing/shipping
// stations that physically hand orders to a carrier.
func canEditTracking(role models.Role) bool {
	switch role {
	case models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RolePacking, models.RoleShipping:
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
	if in.TrackingNumber != nil {
		v := strings.TrimSpace(*in.TrackingNumber)
		fields["tracking_number"] = v
		changes["tracking_number"] = []string{order.TrackingNumber, v}
	}
	if in.Carrier != nil {
		v := strings.TrimSpace(*in.Carrier)
		fields["tracking_carrier"] = v
		changes["tracking_carrier"] = []string{order.TrackingCarrier, v}
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
	return s.GetOrder(order.ID)
}
