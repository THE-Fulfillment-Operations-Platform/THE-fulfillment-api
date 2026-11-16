package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// Chat nội bộ — lớp HTTP. Nghiệp vụ và luật quyền nằm ở services.ChatService.

func (h *Handlers) chat() *services.ChatService { return h.svc.Chat }

// ChatEnabled: có service chat không (router dựng trong test không có chat thì
// bỏ qua các route chat).
func (h *Handlers) ChatEnabled() bool { return h.svc != nil && h.svc.Chat != nil }

// ChatConfig: GET /api/chat/config — 403 nếu chat chưa mở cho tài khoản này.
func (h *Handlers) ChatConfig(c *gin.Context) {
	d, err := h.chat().Config(actor(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, d)
}

// ChatTicket: POST /api/chat/ws-ticket — vé dùng một lần để mở WebSocket.
func (h *Handlers) ChatTicket(c *gin.Context) {
	t, err := h.chat().IssueTicket(actor(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"ticket": t, "expires_in": 60})
}

// ChatSocket: GET /api/chat/ws?ticket=… — nâng cấp lên WebSocket. Nằm ngoài nhóm
// JWT vì trình duyệt không gắn được header vào WebSocket; vé là giấy phép.
func (h *Handlers) ChatSocket(allowedOrigins []string) gin.HandlerFunc {
	allowAll := false
	allowed := map[string]bool{}
	for _, o := range allowedOrigins {
		if o == "*" {
			allowAll = true
		}
		allowed[strings.TrimRight(o, "/")] = true
	}
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" || allowAll || allowed[origin] {
				return true
			}
			// Cùng nguồn sau Caddy (web và API chung một domain).
			u, err := url.Parse(origin)
			return err == nil && strings.EqualFold(u.Host, r.Host)
		},
	}
	return func(c *gin.Context) {
		userID, ok := h.chat().Hub().RedeemTicket(c.Query("ticket"))
		if !ok {
			response.Fail(c, apperr.Unauthorized("Vé WebSocket hết hạn hoặc không hợp lệ"))
			return
		}
		if err := h.chat().AuthorizeSocket(userID); err != nil {
			response.Fail(c, err)
			return
		}
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return // Upgrade đã tự trả lỗi HTTP
		}
		h.chat().Hub().Serve(conn, userID)
	}
}

// ChatDirectory: GET /api/chat/users?q=
func (h *Handlers) ChatDirectory(c *gin.Context) {
	rows, err := h.chat().Directory(actor(c), c.Query("q"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, rows)
}

// ChatListConversations: GET /api/chat/conversations
func (h *Handlers) ChatListConversations(c *gin.Context) {
	rows, err := h.chat().ListConversations(actor(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, rows)
}

// ChatGetConversation: GET /api/chat/conversations/:id
func (h *Handlers) ChatGetConversation(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	d, err := h.chat().GetConversation(actor(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, d)
}

// ChatOpenDirect: POST /api/chat/conversations {user_id}
func (h *Handlers) ChatOpenDirect(c *gin.Context) {
	var in struct {
		UserID uint `json:"user_id" binding:"required"`
	}
	if !bindJSON(c, &in) {
		return
	}
	d, err := h.chat().OpenDirect(actor(c), in.UserID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, d)
}

// ChatMarkRead: POST /api/chat/conversations/:id/read {message_id}
func (h *Handlers) ChatMarkRead(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in struct {
		MessageID uint `json:"message_id"`
	}
	if !bindJSON(c, &in) {
		return
	}
	if err := h.chat().MarkRead(actor(c), id, in.MessageID); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"ok": true})
}

func queryUint(c *gin.Context, name string) uint {
	v, _ := strconv.ParseUint(c.Query(name), 10, 64)
	return uint(v)
}

// ChatListMessages: GET /api/chat/conversations/:id/messages?before_id=&after_id=&limit=
func (h *Handlers) ChatListMessages(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	rows, more, err := h.chat().ListMessages(actor(c), id, services.MessageQuery{
		BeforeID: queryUint(c, "before_id"), AfterID: queryUint(c, "after_id"), Limit: limit,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"messages": rows, "has_more": more})
}

// ChatSendMessage: POST /api/chat/conversations/:id/messages {body, client_id}
func (h *Handlers) ChatSendMessage(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.SendInput
	if !bindJSON(c, &in) {
		return
	}
	d, err := h.chat().SendMessage(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, d)
}

// ---------- đợt 2 (#5453): nhóm, file, sửa/thu hồi, cảm xúc, ghim, tìm, hiện diện ----------

// ChatCreateGroup: POST /api/chat/groups {title, user_ids}
func (h *Handlers) ChatCreateGroup(c *gin.Context) {
	var in struct {
		Title   string `json:"title"`
		UserIDs []uint `json:"user_ids"`
	}
	if !bindJSON(c, &in) {
		return
	}
	d, err := h.chat().CreateGroup(actor(c), in.Title, in.UserIDs)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, d)
}

