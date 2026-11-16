package services

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/chathub"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Chat nội bộ: nhắn tin chữ 1-1, realtime qua WebSocket (Nghĩa #5355 → thu phạm
// vi #5369 → chỉ bật cho Kỹ thuật viên trước #5370; 16/11/2026).
//
// Ai dùng được chat:
//   - Tài khoản nội bộ có vai trò nằm trong CHAT_ROLES (mặc định chỉ TECHNICIAN;
//     "ALL" = mọi vai trò nội bộ). Mở thêm vai trò = sửa env, không sửa code.
//   - Seller chỉ khi CHAT_ALLOW_SELLER=true (mặc định tắt), và chỉ nhắn được với
//     tài khoản nội bộ: không thấy, không nhắn được seller khác. Hai tài khoản của
//     CÙNG một seller (cùng seller_id) coi là một bên.
// Ai không dùng được: mọi endpoint chat trả 403, danh bạ của người khác không
// hiện người đó.

const (
	chatMaxBody     = 4000
	chatPageDefault = 40
	chatPageMax     = 100
)

// ChatOptions cấu hình chat.
type ChatOptions struct {
	// Roles: vai trò NỘI BỘ được dùng chat. SELLER trong đây bị bỏ qua — seller
	// do AllowSeller quyết.
	Roles       map[models.Role]bool
	AllowSeller bool
}

// ChatService là nghiệp vụ chat.
type ChatService struct {
	repo *repositories.Repositories
	hub  *chathub.Hub
	opts ChatOptions
	now  func() time.Time
}

// NewChatService dựng service chat.
func NewChatService(repo *repositories.Repositories, hub *chathub.Hub, opts ChatOptions) *ChatService {
	if hub == nil {
		hub = chathub.New()
	}
	return &ChatService{repo: repo, hub: hub, opts: opts, now: time.Now}
}

// Hub cho handler WebSocket.
func (s *ChatService) Hub() *chathub.Hub { return s.hub }

// ParseChatRoles đọc CHAT_ROLES ("TECHNICIAN,OPS" / "ALL"). Vai trò lạ và SELLER bị bỏ.
func ParseChatRoles(raw string) map[models.Role]bool {
	out := map[models.Role]bool{}
	for _, p := range strings.Split(raw, ",") {
		r := models.Role(strings.ToUpper(strings.TrimSpace(p)))
		if r == "" || r == models.RoleSeller {
			continue
		}
		for _, ar := range models.AllRoles {
			if ar == models.RoleSeller {
				continue
			}
			if r == "ALL" || r == "*" || ar == r {
				out[ar] = true
			}
		}
	}
	return out
}

// ---------- ai dùng được, ai thấy ai ----------

type chatUser = repositories.ChatUserRow

func isSellerUser(u chatUser) bool { return u.Role == models.RoleSeller }

// sellerKey: một bên seller. Seller không gắn công ty nào thì là một bên riêng.
func sellerKey(u chatUser) string {
	if u.SellerID != nil && *u.SellerID != 0 {
		return fmt.Sprintf("s%d", *u.SellerID)
	}
	return fmt.Sprintf("u%d", u.ID)
}

// RoleEnabled: vai trò này có được dùng chat không.
func (s *ChatService) RoleEnabled(r models.Role) bool {
	if r == models.RoleSeller {
		return s.opts.AllowSeller
	}
	return s.opts.Roles[r]
}

// canSee: viewer có thấy / nhắn được target không.
func (s *ChatService) canSee(viewer, target chatUser) bool {
	if !target.IsActive || !s.RoleEnabled(target.Role) || !s.RoleEnabled(viewer.Role) {
		return false
	}
	if isSellerUser(viewer) && isSellerUser(target) {
		return sellerKey(viewer) == sellerKey(target)
	}
	return true
}

