package services

import (
	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/repositories"
)

// AdminService holds destructive maintenance operations. Access is gated to
// OWNER (and the ALLOW_DATA_RESET flag) at the route layer.
type AdminService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// ResetResult reports what a data reset cleared.
type ResetResult struct {
	Scope   string   `json:"scope"`
	Cleared []string `json:"cleared"`
}

// ResetTransactional wipes all order/production data (orders, items, batches,
// packages, handoffs, QC, import jobs) so the catalog can be re-imported from a
// clean slate. Master data (materials/SKUs/mappings/sellers) and users are kept.
func (s *AdminService) ResetTransactional(actor Actor) (*ResetResult, error) {
	cleared, err := s.repo.Admin.ResetTransactional()
	if err != nil {
		return nil, apperr.Internal("could not reset data").Wrap(err)
	}
	// audit_logs is not truncated, so this entry survives the reset as a record
	// of who wiped the data.
	s.audit.Log(actor, "DATA_RESET", "system", nil,
		"Reset transactional data (orders/production)",
		map[string]interface{}{"scope": "transactional", "cleared": cleared})
	return &ResetResult{Scope: "transactional", Cleared: cleared}, nil
}
