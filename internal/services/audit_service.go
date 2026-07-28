package services

import (
	"context"
	"log"
	"sync"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// AuditService writes coarse audit entries for important actions. Audit failures
// never block the primary action; they are logged and swallowed.
//
// Entries are queued and flushed by a background writer instead of being written
// inline. An audit row is a record of what happened, not part of what happened:
// against a remote database its INSERT costs a full round-trip (~400ms measured
// to a Supabase pooler in another region) that every mutating endpoint was paying
// AFTER its real work was already done. Queuing hands that time back to the user
// while still recording everything.
//
// The queue is bounded and falls back to an inline write when full, so a burst
// can slow down but never silently drops history. Drain flushes on shutdown.
type AuditService struct {
	repo *repositories.Repositories
	// async is opt-in via NewAuditService. A zero-value AuditService (what tests
	// build) writes inline: the background writer would take a SECOND pooled
	// connection, and an in-memory SQLite test database is per-connection — the
	// writer would land in an empty one and the test would lose its tables.
	async bool
	queue chan *models.AuditLog
	wg    sync.WaitGroup
	once  sync.Once
}

// NewAuditService builds the audit service used at runtime, with the background
// writer enabled. Call Drain on shutdown to flush what is still queued.
func NewAuditService(repo *repositories.Repositories) *AuditService {
	return &AuditService{repo: repo, async: true}
}

// auditQueueSize is deliberately generous: a bulk action can emit a burst of
// entries, and falling back to the inline path mid-burst would negate the point.
const auditQueueSize = 512

// startWriter spins the background writer on first use, so a directly
// constructed AuditService (as tests do) works without extra wiring.
func (s *AuditService) startWriter() {
	s.once.Do(func() {
		s.queue = make(chan *models.AuditLog, auditQueueSize)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for entry := range s.queue {
				s.write(entry)
			}
		}()
	})
}

func (s *AuditService) write(entry *models.AuditLog) {
	if err := s.repo.Audit.Create(entry); err != nil {
		log.Printf("audit: failed to record %q: %v", entry.Action, err)
	}
}

// Log records an audit entry. Returns as soon as the entry is queued.
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
	if !s.async {
		s.write(entry)
		return
	}
	s.startWriter()
	select {
	case s.queue <- entry:
	default:
		s.write(entry) // queue full — write inline rather than lose the entry
	}
}

// Drain stops accepting entries and waits for the queued ones to be written, or
// until ctx is done. Called on shutdown so a restart can't lose recent history.
func (s *AuditService) Drain(ctx context.Context) {
	if s.queue == nil {
		return
	}
	close(s.queue)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("audit: shutdown timed out with entries still queued")
	}
}

// List returns audit entries (admin/owner only at the handler layer).
func (s *AuditService) List(page repositories.Page) ([]models.AuditLog, int64, error) {
	return s.repo.Audit.List(page.Normalize())
}
