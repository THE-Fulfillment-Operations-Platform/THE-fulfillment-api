package routes

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/database"
	"the-fulfillment/backend/internal/handlers"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/services"
)

// openHarness is the real stack — router, middleware, handlers, services, the
// production migration — over an in-memory database. The open API is a contract
// with other people's code, so it is tested the way they will call it: over HTTP.
type openHarness struct {
	t      *testing.T
	router http.Handler
	db     *gorm.DB
	tokens map[models.Role]string
}

type openResp struct {
	status int
	header http.Header
	body   struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   *struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	raw string
}

func newOpenHarness(t *testing.T) *openHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// One connection: an in-memory sqlite database belongs to its connection, and
	// the audit writer runs on its own goroutine.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	// The production migration, so a model or column this feature needs but the
	// migration forgot fails here rather than on the server.
	if err := database.AutoMigrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	jwt := auth.NewManager("test-secret-test-secret-test-secret", time.Hour)
	svc := services.New(repositories.New(db), jwt, nil, services.TrackingOptions{}, services.ThumbOptions{}, "")
	t.Cleanup(func() { svc.Audit.Drain(context.Background()) })

	h := &openHarness{
		t:      t,
		router: New(&config.Config{MaxBodyBytes: 1 << 20}, handlers.New(svc), jwt),
		db:     db,
		tokens: map[models.Role]string{},
	}

	for _, code := range []string{"S1", "S2"} {
		if err := db.Create(&models.Seller{Code: code, Name: "Seller " + code}).Error; err != nil {
			t.Fatalf("seed seller: %v", err)
		}
	}
	mat := &models.Material{Code: "MICA", Name: "Mica 3 ly"}
	sku := &models.SKU{Code: "TESTSKU", Name: "Test SKU", ProductName: "Test SKU"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	if err := db.Create(sku).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}
	if err := db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mat.ID, QuantityPerUnit: 1}).Error; err != nil {
		t.Fatalf("seed sku-material: %v", err)
	}
	sellerID := uint(1)
	for _, r := range []models.Role{models.RoleOwner, models.RoleOps, models.RoleSeller} {
		u := &models.User{Email: strings.ToLower(string(r)) + "@t", PasswordHash: "x", FullName: string(r), Role: r, IsActive: true}
		if r == models.RoleSeller {
			u.SellerID = &sellerID
		}
		if err := db.Create(u).Error; err != nil {
			t.Fatalf("seed %s: %v", r, err)
		}
		tok, _, err := jwt.Issue(u)
		if err != nil {
			t.Fatalf("issue %s: %v", r, err)
		}
		h.tokens[r] = tok
	}
	return h
}

// do sends one request. auth is the full Authorization header value ("" = none).
func (h *openHarness) do(method, path, auth, body string) openResp {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	out := openResp{status: w.Code, header: w.Header(), raw: w.Body.String()}
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(w.Body.Bytes(), &out.body); err != nil {
			h.t.Fatalf("%s %s: body is not the envelope: %v\n%s", method, path, err, out.raw)
		}
	}
	return out
}

func (r openResp) errCode() string {
	if r.body.Error == nil {
		return ""
	}
	return r.body.Error.Code
}

