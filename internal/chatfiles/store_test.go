package chatfiles

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDisk_PutLocateDelete(t *testing.T) {
	d, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.Put(ctx, "c1/2026-11/ab.png", []byte("x"), "image/png"); err != nil {
		t.Fatal(err)
	}
	p, redirect, err := d.Locate(ctx, "c1/2026-11/ab.png", time.Hour, "")
	if err != nil || redirect != "" {
		t.Fatalf("locate: %v %q", err, redirect)
	}
	if b, _ := os.ReadFile(p); string(b) != "x" {
		t.Fatal("nội dung sai")
	}
	if _, _, err := d.Locate(ctx, "../../etc/passwd", time.Hour, ""); err == nil {
		t.Fatal("khoá ../ phải bị chặn")
	}
	if err := d.Put(ctx, "../thoat.txt", []byte("x"), ""); err == nil {
		t.Fatal("ghi ra ngoài thư mục gốc phải bị chặn")
	}
	if err := d.Delete(ctx, "c1/2026-11/ab.png"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Locate(ctx, "c1/2026-11/ab.png", time.Hour, ""); err != ErrNotFound {
		t.Fatalf("sau khi xoá: %v", err)
	}
	if d.FreeBytes() <= 0 {
		t.Fatal("không đọc được chỗ trống ổ đĩa")
	}
}

// Supabase giả: bucket chưa có → tạo private đúng một lần; tải lên kèm apikey;
// ký URL trả URL đầy đủ, có tên file khi ép tải về.
func TestSupabase_BucketUploadSign(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	bucketExists := false
	var created map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := r.Method + " " + r.URL.Path
		calls[key]++
		if r.Header.Get("apikey") != "sb_secret_test" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/storage/v1/bucket/chat":
			if !bucketExists {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"statusCode":"404","error":"Bucket not found"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"chat"}`)
		case r.Method == "POST" && r.URL.Path == "/storage/v1/bucket":
			_ = json.NewDecoder(r.Body).Decode(&created)
			bucketExists = true
			_, _ = io.WriteString(w, `{"name":"chat"}`)
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/storage/v1/object/sign/chat/"):
			_, _ = io.WriteString(w, `{"signedURL":"/object/sign/chat/c1/a%20b.pdf?token=TKN"}`)
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/storage/v1/object/chat/"):
			if r.Header.Get("x-upsert") != "true" || r.Header.Get("Content-Type") != "application/pdf" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"Key":"chat/c1/a b.pdf"}`)
		case r.Method == "DELETE":
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := NewSupabase(srv.URL+"/", "sb_secret_test", "chat", 20<<20)
	ctx := context.Background()
	if err := s.Put(ctx, "c1/a b.pdf", []byte("%PDF"), "application/pdf"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.Put(ctx, "c1/c.pdf", []byte("%PDF"), "application/pdf"); err != nil {
		t.Fatalf("put 2: %v", err)
	}
	if calls["POST /storage/v1/bucket"] != 1 || calls["GET /storage/v1/bucket/chat"] != 1 {
		t.Fatalf("tạo bucket phải đúng một lần: %v", calls)
	}
	if created["public"] != false || created["id"] != "chat" {
		t.Fatalf("bucket phải private: %v", created)
	}
	_, u, err := s.Locate(ctx, "c1/a b.pdf", time.Hour, "Báo giá.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, srv.URL+"/storage/v1/object/sign/chat/") || !strings.Contains(u, "token=TKN") ||
		!strings.Contains(u, "download=B%C3%A1o+gi%C3%A1.pdf") {
		t.Fatalf("signed URL = %s", u)
	}
	if err := s.Delete(ctx, "c1/a b.pdf"); err != nil {
		t.Fatal(err)
	}

	// Bucket đã có sẵn (khởi động lại API): không tạo lại.
	s2 := NewSupabase(srv.URL, "sb_secret_test", "chat", 20<<20)
	if err := s2.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	if calls["POST /storage/v1/bucket"] != 1 {
		t.Fatal("bucket có rồi mà vẫn tạo lại")
	}
	// Khoá sai: báo lỗi rõ, không in khoá.
	s3 := NewSupabase(srv.URL, "sai", "chat", 1)
	err = s3.EnsureBucket(ctx)
	if err == nil || strings.Contains(err.Error(), "sai") {
		t.Fatalf("khoá sai: %v", err)
	}
}
