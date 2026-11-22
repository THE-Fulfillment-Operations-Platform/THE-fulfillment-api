package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/xuri/excelize/v2"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/database"
	"the-fulfillment/backend/internal/handlers"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/services"
)

// Lọc chọn nhiều qua HTTP thật (router → handler → service → SQL): đúng cách web
// gọi. Kiểm tổng đếm (meta.total), phân trang, hai kiểu gửi nhiều giá trị, tham
// số đơn kiểu cũ, và nút "Xuất theo mẫu import" theo cùng bộ lọc với danh sách.

type multiHarness struct {
	t      *testing.T
	router http.Handler
	db     *gorm.DB
	tokens map[models.Role]string
}

func newMultiHarness(t *testing.T) *multiHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := database.AutoMigrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	jwt := auth.NewManager("test-secret-test-secret-test-secret", time.Hour)
	svc := services.New(repositories.New(db), jwt, nil, services.TrackingOptions{}, services.ThumbOptions{}, "")
	t.Cleanup(func() { svc.Audit.Drain(context.Background()) })
	h := &multiHarness{
		t:      t,
		router: New(&config.Config{MaxBodyBytes: 1 << 20}, handlers.New(svc), jwt),
		db:     db,
		tokens: map[models.Role]string{},
	}
	for _, r := range []models.Role{models.RoleOps, models.RoleTechnician} {
		u := &models.User{Email: strings.ToLower(string(r)) + "@t", PasswordHash: "x", FullName: string(r), Role: r, IsActive: true}
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

type listResp struct {
	Success bool              `json:"success"`
	Data    []json.RawMessage `json:"data"`
	Meta    struct {
		Page       int   `json:"page"`
		PageSize   int   `json:"page_size"`
		Total      int64 `json:"total"`
		TotalPages int   `json:"total_pages"`
	} `json:"meta"`
}

func (h *multiHarness) get(path string, role models.Role) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+h.tokens[role])
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func (h *multiHarness) list(path string) listResp {
	h.t.Helper()
	w := h.get(path, models.RoleOps)
	if w.Code != http.StatusOK {
		h.t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	var out listResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.Success {
		h.t.Fatalf("GET %s: bad body %v %s", path, err, w.Body.String())
	}
	return out
}

func TestMultiSelectFilters_HTTP(t *testing.T) {
	h := newMultiHarness(t)
	db := h.db
	sellers := make([]*models.Seller, 3)
	for i := range sellers {
		sellers[i] = &models.Seller{Code: fmt.Sprintf("HS%d", i+1), Name: fmt.Sprintf("Seller %d", i+1)}
		if err := db.Create(sellers[i]).Error; err != nil {
			t.Fatalf("seller: %v", err)
		}
	}
	mat := &models.Material{Code: "HM", Name: "Gỗ"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("material: %v", err)
	}
	n := 0
	item := func(s *models.Seller, st models.SellerStatus, is models.InternalStatus) *models.OrderItem {
		n++
		code := fmt.Sprintf("50%04d", n)
		o := &models.Order{InternalCode: code, StoreOrderID: "SO-" + code, SellerID: s.ID,
			ReviewStatus: models.ReviewApproved, SellerStatus: st, ShippingName: "Jane", ShippingCountry: "US"}
		if err := db.Create(o).Error; err != nil {
			t.Fatalf("order: %v", err)
		}
		it := &models.OrderItem{OrderID: o.ID, InternalCode: code + "_1/1", SKUCode: "SKU-H", Quantity: 1, LineNo: 1, DesignStatus: models.DesignReady}
		if err := db.Create(it).Error; err != nil {
			t.Fatalf("item: %v", err)
		}
		if err := db.Model(it).Update("internal_status", is).Error; err != nil {
			t.Fatalf("internal_status: %v", err)
		}
		return it
	}
	batch := func(code string, status models.InternalStatus, links bool, items ...*models.OrderItem) {
		b := &models.Batch{Code: code, MaterialID: mat.ID, Status: status}
		if err := db.Create(b).Error; err != nil {
			t.Fatalf("batch: %v", err)
		}
		if links {
			for _, k := range []models.BatchLinkKind{models.BatchLinkPrint, models.BatchLinkCut} {
				if err := db.Create(&models.BatchLink{BatchID: b.ID, Kind: k, URL: "https://f/" + code + string(k), LinkUpdatedAt: time.Now()}).Error; err != nil {
					t.Fatalf("link: %v", err)
				}
			}
		}
		for _, it := range items {
			if err := db.Create(&models.BatchItem{BatchID: b.ID, OrderItemID: it.ID, MaterialID: mat.ID, Status: status}).Error; err != nil {
				t.Fatalf("batch item: %v", err)
			}
		}
	}
	s1, s2, s3 := sellers[0], sellers[1], sellers[2]
	prod := models.SellerStatusProduction
	pendReady := item(s1, prod, models.StatusPending)
	pendMissing := item(s2, prod, models.StatusPending)
	printed := item(s1, prod, models.StatusPrinted)
	cut := item(s3, prod, models.StatusCut)
	handed := item(s2, models.SellerStatusHandedOff, models.StatusQCPassed)
	_ = item(s3, models.SellerStatusShipped, models.StatusQCPassed) // đã bàn giao, seller 3
	batch("#H-READY", models.StatusPending, true, pendReady)
	batch("#H-NOFILE", models.StatusPending, false, pendMissing)
	batch("#H-PRINTED", models.StatusPrinted, true, printed)
	batch("#H-CUT", models.StatusCut, true, cut)
	batch("#H-HANDED", models.StatusQCPassed, true, handed)

	// ---- /api/batches ----
	multi := "/api/batches?status=FILES_MISSING,FILES_READY,PRINTED"
	if r := h.list(multi + "&page=1&page_size=2"); r.Meta.Total != 3 || len(r.Data) != 2 || r.Meta.TotalPages != 2 {
		t.Errorf("batches multi trang 1: total=%d rows=%d pages=%d, want 3/2/2", r.Meta.Total, len(r.Data), r.Meta.TotalPages)
	}
	if r := h.list(multi + "&page=2&page_size=2"); r.Meta.Total != 3 || len(r.Data) != 1 {
		t.Errorf("batches multi trang 2: total=%d rows=%d, want 3/1", r.Meta.Total, len(r.Data))
	}
	if r := h.list("/api/batches?status=FILES_MISSING&status=FILES_READY&status=PRINTED"); r.Meta.Total != 3 {
		t.Errorf("batches khoá lặp: total=%d, want 3", r.Meta.Total)
	}
	// Tham số đơn kiểu cũ vẫn y như trước.
	if r := h.list("/api/batches?status=PENDING&files=missing"); r.Meta.Total != 1 {
		t.Errorf("batches cũ PENDING+missing: total=%d, want 1", r.Meta.Total)
	}
	if r := h.list("/api/batches?status=PENDING"); r.Meta.Total != 2 {
		t.Errorf("batches cũ PENDING (dashboard): total=%d, want 2", r.Meta.Total)
	}
	if r := h.list(fmt.Sprintf("/api/batches?seller_id=%d", s3.ID)); r.Meta.Total != 1 {
		t.Errorf("batches cũ 1 seller: total=%d, want 1", r.Meta.Total)
	}
	// Seller nhiều + AND với trạng thái.
	if r := h.list(fmt.Sprintf("/api/batches?seller_id=%d,%d", s1.ID, s2.ID)); r.Meta.Total != 4 {
		t.Errorf("batches seller 1+2: total=%d, want 4", r.Meta.Total)
	}
	if r := h.list(fmt.Sprintf("/api/batches?seller_id=%d&seller_id=%d&status=FILES_MISSING,HANDED_OVER", s1.ID, s2.ID)); r.Meta.Total != 2 {
		t.Errorf("batches seller 1+2 × Chờ xử lý + Đã bàn giao: total=%d, want 2", r.Meta.Total)
	}

	// ---- /api/items + xuất theo mẫu import (cùng bộ lọc) ----
	itemQ := fmt.Sprintf("status=FILES_MISSING,PRINTED,HANDED_OVER&seller_id=%d,%d", s1.ID, s2.ID)
	r := h.list("/api/items?" + itemQ + "&page=1&page_size=2")
	if r.Meta.Total != 3 || len(r.Data) != 2 {
		t.Errorf("items multi: total=%d rows=%d, want 3/2 (pendMissing, printed, handed)", r.Meta.Total, len(r.Data))
	}
	if r := h.list("/api/items?status=PENDING&files=ready"); r.Meta.Total != 1 {
		t.Errorf("items cũ PENDING+ready: total=%d, want 1", r.Meta.Total)
	}
	if r := h.list("/api/items?handed_over=true"); r.Meta.Total != 2 {
		t.Errorf("items cũ handed_over: total=%d, want 2", r.Meta.Total)
	}

	w := h.get("/api/orders/export/import-template.xlsx?"+itemQ, models.RoleTechnician)
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	f, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("export xlsx: %v", err)
	}
	rows, err := f.GetRows(f.GetSheetList()[0])
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if got := len(rows) - 1; got != 3 {
		t.Errorf("xuất theo mẫu import: %d dòng, want 3 (đúng bằng tổng của danh sách)", got)
	}
	body := strings.Join(func() []string {
		out := []string{}
		for _, row := range rows[1:] {
			out = append(out, strings.Join(row, "|"))
		}
		return out
	}(), "\n")
	for _, code := range []string{"HS1", "HS2"} {
		if !strings.Contains(body, code) {
			t.Errorf("file xuất thiếu seller %s:\n%s", code, body)
		}
	}
	if strings.Contains(body, "HS3") {
		t.Errorf("file xuất lọt seller 3:\n%s", body)
	}
}