// issueKey creates a key for the seller through the internal endpoint, as OWNER.
func (h *openHarness) issueKey(sellerID string) (id uint, key string) {
	h.t.Helper()
	res := h.do("POST", "/api/sellers/"+sellerID+"/api-keys", "Bearer "+h.tokens[models.RoleOwner], `{"name":"Hệ thống của seller"}`)
	if res.status != http.StatusCreated {
		h.t.Fatalf("issue key: %d %s", res.status, res.raw)
	}
	var created struct {
		ID     uint   `json:"id"`
		Key    string `json:"key"`
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(res.body.Data, &created); err != nil || created.Key == "" {
		h.t.Fatalf("issue key: bad body %s", res.raw)
	}
	if strings.Contains(res.raw, "key_hash") {
		h.t.Fatalf("the key hash must never be in a response: %s", res.raw)
	}
	return created.ID, created.Key
}

const openOrderBody = `{
  "order_id": "ETSY-1001",
  "shipping": {"name": "Jane Doe", "address1": "1 Main St", "country": "US"},
  "items": [{"sku": "TESTSKU", "quantity": 2, "mockup_url": "https://example.com/m.png"}]
}`

// The full life of an integration: get a key, prove it works, send an order,
// resend it, read it back, lose the key.
func TestOpenAPI_EndToEnd(t *testing.T) {
	h := newOpenHarness(t)
	keyID, key := h.issueKey("1")
	_, otherKey := h.issueKey("2")
	bearer := "Bearer " + key

	// ping says whose key this is.
	res := h.do("GET", "/api/open/v1/ping", bearer, "")
	if res.status != http.StatusOK || !strings.Contains(res.raw, `"seller_code":"S1"`) {
		t.Fatalf("ping: %d %s", res.status, res.raw)
	}

	// First send creates (201); the resend is the same order (200, created=false).
	type createData struct {
		Created bool `json:"created"`
		Order   struct {
			Code    string `json:"code"`
			OrderID string `json:"order_id"`
			Status  string `json:"status"`
			Items   []struct {
				SKU         string `json:"sku"`
				ProductName string `json:"product_name"`
				Quantity    int    `json:"quantity"`
			} `json:"items"`
		} `json:"order"`
	}
	var first, second createData
	res = h.do("POST", "/api/open/v1/orders", bearer, openOrderBody)
	if res.status != http.StatusCreated {
		t.Fatalf("create: %d %s", res.status, res.raw)
	}
	_ = json.Unmarshal(res.body.Data, &first)
	if !first.Created || first.Order.Status != "PENDING_REVIEW" || first.Order.Code == "" ||
		len(first.Order.Items) != 1 || first.Order.Items[0].ProductName != "Test SKU" || first.Order.Items[0].Quantity != 2 {
		t.Fatalf("create data: %s", res.raw)
	}
	res = h.do("POST", "/api/open/v1/orders", bearer, openOrderBody)
	_ = json.Unmarshal(res.body.Data, &second)
	if res.status != http.StatusOK || second.Created || second.Order.Code != first.Order.Code {
		t.Fatalf("resend: %d %s", res.status, res.raw)
	}
	var orders int64
	h.db.Model(&models.Order{}).Count(&orders)
	if orders != 1 {
		t.Fatalf("orders = %d, want 1", orders)
	}
	// The order belongs to the KEY's seller and waits for the factory's review.
	var stored models.Order
	h.db.First(&stored)
	if stored.SellerID != 1 || stored.ReviewStatus != models.ReviewPending {
		t.Errorf("stored order: seller %d review %s", stored.SellerID, stored.ReviewStatus)
	}

	// Read it back by the caller's id, by our code, and through the list.
	for _, path := range []string{"/api/open/v1/orders/ETSY-1001", "/api/open/v1/orders/" + first.Order.Code} {
		if res = h.do("GET", path, bearer, ""); res.status != http.StatusOK || !strings.Contains(res.raw, `"order_id":"ETSY-1001"`) {
			t.Errorf("GET %s: %d %s", path, res.status, res.raw)
		}
	}
	res = h.do("GET", "/api/open/v1/orders?status=PENDING_REVIEW", bearer, "")
	if res.status != http.StatusOK || !strings.Contains(res.raw, `"total":1`) {
		t.Errorf("list: %d %s", res.status, res.raw)
	}
	// Nothing internal leaks into the public shape.
	for _, leak := range []string{"seller_id", "tracking_url", "internal_status", "created_by_id", "api_ref"} {
		if strings.Contains(res.raw, leak) {
			t.Errorf("list response leaks %q: %s", leak, res.raw)
		}
	}

	// The other seller's key sees none of it.
	if res = h.do("GET", "/api/open/v1/orders/ETSY-1001", "Bearer "+otherKey, ""); res.status != http.StatusNotFound || res.errCode() != "ORDER_NOT_FOUND" {
		t.Errorf("foreign get: %d %s", res.status, res.raw)
	}
	if res = h.do("GET", "/api/open/v1/orders", "Bearer "+otherKey, ""); !strings.Contains(res.raw, `"total":0`) {
		t.Errorf("foreign list: %s", res.raw)
	}

	// X-API-Key works as well as the Authorization header.
	req := httptest.NewRequest("GET", "/api/open/v1/ping", nil)
	req.Header.Set("X-API-Key", key)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("X-API-Key: %d %s", w.Code, w.Body.String())
	}

	// Revoked: the very next request is refused.
	res = h.do("DELETE", "/api/sellers/1/api-keys/"+itoa(keyID), "Bearer "+h.tokens[models.RoleOwner], "")
	if res.status != http.StatusOK || !strings.Contains(res.raw, `"revoked_at":"`) {
		t.Fatalf("revoke: %d %s", res.status, res.raw)
	}
	if res = h.do("GET", "/api/open/v1/ping", bearer, ""); res.status != http.StatusUnauthorized || res.errCode() != "API_KEY_INVALID" {
		t.Errorf("after revoke: %d %s", res.status, res.raw)
	}
	// The key list keeps the revoked key as history, without its hash.
	res = h.do("GET", "/api/sellers/1/api-keys", "Bearer "+h.tokens[models.RoleOwner], "")
	if res.status != http.StatusOK || !strings.Contains(res.raw, `"revoked_at":"`) || strings.Contains(res.raw, key) {
		t.Errorf("key list: %d %s", res.status, res.raw)
	}
}

