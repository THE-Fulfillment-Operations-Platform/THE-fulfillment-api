package chatfiles

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Supabase lưu file trong một bucket PRIVATE của Supabase Storage, gọi thẳng REST
// API (không cần SDK). Service key chỉ nằm trong header của các lời gọi này.
type Supabase struct {
	base     string // https://<ref>.supabase.co
	key      string
	bucket   string
	maxBytes int64
	client   *http.Client

	mu      sync.Mutex
	ensured bool
}

// NewSupabase tạo kho Supabase. Bucket được tạo ở lần dùng đầu (EnsureBucket).
func NewSupabase(baseURL, serviceKey, bucket string, maxBytes int64) *Supabase {
	if bucket == "" {
		bucket = "chat"
	}
	return &Supabase{
		base:     strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		key:      strings.TrimSpace(serviceKey),
		bucket:   bucket,
		maxBytes: maxBytes,
		client:   &http.Client{Timeout: 60 * time.Second},
	}
}

func (s *Supabase) Kind() string { return "supabase" }

// Bucket: tên bucket (để log).
func (s *Supabase) Bucket() string { return s.bucket }

func (s *Supabase) FreeBytes() int64 { return -1 }

func (s *Supabase) do(ctx context.Context, method, path string, body io.Reader, contentType string, extra map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.base+"/storage/v1"+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("apikey", s.key)
	// Khoá kiểu cũ (JWT service_role) đi kèm Authorization; khoá mới sb_secret_…
	// thì cổng Supabase tự đổi từ header apikey.
	if strings.HasPrefix(s.key, "eyJ") {
		req.Header.Set("Authorization", "Bearer "+s.key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		// Lỗi mạng của net/http có URL nhưng không có header — không lộ khoá.
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, b, nil
}

// objectPath: /<bucket>/<key> với từng đoạn được escape.
func (s *Supabase) objectPath(key string) string {
	parts := strings.Split(strings.TrimLeft(key, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/" + url.PathEscape(s.bucket) + "/" + strings.Join(parts, "/")
}

// EnsureBucket tạo bucket private nếu chưa có. Chạy lại bao nhiêu lần cũng được.
func (s *Supabase) EnsureBucket(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ensured {
		return nil
	}
	code, _, err := s.do(ctx, http.MethodGet, "/bucket/"+url.PathEscape(s.bucket), nil, "", nil)
	if err != nil {
		return fmt.Errorf("supabase storage: %w", err)
	}
	if code == http.StatusOK {
		s.ensured = true
		return nil
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return fmt.Errorf("supabase storage: khoá bị từ chối (HTTP %d)", code)
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"id": s.bucket, "name": s.bucket, "public": false, "file_size_limit": s.maxBytes,
	})
	code, body, err := s.do(ctx, http.MethodPost, "/bucket", bytes.NewReader(payload), "application/json", nil)
	if err != nil {
		return fmt.Errorf("supabase storage: %w", err)
	}
	// Hai tiến trình cùng tạo: bên chậm chân nhận "already exists" — coi là xong.
	if code/100 == 2 || code == http.StatusConflict || bytes.Contains(bytes.ToLower(body), []byte("already exists")) {
		s.ensured = true
		return nil
	}
	return fmt.Errorf("supabase storage: tạo bucket lỗi HTTP %d", code)
}

func (s *Supabase) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := s.EnsureBucket(ctx); err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	code, _, err := s.do(ctx, http.MethodPost, "/object"+s.objectPath(key), bytes.NewReader(data), contentType,
		map[string]string{"x-upsert": "true", "cache-control": "3600"})
	if err != nil {
		return fmt.Errorf("supabase storage: %w", err)
	}
	if code/100 != 2 {
		return fmt.Errorf("supabase storage: tải lên lỗi HTTP %d", code)
	}
	return nil
}

func (s *Supabase) Delete(ctx context.Context, key string) error {
	code, _, err := s.do(ctx, http.MethodDelete, "/object"+s.objectPath(key), nil, "", nil)
	if err != nil {
		return fmt.Errorf("supabase storage: %w", err)
	}
	if code/100 != 2 && code != http.StatusNotFound && code != http.StatusBadRequest {
		return fmt.Errorf("supabase storage: xoá lỗi HTTP %d", code)
	}
	return nil
}

func (s *Supabase) Locate(ctx context.Context, key string, ttl time.Duration, downloadName string) (string, string, error) {
	secs := int(ttl / time.Second)
	if secs < 60 {
		secs = 60
	}
	payload, _ := json.Marshal(map[string]int{"expiresIn": secs})
	code, body, err := s.do(ctx, http.MethodPost, "/object/sign"+s.objectPath(key), bytes.NewReader(payload), "application/json", nil)
	if err != nil {
		return "", "", fmt.Errorf("supabase storage: %w", err)
	}
	if code == http.StatusNotFound || code == http.StatusBadRequest {
		return "", "", ErrNotFound
	}
	if code/100 != 2 {
		return "", "", fmt.Errorf("supabase storage: ký URL lỗi HTTP %d", code)
	}
	var out struct {
		SignedURL string `json:"signedURL"`
		SignedUrl string `json:"signedUrl"` //nolint:revive // Supabase trả một trong hai tên tuỳ bản
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", fmt.Errorf("supabase storage: phản hồi ký URL lạ")
	}
	signed := out.SignedURL
	if signed == "" {
		signed = out.SignedUrl
	}
	if signed == "" {
		return "", "", fmt.Errorf("supabase storage: thiếu signedURL")
	}
	full := signed
	if !strings.HasPrefix(signed, "http") {
		full = s.base + "/storage/v1" + signed
	}
	if downloadName != "" {
		sep := "&"
		if !strings.Contains(full, "?") {
			sep = "?"
		}
		full += sep + "download=" + url.QueryEscape(downloadName)
	}
	return "", full, nil
}
