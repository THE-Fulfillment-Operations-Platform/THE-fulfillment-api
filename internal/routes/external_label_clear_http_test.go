package routes

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"the-fulfillment/backend/internal/models"
)

func (h *multiHarness) del(path string, role models.Role) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest("DELETE", path, nil)
	req.Header.Set("Authorization", "Bearer "+h.tokens[role])
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

// DELETE /orders/:id/label/external needs orders.manage: a role that can only
// read orders gets 403 and the link stays.
func TestClearExternalLabel_HTTPPermission(t *testing.T) {
	h := newMultiHarness(t)
	o := &models.Order{InternalCode: "100071", StoreOrderID: "S71", SellerID: 1,
		ReviewStatus: models.ReviewApproved, ExternalLabelURL: "https://files.example.com/a.pdf"}
	if err := h.db.Create(o).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/orders/" + strconv.Itoa(int(o.ID)) + "/label/external"
	qc := &models.User{Email: "qc71@t", PasswordHash: "x", FullName: "QC", Role: models.RoleQC, IsActive: true}
	if err := h.db.Create(qc).Error; err != nil {
		t.Fatal(err)
	}
	tok, _, err := h.jwt.Issue(qc)
	if err != nil {
		t.Fatal(err)
	}
	h.tokens[models.RoleQC] = tok
	if w := h.del(path, models.RoleQC); w.Code != http.StatusForbidden {
		t.Fatalf("qc: want 403, got %d %s", w.Code, w.Body.String())
	}
	var kept models.Order
	h.db.First(&kept, o.ID)
	if kept.ExternalLabelURL == "" {
		t.Fatal("link must stay after a refused call")
	}
	if w := h.del(path, models.RoleTechnician); w.Code != http.StatusOK {
		t.Fatalf("technician: want 200, got %d %s", w.Code, w.Body.String())
	}
	h.db.First(&kept, o.ID)
	if kept.ExternalLabelURL != "" {
		t.Fatalf("link must be cleared, got %q", kept.ExternalLabelURL)
	}
}
