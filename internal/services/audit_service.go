package services

import (
	"log"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// AuditService writes coarse audit entries for important actions. Audit failures
// never block the primary action; they are logged and swallowed.
type AuditService struct {
	repo *repositories.Repositories
}

// Log records an audit entry.
func (s *AuditService) Log(actor Actor, action, entityType string, entityID *uint, summary string, metadata interface{}) {
	meta, _ := models.ToJSONB(metadata)
	entry := &models.AuditLog{
		ActorID:    actor.IDPtr(),
		ActorEmail: actor.Email,
		Action:     action,
		EntityType: entityType,
		EntityID:   entityID,
		Summary:    summary,
		Metadata:   meta,
		IP:         actor.IP,
	}
	if err := s.repo.Audit.Create(entry); err != nil {
		log.Printf("audit: failed to record %q: %v", action, err)
	}
}

// List returns audit entries (admin/owner only at the handler layer).
func (s *AuditService) List(page repositories.Page) ([]models.AuditLog, int64, error) {
	return s.repo.Audit.List(page.Normalize())
}