// ensureEnabled: 403 cho tài khoản chưa được mở chat.
func (s *ChatService) ensureEnabled(a Actor) error {
	if a.ID == 0 {
		return apperr.Unauthorized("Authentication required")
	}
	if !s.RoleEnabled(a.Role) {
		return apperr.Forbidden("Chat chưa mở cho tài khoản này")
	}
	return nil
}

func (s *ChatService) loadUsers(ids []uint) (map[uint]chatUser, error) {
	rows, err := s.repo.Chat.UsersByIDs(uniqueUints(ids))
	if err != nil {
		return nil, apperr.Internal("could not load users").Wrap(err)
	}
	out := make(map[uint]chatUser, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

// viewerOf: thông tin người gọi, với vai trò MỚI NHẤT (LoadAccess) thay cho bản
// đọc từ bảng users.
func (s *ChatService) viewerOf(a Actor, users map[uint]chatUser) chatUser {
	u, ok := users[a.ID]
	if !ok {
		u = chatUser{ID: a.ID, SellerID: a.SellerID, IsActive: true}
	}
	u.Role = a.Role
	return u
}

// ---------- DTO ----------

// ChatConfigDTO: web dùng để biết có hiện chat không.
type ChatConfigDTO struct {
	Enabled bool `json:"enabled"`
}

// ChatUserDTO là một người trong danh bạ / người bên kia của cuộc.
type ChatUserDTO struct {
	ID         uint        `json:"id"`
	FullName   string      `json:"full_name"`
	Email      string      `json:"email,omitempty"`
	Role       models.Role `json:"role"`
	SellerName string      `json:"seller_name,omitempty"`
}

// ChatConversationDTO là một cuộc trong danh sách.
type ChatConversationDTO struct {
	ID            uint                `json:"id"`
	Peer          ChatUserDTO         `json:"peer"`
	LastMessage   *models.ChatMessage `json:"last_message,omitempty"`
	LastMessageAt time.Time           `json:"last_message_at"`
	Unread        int                 `json:"unread"`
	MyLastRead    uint                `json:"my_last_read"`
}

func userDTO(viewer, u chatUser) ChatUserDTO {
	d := ChatUserDTO{ID: u.ID, FullName: u.FullName, Role: u.Role}
	if strings.TrimSpace(d.FullName) == "" {
		d.FullName = u.Email
	}
	// Seller không cần (và không nên) thấy email nội bộ.
	if !isSellerUser(viewer) {
		d.Email = u.Email
		d.SellerName = u.SellerName
	}
	return d
}

// ---------- cấu hình & danh bạ ----------

// Config: 403 nếu chưa mở chat cho người gọi (web ẩn hẳn chat).
func (s *ChatService) Config(a Actor) (ChatConfigDTO, error) {
	if err := s.ensureEnabled(a); err != nil {
		return ChatConfigDTO{}, err
	}
	return ChatConfigDTO{Enabled: true}, nil
}

// Directory: những người người gọi nhắn được (trừ chính mình).
func (s *ChatService) Directory(a Actor, q string) ([]ChatUserDTO, error) {
	if err := s.ensureEnabled(a); err != nil {
		return nil, err
	}
	users, err := s.loadUsers([]uint{a.ID})
	if err != nil {
		return nil, err
	}
	viewer := s.viewerOf(a, users)
	rows, err := s.repo.Chat.ActiveUsers(q, 500)
	if err != nil {
		return nil, apperr.Internal("could not list users").Wrap(err)
	}
	out := make([]ChatUserDTO, 0, len(rows))
	for _, r := range rows {
		if r.ID != viewer.ID && s.canSee(viewer, r) {
			out = append(out, userDTO(viewer, r))
		}
	}
	return out, nil
}

// ---------- cuộc trò chuyện ----------

func directKey(a, b uint) string {
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("%d:%d", a, b)
}

// requireMember: người gọi dùng được chat và ở trong cuộc này.
func (s *ChatService) requireMember(a Actor, convID uint) (*models.ChatConversation, *models.ChatMember, error) {
	if err := s.ensureEnabled(a); err != nil {
		return nil, nil, err
	}
	m, err := s.repo.Chat.Member(convID, a.ID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, apperr.NotFound("Không tìm thấy cuộc trò chuyện")
		}
		return nil, nil, apperr.Internal("could not load membership").Wrap(err)
	}
	c, err := s.repo.Chat.ConversationByID(convID)
	if err != nil {
		return nil, nil, apperr.NotFound("Không tìm thấy cuộc trò chuyện")
	}
	return c, m, nil
}

func (s *ChatService) memberIDs(convID uint) ([]uint, error) {
	rows, err := s.repo.Chat.MembersOf([]uint{convID})
	if err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.UserID)
	}
	return ids, nil
}

