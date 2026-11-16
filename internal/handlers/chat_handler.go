package handlers

import (
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
