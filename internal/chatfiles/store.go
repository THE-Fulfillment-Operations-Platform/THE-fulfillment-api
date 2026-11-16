// Package chatfiles là kho file của chat nội bộ (ảnh, PDF, tài liệu gửi trong chat).
//
// Hai loại kho, chọn lúc khởi động (cmd/server/chat.go):
//   - Supabase Storage: khi env có SUPABASE_URL + SUPABASE_SERVICE_ROLE_KEY. File
//     nằm trong bucket PRIVATE "chat" (tự tạo lần đầu, chạy lại không sao); trình
//     duyệt nhận signed URL có hạn của Supabase.
//   - Đĩa server: không có khoá Supabase. File nằm dưới CHAT_FILES_DIR (trong
//     docker là volume chat_files), API tự phát file qua URL ký HMAC có hạn.
//
// Cả hai đều không bao giờ lộ khoá: service key chỉ nằm trong header gọi từ API
// sang Supabase, không in ra log, không gửi về trình duyệt.
package chatfiles

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound: không có file theo khoá này.
var ErrNotFound = errors.New("chatfiles: not found")

// Store là một kho file.
type Store interface {
	// Kind: "disk" hoặc "supabase" (để log và báo cáo, không chứa bí mật).
	Kind() string
	// Put ghi (đè) một file.
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// Delete xoá một file; không có thì thôi.
	Delete(ctx context.Context, key string) error
	// Locate cho biết cách phát file: đường dẫn trên đĩa (kho đĩa) hoặc URL ký
	// sẵn có hạn ttl (kho Supabase). downloadName != "" thì URL ép tải về.
	Locate(ctx context.Context, key string, ttl time.Duration, downloadName string) (localPath, redirectURL string, err error)
	// FreeBytes: dung lượng còn trống (kho đĩa); -1 = không biết / không áp dụng.
	FreeBytes() int64
}