// ListConversations: các cuộc của người gọi, mới nhất trước.
func (s *ChatService) ListConversations(a Actor) ([]ChatConversationDTO, error) {
	if err := s.ensureEnabled(a); err != nil {
		return nil, err
	}
	mine, err := s.repo.Chat.MyMemberships(a.ID)
	if err != nil {
		return nil, apperr.Internal("could not list conversations").Wrap(err)
	}
	ids := make([]uint, 0, len(mine))
	for _, m := range mine {
		ids = append(ids, m.ConversationID)
	}
	out, err := s.buildConversations(a, ids)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastMessageAt.After(out[j].LastMessageAt) })
	return out, nil
}

// GetConversation: một cuộc (người gọi phải ở trong).
func (s *ChatService) GetConversation(a Actor, id uint) (*ChatConversationDTO, error) {
	if _, _, err := s.requireMember(a, id); err != nil {
		return nil, err
	}
	out, err := s.buildConversations(a, []uint{id})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, apperr.NotFound("Không tìm thấy cuộc trò chuyện")
	}
	return &out[0], nil
}

func (s *ChatService) buildConversations(a Actor, ids []uint) ([]ChatConversationDTO, error) {
	out := []ChatConversationDTO{}
	if len(ids) == 0 {
		return out, nil
	}
	convs, err := s.repo.Chat.ConversationsByIDs(ids)
	if err != nil {
		return nil, apperr.Internal("could not load conversations").Wrap(err)
	}
	members, err := s.repo.Chat.MembersOf(ids)
	if err != nil {
		return nil, apperr.Internal("could not load members").Wrap(err)
	}
	userIDs := []uint{a.ID}
	peerOf := map[uint]uint{}
	myRead := map[uint]uint{}
	for _, m := range members {
		if m.UserID == a.ID {
			myRead[m.ConversationID] = m.LastReadMessageID
		} else {
			peerOf[m.ConversationID] = m.UserID
			userIDs = append(userIDs, m.UserID)
		}
	}
	users, err := s.loadUsers(userIDs)
	if err != nil {
		return nil, err
	}
	viewer := s.viewerOf(a, users)
	unread, err := s.repo.Chat.UnreadCounts(a.ID)
	if err != nil {
		return nil, apperr.Internal("could not count unread").Wrap(err)
	}
	var lastIDs []uint
	for _, c := range convs {
		if c.LastMessageID > 0 {
			lastIDs = append(lastIDs, c.LastMessageID)
		}
	}
	lasts, err := s.repo.Chat.MessagesByIDs(lastIDs)
	if err != nil {
		return nil, apperr.Internal("could not load last messages").Wrap(err)
	}
	lastBy := map[uint]models.ChatMessage{}
	for _, m := range lasts {
		lastBy[m.ID] = m
	}
	for _, c := range convs {
		peer, ok := users[peerOf[c.ID]]
		if !ok {
			peer = chatUser{ID: peerOf[c.ID], FullName: "Tài khoản đã xoá"}
		}
		d := ChatConversationDTO{
			ID: c.ID, Peer: userDTO(viewer, peer), LastMessageAt: c.LastMessageAt,
			Unread: unread[c.ID], MyLastRead: myRead[c.ID],
		}
		if m, ok := lastBy[c.LastMessageID]; ok {
			m := m
			d.LastMessage = &m
		}
		out = append(out, d)
	}
	return out, nil
}