func itoa(n uint) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// What a caller gets wrong, and what they are told.
func TestOpenAPI_Rejections(t *testing.T) {
	h := newOpenHarness(t)
	_, key := h.issueKey("1")
	bearer := "Bearer " + key

	cases := []struct {
		name, method, path, auth, body string
		status                         int
		code                           string
	}{
		{"no key", "GET", "/api/open/v1/ping", "", "", 401, "API_KEY_MISSING"},
		{"unknown key", "GET", "/api/open/v1/ping", "Bearer ffm_" + strings.Repeat("0", 48), "", 401, "API_KEY_INVALID"},
		// A web login token is not an API key — and an API key is not a login.
		{"jwt on the open api", "POST", "/api/open/v1/orders", "Bearer " + h.tokens[models.RoleOwner], openOrderBody, 401, "API_KEY_INVALID"},
		{"api key on the internal api", "GET", "/api/orders", bearer, "", 401, "UNAUTHORIZED"},
		{"broken json", "POST", "/api/open/v1/orders", bearer, `{"order_id": `, 400, "INVALID_JSON"},
		{"wrong type", "POST", "/api/open/v1/orders", bearer, `{"order_id": "A", "items": "nope"}`, 400, "INVALID_JSON"},
		{"empty body", "POST", "/api/open/v1/orders", bearer, ``, 400, "INVALID_JSON"},
		{"invalid order", "POST", "/api/open/v1/orders", bearer, `{"order_id": "A", "items": [{"sku": "NOPE"}]}`, 422, "VALIDATION_ERROR"},
		{"unknown status", "GET", "/api/open/v1/orders?status=WHATEVER", bearer, "", 400, "STATUS_INVALID"},
		{"unknown order", "GET", "/api/open/v1/orders/NOPE", bearer, "", 404, "ORDER_NOT_FOUND"},
	}
	for _, c := range cases {
		res := h.do(c.method, c.path, c.auth, c.body)
		if res.status != c.status || res.errCode() != c.code {
			t.Errorf("%s: got %d %s, want %d %s\n%s", c.name, res.status, res.errCode(), c.status, c.code, res.raw)
		}
	}

	// A 422 names each field the way the caller sent it.
	res := h.do("POST", "/api/open/v1/orders", bearer, `{"order_id": "A", "items": [{"sku": "NOPE"}]}`)
	var details []struct{ Field, Code string }
	_ = json.Unmarshal(res.body.Error.Details, &details)
	got := map[string]string{}
	for _, d := range details {
		got[d.Field] = d.Code
	}
	for field, code := range map[string]string{"shipping.name": "REQUIRED", "shipping.country": "REQUIRED", "items[0].sku": "SKU_NOT_FOUND"} {
		if got[field] != code {
			t.Errorf("422 details: %s = %q, want %q (%s)", field, got[field], code, res.raw)
		}
	}
	var orders int64
	h.db.Model(&models.Order{}).Count(&orders)
	if orders != 0 {
		t.Errorf("rejected requests created %d orders", orders)
	}

	// A paused seller is told so, in its own code.
	h.db.Model(&models.Seller{}).Where("id = ?", 2).Update("status", "paused")
	_, pausedKey := h.issueKey("2")
	if res = h.do("GET", "/api/open/v1/ping", "Bearer "+pausedKey, ""); res.status != http.StatusForbidden || res.errCode() != "SELLER_PAUSED" {
		t.Errorf("paused seller: %d %s", res.status, res.raw)
	}
}

