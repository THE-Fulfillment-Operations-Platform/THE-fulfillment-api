package routes

import (
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
	"the-fulfillment/backend/internal/handlers"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/services"
)

// The role sets every route was guarded by before per-user permissions. An
// account whose permissions were never customised must reach exactly what it
// reached then — this test pins that, route by route, role by role.
var (
	oAdminOwner  = []models.Role{models.RoleOwner, models.RoleAdmin}
	oOpsAdmin    = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps}
	oDesignOps   = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleDesigner}
	oProdOps     = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleProduction, models.RoleDesigner}
	oQCOps       = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleQC}
	oPackOps     = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RolePacking}
	oScrap       = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleProduction, models.RoleQC}
	oShipCarrier = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RolePacking, models.RoleShipping}
	oShipOps     = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RolePacking, models.RoleShipping, models.RoleCS}
	oInternal    = []models.Role{models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RoleDesigner,
		models.RoleProduction, models.RoleQC, models.RolePacking, models.RoleShipping}
	oOrderRead = append(append([]models.Role{}, oInternal...), models.RoleCS)
	oOwner     = []models.Role{models.RoleOwner}
	oSeller    = []models.Role{models.RoleSeller}
)

type routeCase struct {
	method, path string
	old          []models.Role
	// widened lists roles that may now reach a route they could not before —
	// each one a deliberate, harmless read (or a sibling station) noted below.
	widened []models.Role
}

