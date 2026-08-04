package maintenance

import (
	"context"
	"log"
	"time"

	"the-fulfillment/backend/internal/services"
)

// trackingBootDelay lets the server finish booting before the first provider
// pass, so a startup already busy with migrations is not also waiting on a
// third-party API.
const trackingBootDelay = 30 * time.Second

// TrackingScheduler periodically refreshes shipment status and journey from the
// tracking provider. Like PurgeScheduler it is a plain ticker loop — no external
// cron — and stops cleanly when its context is cancelled.
type TrackingScheduler struct {
	sync      *services.TrackingSyncService
	interval  time.Duration
	batchSize int
}

// NewTrackingScheduler wires the job.
func NewTrackingScheduler(sync *services.TrackingSyncService, interval time.Duration, batchSize int) *TrackingScheduler {
	return &TrackingScheduler{sync: sync, interval: interval, batchSize: batchSize}
}

// Start launches the loop in its own goroutine: one pass shortly after boot,
// then once per interval. Non-blocking.
func (s *TrackingScheduler) Start(ctx context.Context) {
	if s == nil || s.sync == nil || !s.sync.Enabled() {
		return
	}
	if s.interval <= 0 {
		s.interval = 20 * time.Minute
	}
	if s.batchSize <= 0 {
		s.batchSize = 120
	}
	go func() {
		timer := time.NewTimer(trackingBootDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			start := time.Now()
			stats := s.sync.RunOnce(ctx, s.batchSize)
			// Only speak up when the pass did something. A quiet factory would
			// otherwise write a line every interval, all night, saying nothing.
			// Reconciled counts: a pass that touched no parcel but pulled an order's
			// status back in line did real work, and staying silent about it would
			// leave the one status change nobody clicked with no trace in the log.
			if stats.Resolved+stats.Synced+stats.Failed+stats.Reconciled > 0 {
				log.Printf("tracking24h: pass done in %s (resolved=%d synced=%d changed=%d failed=%d skipped=%d reconciled=%d)",
					time.Since(start).Round(time.Second),
					stats.Resolved, stats.Synced, stats.Changed, stats.Failed, stats.Skipped, stats.Reconciled)
			}
			timer.Reset(s.interval)
		}
	}()
}