// Issuing or revoking a key decides who may send orders in a seller's name:
// ADMIN/OWNER only. A seller's own login cannot mint itself a key.
func TestOpenAPI_KeyManagementIsAdminOnly(t *testing.T) {
	h := newOpenHarness(t)
	for _, role := range []models.Role{models.RoleOps, models.RoleSeller} {
		res := h.do("POST", "/api/sellers/1/api-keys", "Bearer "+h.tokens[role], `{"name":"x"}`)
		if res.status != http.StatusForbidden {
			t.Errorf("%s creating a key: %d, want 403", role, res.status)
		}
		res = h.do("DELETE", "/api/sellers/1/api-keys/1", "Bearer "+h.tokens[role], "")
		if res.status != http.StatusForbidden {
			t.Errorf("%s revoking a key: %d, want 403", role, res.status)
		}
	}
	// OPS manages master data, so it may SEE which keys exist; a seller may not.
	if res := h.do("GET", "/api/sellers/1/api-keys", "Bearer "+h.tokens[models.RoleOps], ""); res.status != http.StatusOK {
		t.Errorf("OPS listing keys: %d", res.status)
	}
	if res := h.do("GET", "/api/sellers/1/api-keys", "Bearer "+h.tokens[models.RoleSeller], ""); res.status != http.StatusForbidden {
		t.Errorf("SELLER listing keys: %d, want 403", res.status)
	}
	var keys int64
	h.db.Model(&models.APIKey{}).Count(&keys)
	if keys != 0 {
		t.Errorf("%d keys were created by roles that must not", keys)
	}
}

// The reference is what gets sent to the seller's developers, so it has to be
// reachable without a key — and under /api, the only prefix the proxy forwards.
func TestOpenAPI_DocsArePublic(t *testing.T) {
	h := newOpenHarness(t)
	res := h.do("GET", "/api/open/docs", "", "")
	if res.status != http.StatusOK || !strings.HasPrefix(res.header.Get("Content-Type"), "text/html") ||
		!strings.Contains(res.raw, "/api/open/v1") {
		t.Errorf("guide: %d %s", res.status, res.header.Get("Content-Type"))
	}
	res = h.do("GET", "/api/open/v1/openapi.yaml", "", "")
	if res.status != http.StatusOK || !strings.Contains(res.raw, "openapi: 3.0.3") {
		t.Errorf("spec: %d", res.status)
	}
}
