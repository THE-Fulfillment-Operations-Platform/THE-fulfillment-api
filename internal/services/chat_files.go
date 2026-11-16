package services

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/chatfiles"
	"the-fulfillment/backend/internal/models"
)

// Gửi ảnh và file trong chat (#5453). Luồng: trình duyệt tải từng file lên
// (POST /chat/conversations/:id/files) → nhận id → gửi tin kèm attachment_ids.
// File nằm trong kho (đĩa server hoặc Supabase Storage, xem chatfiles); trình
// duyệt xem/tải qua URL ký HMAC có hạn (/api/chat/files/:id?…), kho Supabase thì
// URL đó chuyển tiếp sang signed URL của Supabase.

// chatFileKinds: đuôi file được nhận → nhóm. "image"/"pdf" xem được ngay trong
// trình duyệt (sau khi kiểm nội dung thật đúng loại); còn lại chỉ tải về.
var chatFileKinds = map[string]string{
	".jpg": "image", ".jpeg": "image", ".png": "image", ".gif": "image", ".webp": "image",
	".pdf": "pdf",
	".doc": "file", ".docx": "file", ".xls": "file", ".xlsx": "file", ".csv": "file", ".txt": "file",
	".ppt": "file", ".pptx": "file", ".zip": "file", ".rar": "file", ".7z": "file",
	".ai": "file", ".psd": "file", ".cdr": "file", ".dxf": "file", ".svg": "file", ".eps": "file",
	".heic": "file", ".mp4": "file", ".mov": "file",
}

// chatImageMimes: kiểu ảnh trình duyệt hiện trực tiếp (kiểm bằng nội dung, không tin đuôi).
var chatImageMimes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

func isImageMime(m string) bool { return chatImageMimes[m] }

// chatThumbPx: ảnh lớn hơn thì dựng thêm bản nhỏ cho khung chat.
const chatThumbPx = 1280

var errChatFileSig = errors.New("invalid chat file request")

// FilesEnabled: có kho file không.
func (s *ChatService) FilesEnabled() bool { return s.opts.Files != nil }

// FileMaxBytes: cỡ tối đa một file.
func (s *ChatService) FileMaxBytes() int64 { return s.opts.FileMaxBytes }

func cleanFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "." || name == "" {
		name = "file"
	}
	return truncateRunes(name, 180)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// UploadFile nhận một file cho cuộc convID (chưa gắn vào tin nào).
