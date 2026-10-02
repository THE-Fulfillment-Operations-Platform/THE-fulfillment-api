package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// fakeResolver knows exactly two keys, one per seller.
type fakeResolver struct{ calls int }

func (f *fakeResolver) ResolveAPIKey(raw string) (*models.APIPrincipal, error) {
	f.calls++
	switch raw {
	case "key-a":
		return &models.APIPrincipal{KeyID: 1, SellerID: 10}, nil
	case "key-b":
		return &models.APIPrincipal{KeyID: 2, SellerID: 20}, nil
	}
	return nil, apperr.New(http.StatusUnauthorized, "API_KEY_INVALID", "nope")
}

func keyRouter(resolver APIKeyResolver, max int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(APIKeyAuth(resolver), RateLimitAPIKey(max, time.Minute))
	r.GET("/who", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"seller": CurrentAPIPrincipal(c).SellerID})
	})
	return r
}

func call(r http.Handler, header, value string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/who", nil)
	if header != "" {
		req.Header.Set(header, value)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// The key is read from either header; nothing reaches the handler without one,
// and a request with no key at all never costs a lookup.
func TestAPIKeyAuth(t *testing.T) {
	resolver := &fakeResolver{}
	r := keyRouter(resolver, 100)

	if w := call(r, "", ""); w.Code != http.StatusUnauthorized || resolver.calls != 0 {
		t.Errorf("no key: status %d, lookups %d", w.Code, resolver.calls)
	}
	if w := call(r, "Authorization", "Basic key-a"); w.Code != http.StatusUnauthorized || resolver.calls != 0 {
		t.Errorf("non-bearer scheme: status %d, lookups %d", w.Code, resolver.calls)
	}
	if w := call(r, "Authorization", "Bearer wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: status %d", w.Code)
	}
	for header, value := range map[string]string{"Authorization": "bearer  key-a ", "X-API-Key": " key-a "} {
		if w := call(r, header, value); w.Code != http.StatusOK || w.Body.String() != `{"seller":10}` {
			t.Errorf("%s: status %d body %s", header, w.Code, w.Body.String())
		}
	}
}

// Each key has its own budget: one seller's burst must not lock out another,
// even from the same address.
func TestRateLimitAPIKey_IsPerKey(t *testing.T) {
	r := keyRouter(&fakeResolver{}, 2)

	for i := 0; i < 2; i++ {
		if w := call(r, "X-API-Key", "key-a"); w.Code != http.StatusOK {
			t.Fatalf("request %d for key-a: %d", i+1, w.Code)
		}
	}
	w := call(r, "X-API-Key", "key-a")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" {
		t.Errorf("3rd request for key-a: status %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := call(r, "X-API-Key", "key-b"); w.Code != http.StatusOK {
		t.Errorf("key-b should have its own budget, got %d", w.Code)
	}
}
