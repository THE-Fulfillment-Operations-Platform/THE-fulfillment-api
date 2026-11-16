package services

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/chathub"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

type chatFixture struct {
	db  *gorm.DB
	svc *ChatService
	// tài khoản
	ktv1, ktv2, ops, sellerA1, sellerA2, sellerB Actor
}

func newChatFixture(t *testing.T, roles string, allowSeller bool) *chatFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Seller{}, &models.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.AutoMigrate(models.ChatModels()...); err != nil {
		t.Fatalf("migrate chat: %v", err)
	}
	sA := &models.Seller{Code: "SA", Name: "Seller A"}
	sB := &models.Seller{Code: "SB", Name: "Seller B"}
	for _, s := range []*models.Seller{sA, sB} {
		if err := db.Create(s).Error; err != nil {
			t.Fatalf("seller: %v", err)
		}
	}
	mk := func(email, name string, role models.Role, sellerID *uint) Actor {
		u := &models.User{Email: email, FullName: name, PasswordHash: "x", Role: role, SellerID: sellerID, IsActive: true}
		if err := db.Create(u).Error; err != nil {
			t.Fatalf("user %s: %v", email, err)
		}
		return Actor{ID: u.ID, Email: email, Role: role, SellerID: sellerID}
	}
	f := &chatFixture{db: db}
	f.ktv1 = mk("ktv1@x", "Kỹ Thuật Một", models.RoleTechnician, nil)
	f.ktv2 = mk("ktv2@x", "Kỹ Thuật Hai", models.RoleTechnician, nil)
	f.ops = mk("ops@x", "Vận Hành", models.RoleOps, nil)
	f.sellerA1 = mk("a1@x", "Seller A1", models.RoleSeller, &sA.ID)
	f.sellerA2 = mk("a2@x", "Seller A2", models.RoleSeller, &sA.ID)
	f.sellerB = mk("b@x", "Seller B", models.RoleSeller, &sB.ID)
	f.svc = NewChatService(repositories.New(db), chathub.New(), ChatOptions{
		Roles: ParseChatRoles(roles), AllowSeller: allowSeller,
	})
	return f
}

func wantChatStatus(t *testing.T, err error, status int, what string) {
	t.Helper()
	var ae *apperr.Error
	if err == nil || !errors.As(err, &ae) || ae.Status != status {
		t.Fatalf("%s: err = %v, want HTTP %d", what, err, status)
	}
}

func directoryIDs(t *testing.T, s *ChatService, a Actor) map[uint]bool {
	t.Helper()
	rows, err := s.Directory(a, "")
	if err != nil {
		t.Fatalf("directory: %v", err)
	}
	m := map[uint]bool{}
	for _, r := range rows {
		m[r.ID] = true
	}
	return m
}

func TestParseChatRoles(t *testing.T) {
	r := ParseChatRoles("technician, ops ,SELLER,KHONGCO")
	if !r[models.RoleTechnician] || !r[models.RoleOps] || len(r) != 2 {
		t.Fatalf("roles = %v, want TECHNICIAN + OPS (SELLER và vai trò lạ bị bỏ)", r)
	}
	all := ParseChatRoles("ALL")
	if all[models.RoleSeller] || !all[models.RoleCS] || !all[models.RoleOwner] {
		t.Fatalf("ALL = %v, want mọi vai trò nội bộ, không có SELLER", all)
	}
	if len(ParseChatRoles("NONE")) != 0 {
		t.Fatal("NONE phải tắt hết")
	}
}

