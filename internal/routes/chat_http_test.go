package routes

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/chatfiles"
	"the-fulfillment/backend/internal/chathub"
	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/handlers"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/services"
)

// Chat qua HTTP thật (router + middleware + handler): quyền mặc định (mọi vai trò
// nội bộ được, seller 403), tải file (cỡ, loại), phát file qua URL ký không cần
// đăng nhập, header an toàn.

type chatHTTP struct {
	router http.Handler
	tokens map[models.Role]string
	ids    map[models.Role]uint
}

func newChatHTTP(t *testing.T, fileMax int64) *chatHTTP {
	t.Helper()
	gin.SetMode(gin.TestMode)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Seller{}, &models.User{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(models.ChatModels()...); err != nil {
		t.Fatal(err)
	}
	repo := repositories.New(db)
	store, err := chatfiles.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{MaxBodyBytes: 32 << 20}
	chat := services.NewChatService(repo, chathub.New(), services.ChatOptions{
		Roles: services.ParseChatRoles("ALL"), Files: store, FileMaxBytes: fileMax,
		FileSecret: []byte("k"), MinFreeBytes: 1,
	})
	svc := &services.Services{Access: services.NewAccessService(repo), Chat: chat}
	jwt := auth.NewManager("test-secret-test-secret-test-secret", time.Hour)
	h := &chatHTTP{router: New(&cfg, handlers.New(svc), jwt), tokens: map[models.Role]string{}, ids: map[models.Role]uint{}}
	seller := &models.Seller{Code: "S1", Name: "S1"}
	db.Create(seller)
	for _, r := range models.AllRoles {
		u := &models.User{Email: strings.ToLower(string(r)) + "@t", PasswordHash: "x", FullName: string(r), Role: r, IsActive: true}
		if r == models.RoleSeller {
			u.SellerID = &seller.ID
		}
		if err := db.Create(u).Error; err != nil {
			t.Fatal(err)
		}
		tok, _, err := jwt.Issue(u)
		if err != nil {
			t.Fatal(err)
		}
		h.tokens[r], h.ids[r] = tok, u.ID
	}
	return h
}

func (h *chatHTTP) do(method, path string, role models.Role, body io.Reader, ctype string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if role != "" {
		req.Header.Set("Authorization", "Bearer "+h.tokens[role])
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func (h *chatHTTP) json(method, path string, role models.Role, v interface{}) *httptest.ResponseRecorder {
	b, _ := json.Marshal(v)
	return h.do(method, path, role, bytes.NewReader(b), "application/json")
}

func (h *chatHTTP) upload(path string, role models.Role, name string, data []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	_ = mw.Close()
	return h.do(http.MethodPost, path, role, &buf, mw.FormDataContentType())
}

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decode(t *testing.T, w *httptest.ResponseRecorder, into interface{}) {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	if into != nil {
		if err := json.Unmarshal(env.Data, into); err != nil {
			t.Fatalf("data %s: %v", env.Data, err)
		}
	}
}

func TestChatHTTP_RolesAndFiles(t *testing.T) {
	h := newChatHTTP(t, 64<<10)

	// Mọi vai trò nội bộ vào được chat; seller 403 ở mọi cửa chat.
	for _, r := range models.AllRoles {
		w := h.do(http.MethodGet, "/api/chat/config", r, nil, "")
		want := http.StatusOK
		if r == models.RoleSeller {
			want = http.StatusForbidden
		}
		if w.Code != want {
			t.Fatalf("%s GET /api/chat/config = %d, want %d", r, w.Code, want)
		}
	}
	for _, p := range []struct{ m, path string }{
		{"GET", "/api/chat/users"}, {"GET", "/api/chat/conversations"}, {"POST", "/api/chat/groups"},
		{"POST", "/api/chat/ws-ticket"}, {"GET", "/api/chat/presence"}, {"GET", "/api/chat/search?q=abc"},
	} {
		if w := h.json(p.m, p.path, models.RoleSeller, map[string]interface{}{"title": "x", "user_ids": []uint{1}}); w.Code != http.StatusForbidden {
			t.Fatalf("seller %s %s = %d, want 403", p.m, p.path, w.Code)
		}
	}
	if w := h.do(http.MethodGet, "/api/chat/conversations", "", nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("không đăng nhập = %d, want 401", w.Code)
	}

	// Nhóm 3 người qua HTTP.
	w := h.json(http.MethodPost, "/api/chat/groups", models.RoleOps, map[string]interface{}{
		"title": "Tổ QC", "user_ids": []uint{h.ids[models.RoleQC], h.ids[models.RolePacking]},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("tạo nhóm = %d %s", w.Code, w.Body.String())
	}
	var g struct {
		ID      uint `json:"id"`
		Members []struct {
			ID uint `json:"id"`
		} `json:"members"`
	}
	decode(t, w, &g)
	if len(g.Members) != 3 {
		t.Fatalf("nhóm có %d người", len(g.Members))
	}
	base := "/api/chat/conversations/" + uitoa(g.ID)

	// File: quá cỡ 413, loại cấm 415, người ngoài nhóm 404.
	big := bytes.Repeat([]byte("a"), 70<<10)
	if w := h.upload(base+"/files", models.RoleOps, "to.txt", big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("file quá cỡ = %d %s", w.Code, w.Body.String())
	}
	if w := h.upload(base+"/files", models.RoleOps, "x.exe", []byte("MZ")); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("file .exe = %d", w.Code)
	}
	if w := h.upload(base+"/files", models.RoleCS, "a.txt", []byte("hi")); w.Code != http.StatusNotFound {
		t.Fatalf("người ngoài nhóm tải file = %d", w.Code)
	}
	var img bytes.Buffer
	_ = png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 10, 10)))
	w = h.upload(base+"/files", models.RoleOps, "ảnh mẫu.png", img.Bytes())
	if w.Code != http.StatusCreated {
		t.Fatalf("tải ảnh = %d %s", w.Code, w.Body.String())
	}
	var att struct {
		ID          uint   `json:"id"`
		URL         string `json:"url"`
		DownloadURL string `json:"download_url"`
	}
	decode(t, w, &att)
	w = h.upload(base+"/files", models.RoleOps, "ghi chu.txt", []byte("<script>alert(1)</script>"))
	var txt struct {
		ID  uint   `json:"id"`
		URL string `json:"url"`
	}
	decode(t, w, &txt)
	w = h.json(http.MethodPost, base+"/messages", models.RoleOps, map[string]interface{}{
		"body": "mẫu + ghi chú", "attachment_ids": []uint{att.ID, txt.ID}, "client_id": "c1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("gửi tin kèm file = %d %s", w.Code, w.Body.String())
	}

	// Phát file: không cần đăng nhập, URL ký là giấy phép.
	w = h.do(http.MethodGet, att.URL, "", nil, "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" ||
		w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "inline") ||
		!bytes.Equal(w.Body.Bytes(), img.Bytes()) {
		t.Fatalf("GET ảnh = %d %v", w.Code, w.Header())
	}
	if w := h.do(http.MethodGet, att.URL[:len(att.URL)-4]+"0000", "", nil, ""); w.Code != http.StatusNotFound {
		t.Fatalf("chữ ký sai = %d, want 404", w.Code)
	}
	// File chữ (có thể chứa HTML/script) luôn là tải về, kèm CSP sandbox.
	w = h.do(http.MethodGet, txt.URL, "", nil, "")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("GET file chữ = %d %v", w.Code, w.Header())
	}
	if w := h.do(http.MethodGet, att.DownloadURL, "", nil, ""); !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") ||
		!strings.Contains(w.Header().Get("Content-Disposition"), "filename*=UTF-8''%E1%BA%A3nh%20m%E1%BA%ABu.png") {
		t.Fatalf("tải về = %v", w.Header())
	}
}

func uitoa(v uint) string {
	b, _ := json.Marshal(v)
	return string(b)
}
