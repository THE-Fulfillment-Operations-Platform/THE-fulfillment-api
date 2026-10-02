package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/response"
)

const ctxAPIPrincipal = "api_principal"

// APIKeyResolver turns the key a request presented into the seller it acts for
// (see services.APIKeyService). Its errors are answered to the caller as-is.
type APIKeyResolver interface {
	ResolveAPIKey(raw string) (*models.APIPrincipal, error)
}

// APIKeyAuth guards the open API (/api/open/v1). The caller is a program, so
// there is no login: every request carries the seller's API key, either as
// "Authorization: Bearer <key>" or as "X-API-Key: <key>".
func APIKeyAuth(resolver APIKeyResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader("X-API-Key"))
		if raw == "" {
			if parts := strings.SplitN(c.GetHeader("Authorization"), " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				raw = strings.TrimSpace(parts[1])
			}
		}
		if raw == "" {
			response.Fail(c, apperr.New(http.StatusUnauthorized, "API_KEY_MISSING",
				"Thiếu API key — gửi header 'Authorization: Bearer <key>' hoặc 'X-API-Key: <key>'"))
			c.Abort()
			return
		}
		p, err := resolver.ResolveAPIKey(raw)
		if err != nil {
			response.Fail(c, err)
			c.Abort()
			return
		}
		c.Set(ctxAPIPrincipal, p)
		c.Next()
	}
}

// CurrentAPIPrincipal returns the key + seller APIKeyAuth resolved, or nil.
func CurrentAPIPrincipal(c *gin.Context) *models.APIPrincipal {
	v, ok := c.Get(ctxAPIPrincipal)
	if !ok {
		return nil
	}
	p, _ := v.(*models.APIPrincipal)
	return p
}

// RateLimitAPIKey limits each API key to max requests per window. It must run
// after APIKeyAuth. Keyed by the key rather than the IP: one seller's systems may
// share an address with another's, and a seller's own two systems should not be
// able to starve each other by accident — each has its own key.
//
// Same in-memory, per-process limiter as RateLimit, with the same caveat about
// multiple instances.
func RateLimitAPIKey(max int, window time.Duration) gin.HandlerFunc {
	rl := newRateLimiter(max, window)
	return func(c *gin.Context) {
		p := CurrentAPIPrincipal(c)
		if p == nil {
			response.AbortUnauthorized(c, "Authentication required")
			return
		}
		if !rl.allow("key:"+strconv.FormatUint(uint64(p.KeyID), 10), time.Now()) {
			// Tell a well-behaved client how long to back off for.
			c.Header("Retry-After", strconv.Itoa(int(window.Seconds())))
			response.AbortTooManyRequests(c, "Quá nhiều yêu cầu cho API key này. Thử lại sau ít phút.")
			return
		}
		c.Next()
	}
}