func (s *ChatService) UploadFile(a Actor, convID uint, filename string, data []byte) (*ChatAttachmentDTO, error) {
	if _, _, err := s.requireMember(a, convID); err != nil {
		return nil, err
	}
	if s.opts.Files == nil {
		return nil, apperr.Forbidden("Chưa bật gửi file trong chat")
	}
	if int64(len(data)) > s.opts.FileMaxBytes {
		return nil, apperr.New(http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE",
			fmt.Sprintf("File quá %d MB", s.opts.FileMaxBytes>>20))
	}
	if len(data) == 0 {
		return nil, apperr.BadRequest("File rỗng")
	}
	name := cleanFileName(filename)
	ext := strings.ToLower(filepath.Ext(name))
	kind, ok := chatFileKinds[ext]
	if !ok {
		return nil, apperr.New(http.StatusUnsupportedMediaType, "FILE_TYPE",
			"Loại file này không gửi được trong chat (nhận ảnh, PDF, Word/Excel/PowerPoint, file nén, file thiết kế)")
	}
	sniffed := http.DetectContentType(data)
	if i := strings.IndexByte(sniffed, ';'); i >= 0 {
		sniffed = sniffed[:i]
	}
	mimeType := mime.TypeByExtension(ext)
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = mimeType[:i]
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	switch kind {
	case "image":
		if !chatImageMimes[sniffed] {
			return nil, apperr.New(http.StatusUnsupportedMediaType, "FILE_TYPE", "File không phải ảnh thật (đuôi "+ext+")")
		}
		mimeType = sniffed
	case "pdf":
		if sniffed != "application/pdf" {
			return nil, apperr.New(http.StatusUnsupportedMediaType, "FILE_TYPE", "File không phải PDF thật")
		}
		mimeType = "application/pdf"
	}
	if free := s.opts.Files.FreeBytes(); free >= 0 && free-int64(len(data)) < s.opts.MinFreeBytes {
		chatLog("từ chối file: ổ đĩa còn %d MB", free>>20)
		return nil, apperr.New(http.StatusInsufficientStorage, "STORAGE_FULL",
			"Ổ đĩa máy chủ sắp đầy, chưa nhận file được — báo kỹ thuật")
	}

	now := s.now()
	base := fmt.Sprintf("c%d/%s/%s", convID, now.Format("2006-01"), randHex(12))
	att := &models.ChatAttachment{
		ConversationID: convID, UploaderID: a.ID, StorageKey: base + ext,
		Name: name, Mime: mimeType, Size: int64(len(data)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if kind == "image" {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			att.Width, att.Height = cfg.Width, cfg.Height
		}
		// Ảnh lớn: thêm bản JPEG nhỏ cho khung chat (GIF giữ nguyên để còn động).
		big := att.Width > chatThumbPx || att.Height > chatThumbPx || len(data) > 600<<10
		if mimeType != "image/gif" && big {
			if thumb, err := shrinkToJPEG(data, chatThumbPx); err == nil {
				if err := s.opts.Files.Put(ctx, base+".thumb.jpg", thumb, "image/jpeg"); err == nil {
					att.ThumbKey = base + ".thumb.jpg"
				}
			}
		}
	}
	if err := s.opts.Files.Put(ctx, att.StorageKey, data, mimeType); err != nil {
		chatLog("lưu file lỗi (%s): %v", s.opts.Files.Kind(), err)
		if att.ThumbKey != "" {
			_ = s.opts.Files.Delete(ctx, att.ThumbKey)
		}
		return nil, apperr.Internal("Không lưu được file, thử lại sau")
	}
	if err := s.repo.Chat.CreateAttachment(att); err != nil {
		_ = s.opts.Files.Delete(ctx, att.StorageKey)
		if att.ThumbKey != "" {
			_ = s.opts.Files.Delete(ctx, att.ThumbKey)
		}
		return nil, apperr.Internal("could not save file").Wrap(err)
	}
	d := s.attachmentDTO(*att)
	return &d, nil
}

// ---------- URL ký ----------

func (s *ChatService) fileSig(id uint, variant string, exp int64) string {
	mac := hmac.New(sha256.New, s.opts.FileSecret)
	fmt.Fprintf(mac, "%d|%s|%d", id, variant, exp)
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// fileURL: URL ký có hạn. Hạn làm tròn lên mốc giờ để URL đứng yên trong một
// giờ — trình duyệt dùng lại ảnh đã tải thay vì tải lại mỗi lần làm mới danh sách.
func (s *ChatService) fileURL(id uint, variant string, download bool) string {
	exp := s.now().Add(s.opts.FileURLTTL).Truncate(time.Hour).Add(time.Hour).Unix()
	u := fmt.Sprintf("/api/chat/files/%d?v=%s&e=%d&s=%s", id, variant, exp, s.fileSig(id, variant, exp))
	if download {
		u += "&dl=1"
	}
	return u
}

func (s *ChatService) attachmentDTO(a models.ChatAttachment) ChatAttachmentDTO {
	d := ChatAttachmentDTO{
		ID: a.ID, Name: a.Name, Mime: a.Mime, Size: a.Size, IsImage: isImageMime(a.Mime),
		Width: a.Width, Height: a.Height,
		URL: s.fileURL(a.ID, "full", false), DownloadURL: s.fileURL(a.ID, "full", true),
	}
	if d.IsImage {
		if a.ThumbKey != "" {
			d.ThumbURL = s.fileURL(a.ID, "thumb", false)
		} else {
			d.ThumbURL = d.URL
		}
	}
	return d
}

// ChatFileServe: cách handler phát một file.
type ChatFileServe struct {
	LocalPath string
	Redirect  string
	Mime      string
	Name      string
	Inline    bool
	ETag      string
}

// OpenFile kiểm URL ký rồi cho biết phát file nào. Mọi lỗi là cùng một lỗi mờ
// (endpoint không cần đăng nhập, không giải thích cho người dò).
func (s *ChatService) OpenFile(idParam, variant, expParam, sig string, download bool) (*ChatFileServe, error) {
	if s.opts.Files == nil {
		return nil, errChatFileSig
	}
	id64, err := strconv.ParseUint(idParam, 10, 64)
	if err != nil || id64 == 0 || (variant != "full" && variant != "thumb") {
		return nil, errChatFileSig
	}
	exp, err := strconv.ParseInt(expParam, 10, 64)
	if err != nil || s.now().Unix() > exp {
		return nil, errChatFileSig
	}
	if !hmac.Equal([]byte(s.fileSig(uint(id64), variant, exp)), []byte(sig)) {
		return nil, errChatFileSig
	}
	att, err := s.repo.Chat.AttachmentByID(uint(id64))
	if err != nil {
		return nil, errChatFileSig
	}
	key, mimeType := att.StorageKey, att.Mime
	if variant == "thumb" && att.ThumbKey != "" {
		key, mimeType = att.ThumbKey, "image/jpeg"
	}
	inline := !download && (isImageMime(mimeType) || mimeType == "application/pdf")
	dlName := ""
	if !inline {
		dlName = att.Name
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	path, redirect, err := s.opts.Files.Locate(ctx, key, time.Hour, dlName)
	if err != nil {
		if !errors.Is(err, chatfiles.ErrNotFound) {
			chatLog("mở file %d lỗi: %v", att.ID, err)
		}
		return nil, errChatFileSig
	}
	return &ChatFileServe{
		LocalPath: path, Redirect: redirect, Mime: mimeType, Name: att.Name, Inline: inline,
		ETag: fmt.Sprintf(`"cf%d-%s"`, att.ID, variant),
	}, nil
}

// ---------- dọn file ----------

func (s *ChatService) deleteStored(ctx context.Context, a models.ChatAttachment) {
	if s.opts.Files == nil {
		return
	}
	if err := s.opts.Files.Delete(ctx, a.StorageKey); err != nil {
		chatLog("xoá file %d lỗi: %v", a.ID, err)
	}
	if a.ThumbKey != "" {
		_ = s.opts.Files.Delete(ctx, a.ThumbKey)
	}
}

// SweepOrphanFiles xoá file tải lên mà không gửi trong 24 giờ.
func (s *ChatService) SweepOrphanFiles() int {
	rows, err := s.repo.Chat.OrphanAttachments(s.now().Add(-24*time.Hour), 500)
	if err != nil {
		chatLog("dọn file lỗi: %v", err)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	n := 0
	for _, a := range rows {
		s.deleteStored(ctx, a)
		if s.repo.Chat.DeleteAttachment(a.ID) == nil {
			n++
		}
	}
	if n > 0 {
		chatLog("đã dọn %d file tải lên mà không gửi", n)
	}
	return n
}

// StartJanitor chạy dọn file mồ côi mỗi giờ tới khi ctx xong.
func (s *ChatService) StartJanitor(ctx context.Context) {
	if s.opts.Files == nil {
		return
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.SweepOrphanFiles()
			}
		}
	}()
}