// OpenDirect trả cuộc 1-1 với userID, tạo mới nếu chưa có.
func (s *ChatService) OpenDirect(a Actor, userID uint) (*ChatConversationDTO, error) {
	if err := s.ensureEnabled(a); err != nil {
		return nil, err
	}
	if userID == 0 || userID == a.ID {
		return nil, apperr.BadRequest("Chọn một người khác để nhắn")
	}
	users, err := s.loadUsers([]uint{a.ID, userID})
	if err != nil {
		return nil, err
	}
	other, ok := users[userID]
	if !ok || !s.canSee(s.viewerOf(a, users), other) {
		return nil, apperr.NotFound("Không tìm thấy người dùng")
	}
	key := directKey(a.ID, userID)
	if c, err := s.repo.Chat.ConversationByKey(key); err == nil {
		return s.GetConversation(a, c.ID)
	}
	now := s.now()
	conv := &models.ChatConversation{DirectKey: key, CreatedByID: a.ID, LastMessageAt: now}
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		r := repositories.New(tx).Chat
		if err := r.CreateConversation(conv); err != nil {
			return err
		}
		for _, id := range []uint{a.ID, userID} {
			if err := r.CreateMember(&models.ChatMember{ConversationID: conv.ID, UserID: id, JoinedAt: now}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Hai người cùng bấm một lúc: bên chậm chân đụng unique direct_key.
		if c, err2 := s.repo.Chat.ConversationByKey(key); err2 == nil {
			return s.GetConversation(a, c.ID)
		}
		return nil, apperr.Internal("could not create conversation").Wrap(err)
	}
	return s.GetConversation(a, conv.ID)
}

// MarkRead: người gọi đã đọc tới messageID (mốc không bao giờ lùi). Các tab
// khác của chính người đó nhận sự kiện "read" để xoá số chưa đọc theo.
func (s *ChatService) MarkRead(a Actor, convID, messageID uint) error {
	c, m, err := s.requireMember(a, convID)
	if err != nil {
		return err
	}
	if messageID > c.LastMessageID {
		messageID = c.LastMessageID
	}
	if messageID <= m.LastReadMessageID {
		return nil
	}
	now := s.now()
	m.LastReadMessageID, m.LastReadAt = messageID, &now
	if err := s.repo.Chat.SaveMember(m); err != nil {
		return apperr.Internal("could not mark read").Wrap(err)
	}
	s.hub.SendJSON([]uint{a.ID}, map[string]interface{}{
		"type": "read", "conversation_id": convID, "message_id": messageID,
	})
	return nil
}

// ---------- tin nhắn ----------

// MessageQuery chọn khúc tin cần lấy.
type MessageQuery struct {
	BeforeID uint
	AfterID  uint
	Limit    int
}

// ListMessages trả tin theo thứ tự cũ → mới, kèm hasMore (còn tin cũ hơn).
// after_id dùng để kéo bù tin lỡ khi WebSocket rớt.
func (s *ChatService) ListMessages(a Actor, convID uint, q MessageQuery) ([]models.ChatMessage, bool, error) {
	if _, _, err := s.requireMember(a, convID); err != nil {
		return nil, false, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = chatPageDefault
	}
	if limit > chatPageMax {
		limit = chatPageMax
	}
	if q.AfterID > 0 {
		rows, err := s.repo.Chat.MessagesAfter(convID, q.AfterID, chatPageMax)
		if err != nil {
			return nil, false, apperr.Internal("could not list messages").Wrap(err)
		}
		return rows, false, nil
	}
	rows, err := s.repo.Chat.MessagesBefore(convID, q.BeforeID, limit+1)
	if err != nil {
		return nil, false, apperr.Internal("could not list messages").Wrap(err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, hasMore, nil
}

// SendInput là nội dung một lần gửi.
type SendInput struct {
	Body     string `json:"body"`
	ClientID string `json:"client_id"`
}

// SendMessage gửi một tin chữ vào cuộc và đẩy realtime cho cả hai bên.
func (s *ChatService) SendMessage(a Actor, convID uint, in SendInput) (*models.ChatMessage, error) {
	conv, _, err := s.requireMember(a, convID)
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(strings.ReplaceAll(in.Body, "\r\n", "\n"))
	if body == "" {
		return nil, apperr.BadRequest("Tin nhắn trống")
	}
	if utf8.RuneCountInString(body) > chatMaxBody {
		return nil, apperr.Unprocessable(fmt.Sprintf("Tin nhắn dài quá %d ký tự", chatMaxBody))
	}
	clientID := strings.TrimSpace(in.ClientID)
	if len(clientID) > 64 {
		clientID = clientID[:64]
	}
	if clientID != "" {
		if old, err := s.repo.Chat.MessageByClientID(a.ID, clientID); err == nil {
			return old, nil // gửi lại vì mạng chập chờn: trả đúng tin cũ
		}
	}
	ids, err := s.memberIDs(convID)
	if err != nil {
		return nil, apperr.Internal("could not load members").Wrap(err)
	}
	users, err := s.loadUsers(ids)
	if err != nil {
		return nil, err
	}
	me := s.viewerOf(a, users)
	for _, id := range ids {
		if id != a.ID && !s.canSee(me, users[id]) {
			return nil, apperr.Forbidden("Người này hiện không nhận được tin nhắn")
		}
	}
	msg := &models.ChatMessage{ConversationID: convID, SenderID: a.ID, Body: body}
	if clientID != "" {
		msg.ClientID = &clientID
	}
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		r := repositories.New(tx).Chat
		if err := r.CreateMessage(msg); err != nil {
			return err
		}
		conv.LastMessageID, conv.LastMessageAt = msg.ID, msg.CreatedAt
		if err := r.SaveConversation(conv); err != nil {
			return err
		}
		// Người gửi hiển nhiên đã đọc tin của mình.
		m, err := r.Member(convID, a.ID)
		if err != nil {
			return err
		}
		m.LastReadMessageID, m.LastReadAt = msg.ID, &msg.CreatedAt
		return r.SaveMember(m)
	})
	if err != nil {
		if clientID != "" {
			if old, err2 := s.repo.Chat.MessageByClientID(a.ID, clientID); err2 == nil {
				return old, nil
			}
		}
		return nil, apperr.Internal("could not send message").Wrap(err)
	}
	s.hub.SendJSON(ids, map[string]interface{}{"type": "message.new", "message": msg})
	return msg, nil
}

// ---------- WebSocket ----------

// IssueTicket cấp vé WebSocket cho người gọi.
func (s *ChatService) IssueTicket(a Actor) (string, error) {
	if err := s.ensureEnabled(a); err != nil {
		return "", err
	}
	return s.hub.IssueTicket(a.ID), nil
}

// AuthorizeSocket kiểm lại người dùng của một vé ngay lúc mở WebSocket (tài
// khoản vừa bị khoá / đổi vai trò trong 60 giây hạn vé thì không vào được).
func (s *ChatService) AuthorizeSocket(userID uint) error {
	users, err := s.loadUsers([]uint{userID})
	if err != nil {
		return err
	}
	u, ok := users[userID]
	if !ok || !u.IsActive {
		return apperr.Unauthorized("Tài khoản đã bị khoá")
	}
	if !s.RoleEnabled(u.Role) {
		return apperr.Forbidden("Chat chưa mở cho tài khoản này")
	}
	return nil
}

// ---------- tiện ích ----------

func uniqueUints(in []uint) []uint {
	seen := make(map[uint]bool, len(in))
	out := make([]uint, 0, len(in))
	for _, v := range in {
		if v == 0 || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
