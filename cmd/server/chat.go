package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"log"
	"sort"
	"strings"
	"time"

	"the-fulfillment/backend/internal/chatfiles"
	"the-fulfillment/backend/internal/chathub"
	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/services"
)

// newChatService dựng chat nội bộ: hub WebSocket trong RAM (một instance API) và
// kho file (Supabase Storage nếu env có khoá, không thì đĩa server).
func newChatService(cfg *config.Config, repo *repositories.Repositories) *services.ChatService {
	roles := services.ParseChatRoles(cfg.ChatRoles)
	names := make([]string, 0, len(roles))
	for r := range roles {
		names = append(names, string(r))
	}
	sort.Strings(names)
	log.Printf("chat: mở cho vai trò [%s], seller=%v", strings.Join(names, ","), cfg.ChatAllowSeller)

	maxBytes := int64(cfg.ChatFileMaxMB) << 20
	// Khoá ký URL file rút từ JWT secret (như ảnh thu nhỏ QC): không thêm bí mật
	// mới; đổi JWT secret thì URL file cũ hết hiệu lực — đúng ý, chúng là giấy phép.
	fileKey := hmac.New(sha256.New, []byte(cfg.JWTSecret))
	fileKey.Write([]byte("ffm-chat-file-url-v1"))

	return services.NewChatService(repo, chathub.New(), services.ChatOptions{
		Roles:        roles,
		AllowSeller:  cfg.ChatAllowSeller,
		Files:        newChatStore(cfg, maxBytes),
		FileMaxBytes: maxBytes,
		FileSecret:   fileKey.Sum(nil),
		FileURLTTL:   time.Duration(cfg.ChatFileURLTTLH) * time.Hour,
		MinFreeBytes: int64(cfg.ChatMinFreeMB) << 20,
	})
}

// newChatStore chọn kho file chat. Không in khoá ra log.
func newChatStore(cfg *config.Config, maxBytes int64) chatfiles.Store {
	mode := cfg.ChatStorage
	haveSupabase := cfg.SupabaseURL != "" && cfg.SupabaseKey != ""
	if mode == "off" || mode == "none" {
		log.Printf("chat: gửi file TẮT (CHAT_STORAGE=%s)", mode)
		return nil
	}
	if mode == "supabase" || (mode == "auto" && haveSupabase) {
		if !haveSupabase {
			log.Printf("chat: CHAT_STORAGE=supabase nhưng thiếu SUPABASE_URL / SUPABASE_SERVICE_ROLE_KEY — dùng đĩa server")
		} else {
			sb := chatfiles.NewSupabase(cfg.SupabaseURL, cfg.SupabaseKey, cfg.ChatFileBucket, maxBytes)
			// Tạo bucket private ngay lúc khởi động (chạy lại không sao); lỗi thì lần
			// tải file đầu tiên tự thử lại.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := sb.EnsureBucket(ctx); err != nil {
					log.Printf("chat: kho Supabase chưa sẵn sàng: %v", err)
					return
				}
				log.Printf("chat: file lưu ở Supabase Storage, bucket private %q", sb.Bucket())
			}()
			return sb
		}
	}
	d, err := chatfiles.NewDisk(cfg.ChatFilesDir)
	if err != nil {
		log.Printf("chat: gửi file TẮT — %v", err)
		return nil
	}
	log.Printf("chat: file lưu trên đĩa server ở %s (còn trống %d MB)", d.Dir(), d.FreeBytes()>>20)
	return d
}