// ChatRenameGroup: PATCH /api/chat/conversations/:id {title}
func (h *Handlers) ChatRenameGroup(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in struct {
		Title string `json:"title"`
	}
	if !bindJSON(c, &in) {
		return
	}
	d, err := h.chat().RenameGroup(actor(c), id, in.Title)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, d)
}

// ChatAddMembers: POST /api/chat/conversations/:id/members {user_ids}
func (h *Handlers) ChatAddMembers(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in struct {
		UserIDs []uint `json:"user_ids"`
	}
	if !bindJSON(c, &in) {
		return
	}
	d, err := h.chat().AddMembers(actor(c), id, in.UserIDs)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, d)
}

// ChatRemoveMember: DELETE /api/chat/conversations/:id/members/:userId
// (userId = chính mình là rời nhóm).
func (h *Handlers) ChatRemoveMember(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	uid, ok := uintParam(c, "userId")
	if !ok {
		return
	}
	if err := h.chat().RemoveMember(actor(c), id, uid); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"ok": true})
}

// ChatPin: POST /api/chat/conversations/:id/pin {pinned}
func (h *Handlers) ChatPin(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in struct {
		Pinned bool `json:"pinned"`
	}
	if !bindJSON(c, &in) {
		return
	}
	at, err := h.chat().SetPinned(actor(c), id, in.Pinned)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"pinned_at": at})
}

// ChatEditMessage: PATCH /api/chat/messages/:id {body}
func (h *Handlers) ChatEditMessage(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if !bindJSON(c, &in) {
		return
	}
	d, err := h.chat().EditMessage(actor(c), id, in.Body)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, d)
}

// ChatRecallMessage: DELETE /api/chat/messages/:id — thu hồi.
func (h *Handlers) ChatRecallMessage(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	d, err := h.chat().RecallMessage(actor(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, d)
}

// ChatReact: POST /api/chat/messages/:id/reactions {emoji}
func (h *Handlers) ChatReact(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in struct {
		Emoji string `json:"emoji"`
	}
	if !bindJSON(c, &in) {
		return
	}
	d, err := h.chat().React(actor(c), id, in.Emoji)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"reactions": d})
}

// ChatSearch: GET /api/chat/search?q=&conversation_id=
func (h *Handlers) ChatSearch(c *gin.Context) {
	rows, err := h.chat().Search(actor(c), c.Query("q"), queryUint(c, "conversation_id"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, rows)
}

// ChatPresence: GET /api/chat/presence
func (h *Handlers) ChatPresence(c *gin.Context) {
	d, err := h.chat().Presence(actor(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, d)
}

// ChatUpload: POST /api/chat/conversations/:id/files (multipart, trường "file").
func (h *Handlers) ChatUpload(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	max := h.chat().FileMaxBytes()
	// Dư 1 MB cho phần khung multipart; file quá cỡ thì đọc dừng ở đây, không
	// kéo cả trăm MB vào RAM.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max+(1<<20))
	fh, err := c.FormFile("file")
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) || strings.Contains(err.Error(), "request body too large") {
			response.Fail(c, apperr.New(http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE",
				fmt.Sprintf("File quá %d MB", max>>20)))
			return
		}
		response.Fail(c, apperr.BadRequest("Thiếu file (trường \"file\")"))
		return
	}
	if fh.Size > max {
		response.Fail(c, apperr.New(http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE",
			fmt.Sprintf("File quá %d MB", max>>20)))
		return
	}
	f, err := fh.Open()
	if err != nil {
		response.Fail(c, apperr.BadRequest("Không đọc được file"))
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		response.Fail(c, apperr.BadRequest("Không đọc được file"))
		return
	}
	d, err := h.chat().UploadFile(actor(c), id, fh.Filename, data)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, d)
}

// ChatFile: GET /api/chat/files/:id?v=&e=&s=[&dl=1] — phát file chat. Không qua
// JWT vì là src của <img>/<iframe>; URL ký có hạn là giấy phép. Sai gì cũng 404.
func (h *Handlers) ChatFile(c *gin.Context) {
	f, err := h.chat().OpenFile(c.Param("id"), c.Query("v"), c.Query("e"), c.Query("s"), c.Query("dl") == "1")
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if f.Redirect != "" {
		c.Header("Cache-Control", "private, max-age=600")
		c.Redirect(http.StatusFound, f.Redirect)
		return
	}
	disp := "attachment"
	if f.Inline {
		disp = "inline"
	}
	c.Header("Content-Disposition", fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`,
		disp, asciiFileName(f.Name), url.PathEscape(f.Name)))
	c.Header("Content-Type", f.Mime)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, max-age=43200")
	c.Header("ETag", f.ETag)
	if f.Mime != "application/pdf" {
		// File người dùng tải lên không bao giờ được chạy như trang của mình.
		c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox")
	}
	c.File(f.LocalPath)
}

// asciiFileName: tên file cho trình duyệt cũ (bỏ dấu, bỏ ký tự lạ).
func asciiFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 0x20 && r < 0x7f && r != '"' && r != '\\' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
