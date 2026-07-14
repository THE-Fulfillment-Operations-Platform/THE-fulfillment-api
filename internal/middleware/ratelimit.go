package middleware

import (
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
)

// RateLimit is a small in-memory fixed-window limiter keyed by client IP. It
// exists to blunt brute-force / credential-stuffing on sensitive endpoints
// (notably login): after `max` requests within `window`, further requests from
// that IP get 429 until the window rolls off.
//
// Scope & limits (documented on purpose): the counter is per-process and
// in-memory, so it does NOT coordinate across multiple instances or survive a
// restart. For the single-instance MVP that is sufficient; a multi-instance
// deployment should move this to a shared store (e.g. Redis). It keys on
// ClientIP() — behind a proxy, ensure the real client IP is forwarded.
func RateLimit(max int, window time.Duration) gin.HandlerFunc {
	rl := newRateLimiter(max, window)
	return func(c *gin.Context) {
		if !rl.allow(c.ClientIP(), time.Now()) {
			response.AbortTooManyRequests(c, "Quá nhiều yêu cầu. Vui lòng thử lại sau ít phút.")
			return
		}
		c.Next()
	}
}

type rateLimiter struct {
	mu        sync.Mutex
	hits      map[string][]time.Time
	max       int
	window    time.Duration
	lastSweep time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{hits: make(map[string][]time.Time), max: max, window: window}
}

// allow records an attempt for key at now and reports whether it is within the
// limit. Timestamps older than the window are dropped; idle keys are swept
// periodically so the map cannot grow without bound.
func (r *rateLimiter) allow(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := now.Add(-r.window)
	r.sweep(cutoff, now)

	recent := r.hits[key][:0]
	for _, t := range r.hits[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= r.max {
		r.hits[key] = recent
		return false
	}
	r.hits[key] = append(recent, now)
	return true
}

// sweep drops keys whose most recent hit is older than the window. Runs at most
// once per window to keep allow() cheap.
func (r *rateLimiter) sweep(cutoff, now time.Time) {
	if now.Sub(r.lastSweep) < r.window {
		return
	}
	r.lastSweep = now
	for k, times := range r.hits {
		if len(times) == 0 || times[len(times)-1].Before(cutoff) {
			delete(r.hits, k)
		}
	}
}