// Mặc định (Nghĩa #5370): chỉ Kỹ thuật viên dùng chat. Vai trò khác và seller bị
// 403 ở mọi cửa, và không hiện trong danh bạ của KTV.
func TestChat_OnlyConfiguredRoles(t *testing.T) {
	f := newChatFixture(t, "TECHNICIAN", false)
	s := f.svc

	if _, err := s.Config(f.ktv1); err != nil {
		t.Fatalf("KTV config: %v", err)
	}
	for name, a := range map[string]Actor{"ops": f.ops, "seller": f.sellerA1} {
		_, err := s.Config(a)
		wantChatStatus(t, err, http.StatusForbidden, name+" config")
		_, err = s.Directory(a, "")
		wantChatStatus(t, err, http.StatusForbidden, name+" directory")
		_, err = s.ListConversations(a)
		wantChatStatus(t, err, http.StatusForbidden, name+" conversations")
		_, err = s.IssueTicket(a)
		wantChatStatus(t, err, http.StatusForbidden, name+" ws ticket")
		_, err = s.OpenDirect(a, f.ktv1.ID)
		wantChatStatus(t, err, http.StatusForbidden, name+" open direct")
	}
	if err := s.AuthorizeSocket(f.ops.ID); err == nil {
		t.Fatal("vé WS của OPS phải bị từ chối lúc mở kết nối")
	}

	ids := directoryIDs(t, s, f.ktv1)
	if !ids[f.ktv2.ID] || ids[f.ops.ID] || ids[f.sellerA1.ID] || ids[f.ktv1.ID] {
		t.Fatalf("danh bạ KTV = %v, want chỉ KTV khác", ids)
	}
	// KTV không mở được chat với người chưa được bật.
	_, err := s.OpenDirect(f.ktv1, f.ops.ID)
	wantChatStatus(t, err, http.StatusNotFound, "KTV → OPS")

	// Mở thêm vai trò bằng cấu hình là dùng được ngay, không sửa code.
	s.opts.Roles = ParseChatRoles("TECHNICIAN,OPS")
	if _, err := s.OpenDirect(f.ktv1, f.ops.ID); err != nil {
		t.Fatalf("sau khi mở OPS: %v", err)
	}
}

// Bật seller: seller chỉ thấy nội bộ + đồng nghiệp cùng seller, không thấy seller khác.
func TestChat_SellerIsolation(t *testing.T) {
	f := newChatFixture(t, "ALL", true)
	s := f.svc

	ids := directoryIDs(t, s, f.sellerA1)
	if !ids[f.ops.ID] || !ids[f.ktv1.ID] || !ids[f.sellerA2.ID] {
		t.Fatalf("seller A1 phải thấy nội bộ + seller A2: %v", ids)
	}
	if ids[f.sellerB.ID] {
		t.Fatal("seller A1 KHÔNG được thấy seller B")
	}
	rows, _ := s.Directory(f.sellerA1, "")
	for _, r := range rows {
		if r.Email != "" {
			t.Fatalf("seller không được thấy email: %+v", r)
		}
	}
	if rows, _ := s.Directory(f.sellerA1, "Seller B"); len(rows) != 0 {
		t.Fatalf("tìm 'Seller B' từ seller A trả %d dòng", len(rows))
	}
	if ids := directoryIDs(t, s, f.ops); !ids[f.sellerA1.ID] || !ids[f.sellerB.ID] {
		t.Fatal("nội bộ phải thấy mọi seller khi bật")
	}
	_, err := s.OpenDirect(f.sellerA1, f.sellerB.ID)
	wantChatStatus(t, err, http.StatusNotFound, "seller A1 → seller B")
	c, err := s.OpenDirect(f.sellerA1, f.ops.ID)
	if err != nil {
		t.Fatalf("seller → ops: %v", err)
	}
	// Seller B không đọc / gửi được vào cuộc của người khác.
	_, _, err = s.ListMessages(f.sellerB, c.ID, MessageQuery{})
	wantChatStatus(t, err, http.StatusNotFound, "seller B đọc cuộc của A1")
	_, err = s.SendMessage(f.sellerB, c.ID, SendInput{Body: "chen vào"})
	wantChatStatus(t, err, http.StatusNotFound, "seller B gửi vào cuộc của A1")

	// Tắt seller: seller mất quyền ngay, nội bộ không gửi tới seller được nữa.
	s.opts.AllowSeller = false
	_, _, err = s.ListMessages(f.sellerA1, c.ID, MessageQuery{})
	wantChatStatus(t, err, http.StatusForbidden, "seller khi đã tắt")
	_, err = s.SendMessage(f.ops, c.ID, SendInput{Body: "còn đó không"})
	wantChatStatus(t, err, http.StatusForbidden, "ops gửi tới seller khi đã tắt")
}

