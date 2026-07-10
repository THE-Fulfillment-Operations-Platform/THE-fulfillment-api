// Package maintenance holds background housekeeping jobs. Currently it runs a
// periodic purge of long-soft-deleted rows so GORM's deleted_at markers don't
// pile up in the database forever.
package maintenance

import (
	"context"
	"log"
	"time"

	"the-fulfillment/backend/internal/repositories"
)

// bootDelay lets startup (connect + migrate + seed) settle before the first
// purge, so the job never competes with boot work.
const bootDelay = time.Minute

// PurgeScheduler periodically hard-deletes rows soft-deleted more than
// RetentionDays ago. It is a plain time.Ticker loop — no external cron
// dependency — and stops cleanly when its context is cancelled.
type PurgeScheduler struct {
	admin         *repositories.AdminRepository
	retentionDays int
	interval      time.Duration
}

// NewPurgeScheduler wires the job. interval is how often the purge runs (e.g. 24h).
func NewPurgeScheduler(admin *repositories.AdminRepository, retentionDays int, interval time.Duration) *PurgeScheduler {
	return &PurgeScheduler{admin: admin, retentionDays: retentionDays, interval: interval}
}

// Start launches the job in its own goroutine: one run shortly after boot, then
// once per interval, until ctx is cancelled. Non-blocking.
func (s *PurgeScheduler) Start(ctx context.Context) {
	if s.interval <= 0 {
		s.interval = 24 * time.Hour
	}
	go func() {
		timer := time.NewTimer(bootDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.runOnce(ctx)
		}

		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runOnce(ctx)
			}
		}
	}()
}

func (s *PurgeScheduler) runOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	// A panic in this background goroutine would crash the whole process; contain it.
	defer func() {
		if p := recover(); p != nil {
			log.Printf("maintenance: purge panicked: %v", p)
		}
	}()
	start := time.Now()
	if err := s.admin.PurgeSoftDeleted(s.retentionDays); err != nil {
		log.Printf("maintenance: purge failed: %v", err)
		return
	}
	log.Printf("maintenance: purge ok (retention=%dd, took=%s)", s.retentionDays, time.Since(start).Round(time.Millisecond))
}