var routeCases = []routeCase{
	{"POST", "/api/users", oAdminOwner, nil},
	{"GET", "/api/users", oAdminOwner, nil},
	{"PUT", "/api/users/1", oAdminOwner, nil},
	{"DELETE", "/api/users/1", oAdminOwner, nil},
	{"GET", "/api/audit-logs", oAdminOwner, nil},
	{"POST", "/api/admin/reset", oOwner, nil},

	{"GET", "/api/sellers", oOrderRead, nil},
	{"POST", "/api/sellers", oOpsAdmin, nil},
	{"PUT", "/api/sellers/1", oOpsAdmin, nil},
	{"DELETE", "/api/sellers/1", oAdminOwner, nil},
	// Reference data: CS may now read stores/materials/SKUs like the sellers
	// list they could already read.
	{"GET", "/api/stores", oInternal, []models.Role{models.RoleCS}},
	{"POST", "/api/stores", oOpsAdmin, nil},
	{"DELETE", "/api/stores/1", oAdminOwner, nil},
	{"GET", "/api/materials", oInternal, []models.Role{models.RoleCS}},
	// Material import: was the OWNER-only quota lever; now it imports sheet sizes
	// (master data) and opens to Master Data's manage like the SKU import.
	{"POST", "/api/materials/import/commit", oOwner, []models.Role{models.RoleAdmin, models.RoleOps}},
	{"POST", "/api/materials", oOpsAdmin, nil},
	{"PUT", "/api/materials/1", oOpsAdmin, nil},
	{"DELETE", "/api/materials/1", oAdminOwner, nil},
	{"POST", "/api/materials/bulk-delete", oAdminOwner, nil},
	{"GET", "/api/skus", oInternal, []models.Role{models.RoleCS}},
	{"POST", "/api/skus", oOpsAdmin, nil},
	{"POST", "/api/skus/bulk-delete", oAdminOwner, nil},
	{"POST", "/api/skus/bulk-active", oOpsAdmin, nil},

	{"GET", "/api/orders", oOrderRead, nil},
	{"GET", "/api/orders/1", oOrderRead, nil},
	{"POST", "/api/orders", oOpsAdmin, nil},
	{"POST", "/api/orders/import", oOpsAdmin, nil},
	{"POST", "/api/orders/import/commit", oOpsAdmin, nil},
	{"PUT", "/api/orders/1", oOpsAdmin, nil},
	{"POST", "/api/orders/1/cancel", oOpsAdmin, nil},
	{"DELETE", "/api/orders/1", oAdminOwner, nil},
	{"POST", "/api/orders/bulk-delete", oAdminOwner, nil},
	{"POST", "/api/orders/ship-to-carrier", oShipCarrier, nil},
	{"POST", "/api/orders/ship-scan", oShipCarrier, nil},
	{"PATCH", "/api/orders/1/tracking", oShipOps, nil},
	{"POST", "/api/orders/tracking/import", oShipOps, nil},
	{"POST", "/api/orders/tracking/import/commit", oShipOps, nil},
	{"GET", "/api/orders/1/tracking/events", oOrderRead, nil},
	{"POST", "/api/orders/1/tracking/sync", oShipOps, nil},
	{"POST", "/api/tracking/sync", oShipOps, nil},
	{"GET", "/api/import-jobs", oOpsAdmin, nil},
	{"POST", "/api/master-data/import/commit", oOpsAdmin, nil},
	{"POST", "/api/master-data/parents/import/preview", oOpsAdmin, nil},
	{"POST", "/api/master-data/parents/import/commit", oOpsAdmin, nil},

	{"GET", "/api/review/orders", oDesignOps, nil},
	{"POST", "/api/review/orders/bulk-approve", oDesignOps, nil},
	{"POST", "/api/review/orders/1/approve", oDesignOps, nil},
	{"GET", "/api/cancellation-requests", oOpsAdmin, nil},
	{"POST", "/api/cancellation-requests/1/approve", oOpsAdmin, nil},
	{"POST", "/api/cancellation-requests/items/1/reject", oOpsAdmin, nil},

	{"GET", "/api/items", oInternal, nil},
	{"GET", "/api/items/1", oInternal, nil},
	{"PATCH", "/api/items/1/design", oDesignOps, nil},
	{"GET", "/api/action-counts", oOrderRead, nil},
	{"GET", "/api/design-queue", oDesignOps, nil},
	{"POST", "/api/design-queue/set-ready", oDesignOps, nil},
	{"GET", "/api/design-queue/material-buckets", oDesignOps, nil},

	{"GET", "/api/batches", oInternal, nil},
	{"GET", "/api/batches/1", oInternal, nil},
	{"GET", "/api/batches/1/assets.zip", oInternal, nil},
	{"POST", "/api/batches", oDesignOps, nil},
	{"POST", "/api/batches/auto", oDesignOps, nil},
	{"PUT", "/api/batches/1/links", oDesignOps, nil},
	{"DELETE", "/api/batches/1", oDesignOps, nil},
	{"PATCH", "/api/batches/1/status", oProdOps, nil},
	{"POST", "/api/batches/1/scrap", oScrap, nil},

	{"POST", "/api/qc/scan", oQCOps, nil},
	{"POST", "/api/qc/pass", oQCOps, nil},
	{"POST", "/api/qc/fail", oQCOps, nil},
	{"POST", "/api/qc/undo", oAdminOwner, nil},
	{"GET", "/api/qc/results", oInternal, nil},

	// The off-menu packing station now sits with the ship queue, which the
	// shipping desk already worked.
	{"POST", "/api/packing/scan", oPackOps, []models.Role{models.RoleShipping}},
	{"GET", "/api/packing/order/1", oPackOps, []models.Role{models.RoleShipping}},
	{"POST", "/api/handoffs", oShipOps, nil},
	// CS may now list handoffs (read-only) from the journeys board they work.
	{"GET", "/api/handoffs", oInternal, []models.Role{models.RoleCS}},
	{"POST", "/api/handoffs/1/ship", oShipOps, nil},

	{"GET", "/api/notes", oOrderRead, nil},
	{"POST", "/api/notes", oOrderRead, nil},
	{"DELETE", "/api/notes/1", oOrderRead, nil},

	{"GET", "/api/seller/orders", oSeller, nil},
	{"POST", "/api/seller/orders/1/cancel", oSeller, nil},
}

func has(roles []models.Role, r models.Role) bool {
	for _, x := range roles {
		if x == r {
			return true
		}
	}
	return false
}

type accessHarness struct {
	router http.Handler
	tokens map[models.Role]string
	db     *gorm.DB
	jwt    *auth.Manager
}