func findConv(t *testing.T, s *ChatService, a Actor, id uint) ChatConversationDTO {
	t.Helper()
	list, err := s.ListConversations(a)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, c := range list {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("user %d không thấy cuộc %d", a.ID, id)
	return ChatConversationDTO{}
}

// Số chưa đọc: đếm đúng, người gửi không tự có tin chưa đọc, mở cuộc (đánh dấu
// đã đọc) thì về 0, mốc đọc không lùi.
func TestChat_UnreadAndMarkRead(t *testing.T) {
	f := newChatFixture(t, "TECHNICIAN", false)
	s := f.svc
	c, err := s.OpenDirect(f.ktv1, f.ktv2.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if c.Peer.ID != f.ktv2.ID {
		t.Fatalf("peer = %d, want ktv2", c.Peer.ID)
	}
	c2, err := s.OpenDirect(f.ktv2, f.ktv1.ID)
	if err != nil || c2.ID != c.ID {
		t.Fatalf("mở lại phải ra cùng cuộc: %v / %d vs %d", err, c2.ID, c.ID)
	}
	var last *models.ChatMessage
	for _, b := range []string{"một", "hai", "ba"} {
		if last, err = s.SendMessage(f.ktv1, c.ID, SendInput{Body: b}); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	if got := findConv(t, s, f.ktv2, c.ID); got.Unread != 3 || got.LastMessage == nil || got.LastMessage.Body != "ba" {
		t.Fatalf("ktv2: unread=%d last=%+v, want 3 / 'ba'", got.Unread, got.LastMessage)
	}
	if got := findConv(t, s, f.ktv1, c.ID).Unread; got != 0 {
		t.Fatalf("người gửi unread = %d, want 0", got)
	}
	if err := s.MarkRead(f.ktv2, c.ID, last.ID-1); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if got := findConv(t, s, f.ktv2, c.ID).Unread; got != 1 {
		t.Fatalf("đọc tới tin 2: unread = %d, want 1", got)
	}
	_ = s.MarkRead(f.ktv2, c.ID, 1) // không lùi
	if got := findConv(t, s, f.ktv2, c.ID); got.MyLastRead != last.ID-1 {
		t.Fatalf("mốc đọc bị lùi: %d", got.MyLastRead)
	}
	_ = s.MarkRead(f.ktv2, c.ID, 999999) // mốc quá tin cuối bị kẹp lại
	if got := findConv(t, s, f.ktv2, c.ID); got.Unread != 0 || got.MyLastRead != last.ID {
		t.Fatalf("đọc hết: unread=%d lastRead=%d want 0/%d", got.Unread, got.MyLastRead, last.ID)
	}
}

// Gửi tin: trống / quá dài bị chặn; gửi lại cùng client_id chỉ ra một tin; tải
// tin theo trang và theo after_id (kéo bù sau khi rớt mạng).
func TestChat_SendAndPaging(t *testing.T) {
	f := newChatFixture(t, "TECHNICIAN", false)
	s := f.svc
	c, _ := s.OpenDirect(f.ktv1, f.ktv2.ID)

	_, err := s.SendMessage(f.ktv1, c.ID, SendInput{Body: "   \n "})
	wantChatStatus(t, err, http.StatusBadRequest, "tin trống")
	_, err = s.SendMessage(f.ktv1, c.ID, SendInput{Body: strings.Repeat("a", chatMaxBody+1)})
	wantChatStatus(t, err, http.StatusUnprocessableEntity, "tin quá dài")

	a, err := s.SendMessage(f.ktv1, c.ID, SendInput{Body: "x", ClientID: "abc-1"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	b, err := s.SendMessage(f.ktv1, c.ID, SendInput{Body: "x", ClientID: "abc-1"})
	if err != nil || b.ID != a.ID {
		t.Fatalf("gửi lại cùng client_id ra tin mới: %v %d vs %d", err, b.ID, a.ID)
	}
	for i := 0; i < 50; i++ {
		if _, err := s.SendMessage(f.ktv2, c.ID, SendInput{Body: "tin"}); err != nil {
			t.Fatal(err)
		}
	}
	page, more, err := s.ListMessages(f.ktv1, c.ID, MessageQuery{})
	if err != nil || len(page) != chatPageDefault || !more {
		t.Fatalf("trang cuối: %d tin, more=%v, err=%v", len(page), more, err)
	}
	if page[0].ID >= page[len(page)-1].ID {
		t.Fatal("tin phải theo thứ tự cũ → mới")
	}
	older, more, _ := s.ListMessages(f.ktv1, c.ID, MessageQuery{BeforeID: page[0].ID})
	if len(older) != 51-chatPageDefault || more {
		t.Fatalf("trang cũ hơn: %d tin, more=%v", len(older), more)
	}
	after, _, _ := s.ListMessages(f.ktv1, c.ID, MessageQuery{AfterID: page[len(page)-3].ID})
	if len(after) != 2 {
		t.Fatalf("after_id: %d tin, want 2", len(after))
	}
	_, _, err = s.ListMessages(f.ops, c.ID, MessageQuery{})
	wantChatStatus(t, err, http.StatusForbidden, "OPS đọc tin khi chưa mở chat")
}

// WebSocket thật qua hub: tin mới tới cả hai bên ngay, người ngoài cuộc không
// nhận; vé dùng một lần; đánh dấu đã đọc báo sang tab khác của chính người đó.
func TestChat_WebSocketDelivery(t *testing.T) {
	f := newChatFixture(t, "TECHNICIAN,OPS", false)
	s := f.svc
	c, _ := s.OpenDirect(f.ktv1, f.ktv2.ID)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, ok := s.Hub().RedeemTicket(r.URL.Query().Get("ticket"))
		if !ok {
			http.Error(w, "ticket", http.StatusUnauthorized)
			return
		}
		if err := s.AuthorizeSocket(uid); err != nil {
			http.Error(w, "auth", http.StatusForbidden)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.Hub().Serve(conn, uid)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/?ticket="

	dial := func(a Actor) *websocket.Conn {
		tk, err := s.IssueTicket(a)
		if err != nil {
			t.Fatalf("ticket: %v", err)
		}
		conn, _, err := websocket.DefaultDialer.Dial(wsURL+tk, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return conn
	}
	next := func(conn *websocket.Conn, typ string) map[string]interface{} {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("chờ %q: %v", typ, err)
			}
			var ev map[string]interface{}
			_ = json.Unmarshal(data, &ev)
			if ev["type"] == typ {
				return ev
			}
		}
	}
	k2 := dial(f.ktv2)
	defer k2.Close()
	next(k2, "hello")
	k1a := dial(f.ktv1)
	defer k1a.Close()
	next(k1a, "hello")
	k1b := dial(f.ktv1) // tab thứ hai của ktv1
	defer k1b.Close()
	next(k1b, "hello")
	outsider := dial(f.ops)
	defer outsider.Close()
	next(outsider, "hello")

	// Vé dùng một lần.
	tk, _ := s.IssueTicket(f.ktv1)
	s.Hub().RedeemTicket(tk)
	if _, resp, err := websocket.DefaultDialer.Dial(wsURL+tk, nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("vé đã dùng vẫn vào được")
	}

	sent, err := s.SendMessage(f.ktv1, c.ID, SendInput{Body: "xin chào qua websocket", ClientID: "c-1"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	for name, conn := range map[string]*websocket.Conn{"ktv2": k2, "ktv1 tab A": k1a, "ktv1 tab B": k1b} {
		ev := next(conn, "message.new")
		msg := ev["message"].(map[string]interface{})
		if msg["body"] != "xin chào qua websocket" || msg["client_id"] != "c-1" {
			t.Fatalf("%s nhận %v", name, msg)
		}
	}
	// Người ngoài cuộc không nhận gì (ping để chắc hàng đợi đã rỗng).
	_ = outsider.WriteJSON(map[string]string{"type": "ping"})
	_ = outsider.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, data, err := outsider.ReadMessage()
		if err != nil {
			t.Fatalf("outsider: %v", err)
		}
		var ev map[string]interface{}
		_ = json.Unmarshal(data, &ev)
		if ev["type"] == "pong" {
			break
		}
		if ev["type"] == "message.new" {
			t.Fatal("người ngoài cuộc nhận được tin")
		}
	}

	// ktv2 đọc ở một tab → chính ktv2 nhận "read"; ktv1 thì không (không có "đã xem").
	k2b := dial(f.ktv2)
	defer k2b.Close()
	next(k2b, "hello")
	if err := s.MarkRead(f.ktv2, c.ID, sent.ID); err != nil {
		t.Fatal(err)
	}
	if ev := next(k2b, "read"); uint(ev["message_id"].(float64)) != sent.ID {
		t.Fatalf("read = %v", ev)
	}
	_ = k1a.WriteJSON(map[string]string{"type": "ping"})
	_ = k1a.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, data, err := k1a.ReadMessage()
		if err != nil {
			t.Fatalf("k1a: %v", err)
		}
		var ev map[string]interface{}
		_ = json.Unmarshal(data, &ev)
		if ev["type"] == "pong" {
			break
		}
		if ev["type"] == "read" {
			t.Fatal("người gửi không được nhận sự kiện đã đọc của người kia")
		}
	}
}