func newAccessHarness(t *testing.T) *accessHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	// Handlers run with no services behind them and panic once a guard lets a
	// request through; that 500 is the "allowed" signal. Keep the log quiet.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Seller{}, &models.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := repositories.New(db)
	svc := &services.Services{Access: services.NewAccessService(repo)}
	jwt := auth.NewManager("test-secret-test-secret-test-secret", time.Hour)
	h := &accessHarness{
		router: New(&config.Config{MaxBodyBytes: 1 << 20}, handlers.New(svc), jwt),
		tokens: map[models.Role]string{},
		db:     db,
		jwt:    jwt,
	}
	seller := &models.Seller{Code: "S1", Name: "S1"}
	db.Create(seller)
	for _, r := range models.AllRoles {
		u := &models.User{Email: strings.ToLower(string(r)) + "@t", PasswordHash: "x", FullName: string(r), Role: r, IsActive: true}
		if r == models.RoleSeller {
			u.SellerID = &seller.ID
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

func (h *accessHarness) status(method, path, token string) int {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w.Code
}

// TestRoleDefaultsKeepPreviousAccess: with permissions never customised, every
// role reaches exactly the routes it reached under the old role guards (plus
// the few deliberate widenings listed in routeCases).
func TestRoleDefaultsKeepPreviousAccess(t *testing.T) {
	h := newAccessHarness(t)
	for _, rc := range routeCases {
		for _, role := range models.AllRoles {
			want := has(rc.old, role) || has(rc.widened, role)
			code := h.status(rc.method, rc.path, h.tokens[role])
			if code == http.StatusUnauthorized || code == http.StatusNotFound {
				t.Fatalf("%s %s as %s: unexpected %d (route missing or auth broken)", rc.method, rc.path, role, code)
			}
			if got := code != http.StatusForbidden; got != want {
				t.Errorf("%s %s as %s: allowed=%v, want %v (status %d)", rc.method, rc.path, role, got, want, code)
			}
		}
	}
}

// TestCustomTicksApplyWithoutRelogin: ticks stored on the user decide access on
// the next request — the token is untouched — both widening and narrowing.
func TestCustomTicksApplyWithoutRelogin(t *testing.T) {
	h := newAccessHarness(t)
	var qc models.User
	h.db.Where("role = ?", models.RoleQC).First(&qc)
	token := h.tokens[models.RoleQC]

	if code := h.status("POST", "/api/orders/ship-to-carrier", token); code != http.StatusForbidden {
		t.Fatalf("QC shipping before the tick: status %d, want 403", code)
	}
	// Give this QC the ship queue and take QC verdicts away.
	h.db.Model(&qc).Update("permissions", models.PermList{models.Manage(models.FeatShipQueue), models.View(models.FeatQC)})
	// Another process would wait out the cache TTL; here a fresh router has an
	// empty cache, which is what the invalidation on user update gives in prod.
	h2 := *h
	h2.router = New(&config.Config{MaxBodyBytes: 1 << 20},
		handlers.New(&services.Services{Access: services.NewAccessService(repositories.New(h.db))}), h.jwt)

	if code := h2.status("POST", "/api/orders/ship-to-carrier", token); code == http.StatusForbidden {
		t.Errorf("ticked ship queue should allow shipping, got 403")
	}
	if code := h2.status("POST", "/api/qc/pass", token); code != http.StatusForbidden {
		t.Errorf("unticked QC manage should block pass, got %d", code)
	}
	if code := h2.status("POST", "/api/qc/scan", token); code == http.StatusForbidden {
		t.Errorf("QC view should still allow scanning, got 403")
	}

	// Locking the account ends the session on the next request.
	h.db.Model(&qc).Update("is_active", false)
	h3 := h2
	h3.router = New(&config.Config{MaxBodyBytes: 1 << 20},
		handlers.New(&services.Services{Access: services.NewAccessService(repositories.New(h.db))}), h.jwt)
	if code := h3.status("GET", "/api/qc/results", token); code != http.StatusUnauthorized {
		t.Errorf("locked account: status %d, want 401", code)
	}
}
