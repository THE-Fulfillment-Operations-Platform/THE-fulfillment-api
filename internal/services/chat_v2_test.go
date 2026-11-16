package services

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"the-fulfillment/backend/internal/chatfiles"
	"the-fulfillment/backend/internal/models"
)

// Chat đợt 2 (#5453): quyền mặc định (nội bộ, seller tắt), nhóm, thu hồi / sửa
// 15 phút, cảm xúc, đã xem, file (cỡ, loại, URL ký), trả lời, ghim, tìm, mã đơn.

// v2Fixture: mặc định đúng như production sau #5453 (CHAT_ROLES=ALL, seller tắt),
// thêm chủ xưởng (OWNER) và kho file trên đĩa tạm.
type v2Fixture struct {
	*chatFixture
	owner, cs Actor
	dir       string
}

func newV2Fixture(t *testing.T) *v2Fixture {
	t.Helper()
	f := newChatFixture(t, "ALL", false)
	mk := func(email, name string, role models.Role) Actor {
		u := &models.User{Email: email, FullName: name, PasswordHash: "x", Role: role, IsActive: true}
		if err := f.db.Create(u).Error; err != nil {
			t.Fatalf("user %s: %v", email, err)
		}
		return Actor{ID: u.ID, Email: email, Role: role}
	}
	v := &v2Fixture{chatFixture: f, dir: t.TempDir()}
	v.owner = mk("owner@x", "Chủ Xưởng", models.RoleOwner)
	v.cs = mk("cs@x", "Chăm Sóc", models.RoleCS)
	store, err := chatfiles.NewDisk(v.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.svc.opts.Files = store
	f.svc.opts.MinFreeBytes = 1 // máy test không cần chừa 1 GB
	return v
}

func chatPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.RGBA{200, 30, 30, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func messagesOf(t *testing.T, s *ChatService, a Actor, convID uint) []ChatMessageDTO {
	t.Helper()
	rows, _, err := s.ListMessages(a, convID, MessageQuery{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return rows
}

// Mặc định sau #5453: MỌI vai trò nội bộ dùng chat; seller tắt hẳn — 403 ở mọi
// cửa, kể cả các cửa mới (nhóm, file, tìm, hiện diện), và không có trong danh bạ.
func TestChatV2_InternalOnlyDefault(t *testing.T) {
	v := newV2Fixture(t)
	s := v.svc
	for name, a := range map[string]Actor{"ktv": v.ktv1, "ops": v.ops, "owner": v.owner, "cs": v.cs} {
		if _, err := s.Config(a); err != nil {
			t.Fatalf("%s phải dùng được chat: %v", name, err)
		}
	}
	ids := directoryIDs(t, s, v.ops)
	if !ids[v.ktv1.ID] || !ids[v.owner.ID] || !ids[v.cs.ID] {
		t.Fatalf("danh bạ OPS thiếu người nội bộ: %v", ids)
	}
	if ids[v.sellerA1.ID] || ids[v.sellerB.ID] {
		t.Fatal("seller không được có trong danh bạ khi seller tắt")
	}
	_, err := s.OpenDirect(v.ops, v.sellerA1.ID)
	wantChatStatus(t, err, http.StatusNotFound, "nội bộ mở chat với seller")

	c, _ := s.OpenDirect(v.ops, v.ktv1.ID)
	sel := v.sellerA1
	checks := map[string]error{}
	_, checks["config"] = s.Config(sel)
	_, checks["directory"] = s.Directory(sel, "")
	_, checks["conversations"] = s.ListConversations(sel)
	_, checks["ws ticket"] = s.IssueTicket(sel)
	_, checks["group"] = s.CreateGroup(sel, "Nhóm lén", []uint{v.ops.ID})
	_, checks["upload"] = s.UploadFile(sel, c.ID, "a.png", chatPNG(t, 4, 4))
	_, checks["search"] = s.Search(sel, "chào", 0)
	_, checks["presence"] = s.Presence(sel)
	_, checks["react"] = s.React(sel, 1, "👍")
	_, checks["edit"] = s.EditMessage(sel, 1, "x")
	for name, err := range checks {
		wantChatStatus(t, err, http.StatusForbidden, "seller "+name)
	}
	if err := s.AuthorizeSocket(sel.ID); err == nil {
		t.Fatal("vé WS của seller phải bị từ chối")
	}
}

// Nhóm: tạo, đặt tên, thêm/bớt người, rời nhóm. Người tạo + Owner/Admin (và Kỹ
// thuật viên — HasAdminRights) quản lý thành viên; thành viên thường chỉ đổi tên
// và rời nhóm.
func TestChatV2_Groups(t *testing.T) {
	v := newV2Fixture(t)
	s := v.svc
	_, err := s.CreateGroup(v.ops, "   ", []uint{v.cs.ID})
	wantChatStatus(t, err, http.StatusBadRequest, "nhóm không tên")
	_, err = s.CreateGroup(v.ops, "Tổ in", nil)
	wantChatStatus(t, err, http.StatusBadRequest, "nhóm không người")
	_, err = s.CreateGroup(v.ops, "Tổ in", []uint{v.sellerA1.ID})
	wantChatStatus(t, err, http.StatusUnprocessableEntity, "thêm seller vào nhóm")

	g, err := s.CreateGroup(v.ops, "  Tổ   in UV ", []uint{v.cs.ID, v.ktv1.ID, v.cs.ID})
	if err != nil {
		t.Fatalf("tạo nhóm: %v", err)
	}
	if g.Kind != models.ChatKindGroup || g.Title != "Tổ in UV" || len(g.Members) != 3 || !g.CanManage ||
		g.Peer == nil || g.Peer.ID != 0 || g.Peer.FullName != "Tổ in UV" {
		t.Fatalf("nhóm = %+v", g)
	}
	// Thành viên khác thấy nhóm, có dòng hệ thống "đã tạo nhóm", chưa đọc 1.
	got := findConv(t, s, v.cs, g.ID)
	if got.CanManage || got.Unread != 1 || got.LastMessage == nil || got.LastMessage.Kind != models.ChatMsgSystem ||
		!strings.Contains(got.LastMessage.Body, "đã tạo nhóm") {
		t.Fatalf("cs thấy nhóm = %+v / last %+v", got, got.LastMessage)
	}
	// Ba người nhắn trong nhóm.
	for _, a := range []Actor{v.ops, v.cs, v.ktv1} {
		if _, err := s.SendMessage(a, g.ID, SendInput{Body: "chào nhóm"}); err != nil {
			t.Fatalf("gửi trong nhóm: %v", err)
		}
	}
	// Thành viên thường: không thêm / bớt người được, đổi tên được.
	_, err = s.AddMembers(v.cs, g.ID, []uint{v.ktv2.ID})
	wantChatStatus(t, err, http.StatusForbidden, "thành viên thường thêm người")
	err = s.RemoveMember(v.cs, g.ID, v.ktv1.ID)
	wantChatStatus(t, err, http.StatusForbidden, "thành viên thường bớt người")
	if r, err := s.RenameGroup(v.cs, g.ID, "Tổ in UV ca đêm"); err != nil || r.Title != "Tổ in UV ca đêm" {
		t.Fatalf("đổi tên: %v %+v", err, r)
	}
	// Người tạo thêm chủ xưởng; người mới vào không bị đổ 4 tin cũ thành chưa
	// đọc — chỉ dòng "đã thêm Chủ Xưởng" là mới với họ.
	if _, err := s.AddMembers(v.ops, g.ID, []uint{v.owner.ID}); err != nil {
		t.Fatalf("người tạo thêm người: %v", err)
	}
	if got := findConv(t, s, v.owner, g.ID); got.Unread != 1 || !got.CanManage {
		t.Fatalf("owner vừa vào: unread=%d canManage=%v", got.Unread, got.CanManage)
	}
	// Owner (không phải người tạo) mời ktv1 ra.
	if err := s.RemoveMember(v.owner, g.ID, v.ktv1.ID); err != nil {
		t.Fatalf("owner bớt người: %v", err)
	}
	_, _, err = s.ListMessages(v.ktv1, g.ID, MessageQuery{})
	wantChatStatus(t, err, http.StatusNotFound, "người đã bị mời ra đọc nhóm")
	_, err = s.SendMessage(v.ktv1, g.ID, SendInput{Body: "còn đây không"})
	wantChatStatus(t, err, http.StatusNotFound, "người đã bị mời ra gửi tin")
	// CS tự rời nhóm.
	if err := s.RemoveMember(v.cs, g.ID, v.cs.ID); err != nil {
		t.Fatalf("rời nhóm: %v", err)
	}
	if got := findConv(t, s, v.ops, g.ID); len(got.Members) != 2 {
		t.Fatalf("còn %d người, want 2 (ops + owner)", len(got.Members))
	}
	msgs := messagesOf(t, s, v.ops, g.ID)
	var sys []string
	for _, m := range msgs {
		if m.Kind == models.ChatMsgSystem {
			sys = append(sys, m.Body)
		}
	}
	joined := strings.Join(sys, " | ")
	for _, want := range []string{"đã tạo nhóm", "đổi tên nhóm", "đã thêm Chủ Xưởng", "mời Kỹ Thuật Một ra", "Chăm Sóc đã rời nhóm"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("thiếu dòng hệ thống %q trong %q", want, joined)
		}
	}
	// Cuộc 1-1 không có thao tác nhóm.
	d, _ := s.OpenDirect(v.ktv1, v.ktv2.ID)
	err = s.RemoveMember(v.ktv1, d.ID, v.ktv2.ID)
	wantChatStatus(t, err, http.StatusBadRequest, "bớt người trong cuộc 1-1")
}

// Sửa tin: chỉ người gửi, chỉ trong 15 phút. Thu hồi: chỉ người gửi; chữ, file,
// cảm xúc bị xoá hẳn (kể cả file trong kho).
func TestChatV2_EditAndRecall(t *testing.T) {
	v := newV2Fixture(t)
	s := v.svc
	c, _ := s.OpenDirect(v.ktv1, v.ktv2.ID)
	m, err := s.SendMessage(v.ktv1, c.ID, SendInput{Body: "in 20 cái"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.EditMessage(v.ktv2, m.ID, "in 50 cái")
	wantChatStatus(t, err, http.StatusForbidden, "sửa tin người khác")
	e, err := s.EditMessage(v.ktv1, m.ID, "in 30 cái")
	if err != nil || e.Body != "in 30 cái" || e.EditedAt == nil {
		t.Fatalf("sửa trong 15 phút: %v %+v", err, e)
	}
	// 16 phút sau: bị chặn.
	base := time.Now()
	s.now = func() time.Time { return base.Add(16 * time.Minute) }
	_, err = s.EditMessage(v.ktv1, m.ID, "in 40 cái")
	wantChatStatus(t, err, http.StatusUnprocessableEntity, "sửa quá 15 phút")
	s.now = time.Now

	// Tin có ảnh + cảm xúc → thu hồi.
	att, err := s.UploadFile(v.ktv1, c.ID, "mau.png", chatPNG(t, 8, 8))
	if err != nil {
		t.Fatal(err)
	}
	withFile, err := s.SendMessage(v.ktv1, c.ID, SendInput{Body: "mẫu đây", AttachmentIDs: []uint{att.ID}})
	if err != nil || len(withFile.Attachments) != 1 {
		t.Fatalf("gửi kèm ảnh: %v %+v", err, withFile)
	}
	stored, _ := s.repo.Chat.AttachmentByID(att.ID)
	path := v.dir + "/" + stored.StorageKey
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file chưa nằm trong kho: %v", err)
	}
	if _, err := s.React(v.ktv2, withFile.ID, "👍"); err != nil {
		t.Fatal(err)
	}
	_, err = s.RecallMessage(v.ktv2, withFile.ID)
	wantChatStatus(t, err, http.StatusForbidden, "thu hồi tin người khác")
	r, err := s.RecallMessage(v.ktv1, withFile.ID)
	if err != nil || r.RecalledAt == nil || r.Body != "" || len(r.Attachments) != 0 || len(r.Reactions) != 0 {
		t.Fatalf("thu hồi: %v %+v", err, r)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("thu hồi rồi mà file vẫn nằm trong kho")
	}
	// Người kia cũng chỉ thấy tin đã thu hồi.
	for _, x := range messagesOf(t, s, v.ktv2, c.ID) {
		if x.ID == withFile.ID && (x.RecalledAt == nil || x.Body != "" || len(x.Attachments) != 0) {
			t.Fatalf("ktv2 vẫn thấy nội dung tin đã thu hồi: %+v", x)
		}
	}
	_, err = s.EditMessage(v.ktv1, withFile.ID, "sửa lại")
	wantChatStatus(t, err, http.StatusUnprocessableEntity, "sửa tin đã thu hồi")
	_, err = s.React(v.ktv2, withFile.ID, "❤️")
	wantChatStatus(t, err, http.StatusUnprocessableEntity, "thả cảm xúc tin đã thu hồi")
}

// Cảm xúc: bộ cố định; mỗi người một cảm xúc/tin, bấm lại là bỏ, bấm khác là đổi.
func TestChatV2_Reactions(t *testing.T) {
	v := newV2Fixture(t)
	s := v.svc
	g, _ := s.CreateGroup(v.ktv1, "Nhóm", []uint{v.ktv2.ID, v.ops.ID})
	m, _ := s.SendMessage(v.ktv1, g.ID, SendInput{Body: "xong đơn"})
	_, err := s.React(v.ktv2, m.ID, "🤡")
	wantChatStatus(t, err, http.StatusUnprocessableEntity, "cảm xúc ngoài bộ")
	_, err = s.React(v.cs, m.ID, "👍")
	wantChatStatus(t, err, http.StatusNotFound, "người ngoài nhóm thả cảm xúc")

	_, _ = s.React(v.ktv2, m.ID, "👍")
	out, _ := s.React(v.ops, m.ID, "👍")
	if len(out) != 1 || out[0].Emoji != "👍" || len(out[0].UserIDs) != 2 {
		t.Fatalf("2 người cùng 👍: %+v", out)
	}
	out, _ = s.React(v.ops, m.ID, "❤️") // đổi
	if len(out) != 2 || out[0].Emoji != "👍" || out[1].Emoji != "❤️" {
		t.Fatalf("đổi cảm xúc: %+v", out)
	}
	out, _ = s.React(v.ktv2, m.ID, "👍") // bấm lại = bỏ
	if len(out) != 1 || out[0].Emoji != "❤️" {
		t.Fatalf("bỏ cảm xúc: %+v", out)
	}
	if got := messagesOf(t, s, v.ktv1, g.ID); got[len(got)-1].Reactions[0].UserIDs[0] != v.ops.ID {
		t.Fatalf("tin không mang cảm xúc: %+v", got[len(got)-1])
	}
}

// Đã xem: mốc đọc của từng thành viên nằm trong cuộc (để hiện avatar / "ai đã xem").
func TestChatV2_ReadReceipts(t *testing.T) {
	v := newV2Fixture(t)
	s := v.svc
	g, _ := s.CreateGroup(v.ktv1, "Nhóm", []uint{v.ktv2.ID, v.ops.ID})
	m, _ := s.SendMessage(v.ktv1, g.ID, SendInput{Body: "ai xem chưa"})
	if err := s.MarkRead(v.ktv2, g.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	got := findConv(t, s, v.ktv1, g.ID)
	seen := map[uint]uint{}
	for _, mb := range got.Members {
		seen[mb.ID] = mb.LastReadMessageID
	}
	if seen[v.ktv2.ID] != m.ID || seen[v.ops.ID] >= m.ID || seen[v.ktv1.ID] != m.ID {
		t.Fatalf("mốc đã xem = %v, want ktv2 + ktv1 đã xem tin %d, ops chưa", seen, m.ID)
	}
}

// File: giới hạn cỡ, loại file (đuôi lẫn nội dung thật), URL ký có hạn, gắn vào
// tin đúng một lần, dọn file mồ côi.
func TestChatV2_Upload(t *testing.T) {
	v := newV2Fixture(t)
	s := v.svc
	c, _ := s.OpenDirect(v.ktv1, v.ktv2.ID)
	other, _ := s.OpenDirect(v.ktv1, v.ops.ID)
	img := chatPNG(t, 32, 20)

	s.opts.FileMaxBytes = int64(len(img)) - 1
	_, err := s.UploadFile(v.ktv1, c.ID, "to.png", img)
	wantChatStatus(t, err, http.StatusRequestEntityTooLarge, "file quá cỡ")
	s.opts.FileMaxBytes = 20 << 20

	_, err = s.UploadFile(v.ktv1, c.ID, "virus.exe", []byte("MZ...."))
	wantChatStatus(t, err, http.StatusUnsupportedMediaType, "file .exe")
	_, err = s.UploadFile(v.ktv1, c.ID, "trang.html", []byte("<html></html>"))
	wantChatStatus(t, err, http.StatusUnsupportedMediaType, "file .html")
	_, err = s.UploadFile(v.ktv1, c.ID, "gia.png", []byte("<html><script>alert(1)</script></html>"))
	wantChatStatus(t, err, http.StatusUnsupportedMediaType, "HTML đội lốt .png")
	_, err = s.UploadFile(v.ktv1, c.ID, "gia.pdf", []byte("không phải pdf"))
	wantChatStatus(t, err, http.StatusUnsupportedMediaType, "PDF giả")
	_, err = s.UploadFile(v.ops, c.ID, "a.png", img)
	wantChatStatus(t, err, http.StatusNotFound, "người ngoài cuộc tải file vào cuộc")

	pdf, err := s.UploadFile(v.ktv1, c.ID, "bao gia.pdf", []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n%%EOF"))
	if err != nil || pdf.Mime != "application/pdf" || pdf.IsImage {
		t.Fatalf("PDF: %v %+v", err, pdf)
	}
	a, err := s.UploadFile(v.ktv1, c.ID, "../../anh mau.png", img)
	if err != nil {
		t.Fatalf("ảnh: %v", err)
	}
	if !a.IsImage || a.Name != "anh mau.png" || a.Width != 32 || a.Height != 20 || a.URL == "" || a.ThumbURL == "" {
		t.Fatalf("ảnh = %+v", a)
	}

	// URL ký: đúng thì mở được file thật; sai chữ ký / hết hạn / đổi biến thể thì không.
	q := parseFileURL(t, a.URL)
	f, err := s.OpenFile(q["id"], q["v"], q["e"], q["s"], false)
	if err != nil || !f.Inline || f.Mime != "image/png" {
		t.Fatalf("mở ảnh: %v %+v", err, f)
	}
	if b, _ := os.ReadFile(f.LocalPath); !bytes.Equal(b, img) {
		t.Fatal("file trong kho khác file gửi lên")
	}
	if _, err := s.OpenFile(q["id"], q["v"], q["e"], strings.Repeat("0", 32), false); err == nil {
		t.Fatal("chữ ký sai vẫn mở được")
	}
	if _, err := s.OpenFile(q["id"], "thumb", q["e"], q["s"], false); err == nil {
		t.Fatal("chữ ký của bản gốc dùng cho bản khác được")
	}
	if _, err := s.OpenFile(q["id"], q["v"], "1000", q["s"], false); err == nil {
		t.Fatal("URL hết hạn vẫn mở được")
	}
	if f, _ := s.OpenFile(q["id"], q["v"], q["e"], q["s"], true); f == nil || f.Inline {
		t.Fatal("dl=1 phải là tải về")
	}

	// Gắn vào tin: đúng một lần, đúng cuộc, đúng người tải.
	_, err = s.SendMessage(v.ktv1, other.ID, SendInput{AttachmentIDs: []uint{a.ID}})
	wantChatStatus(t, err, http.StatusUnprocessableEntity, "gắn file của cuộc khác")
	_, err = s.SendMessage(v.ktv2, c.ID, SendInput{AttachmentIDs: []uint{a.ID}})
	wantChatStatus(t, err, http.StatusUnprocessableEntity, "gắn file người khác tải")
	msg, err := s.SendMessage(v.ktv1, c.ID, SendInput{AttachmentIDs: []uint{a.ID, pdf.ID}})
	if err != nil || msg.Body != "" || len(msg.Attachments) != 2 {
		t.Fatalf("gửi 2 file không chữ: %v %+v", err, msg)
	}
	_, err = s.SendMessage(v.ktv1, c.ID, SendInput{AttachmentIDs: []uint{a.ID}})
	wantChatStatus(t, err, http.StatusUnprocessableEntity, "gửi lại file đã gửi")

	// File tải lên mà không gửi: quá 24 giờ thì bị dọn khỏi kho và DB.
	orphan, _ := s.UploadFile(v.ktv1, c.ID, "bo.png", img)
	ostored, _ := s.repo.Chat.AttachmentByID(orphan.ID)
	s.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if n := s.SweepOrphanFiles(); n != 1 {
		t.Fatalf("dọn %d file, want 1 (file đã gửi phải giữ)", n)
	}
	s.now = time.Now
	if _, err := os.Stat(v.dir + "/" + ostored.StorageKey); !os.IsNotExist(err) {
		t.Fatal("file mồ côi vẫn còn trong kho")
	}
	if _, err := s.repo.Chat.AttachmentByID(a.ID); err != nil {
		t.Fatal("file đã gửi bị dọn nhầm")
	}
}

func parseFileURL(t *testing.T, u string) map[string]string {
	t.Helper()
	// /api/chat/files/<id>?v=…&e=…&s=…
	rest := strings.TrimPrefix(u, "/api/chat/files/")
	parts := strings.SplitN(rest, "?", 2)
	out := map[string]string{"id": parts[0]}
	for _, kv := range strings.Split(parts[1], "&") {
		p := strings.SplitN(kv, "=", 2)
		out[p[0]] = p[1]
	}
	return out
}

// Trả lời trích dẫn, ghim, tìm tin.
func TestChatV2_ReplyPinSearch(t *testing.T) {
	v := newV2Fixture(t)
	s := v.svc
	c, _ := s.OpenDirect(v.ktv1, v.ktv2.ID)
	c2, _ := s.OpenDirect(v.ktv1, v.ops.ID)
	q, _ := s.SendMessage(v.ktv2, c.ID, SendInput{Body: "Mẫu đèn núi bản 8 duyệt chưa?"})
	foreign, _ := s.SendMessage(v.ops, c2.ID, SendInput{Body: "tin ở cuộc khác"})
	_, err := s.SendMessage(v.ktv1, c.ID, SendInput{Body: "x", ReplyToID: foreign.ID})
	wantChatStatus(t, err, http.StatusUnprocessableEntity, "trả lời tin cuộc khác")
	r, err := s.SendMessage(v.ktv1, c.ID, SendInput{Body: "Duyệt rồi", ReplyToID: q.ID})
	if err != nil || r.ReplyTo == nil || r.ReplyTo.ID != q.ID || r.ReplyTo.SenderName != "Kỹ Thuật Hai" ||
		!strings.Contains(r.ReplyTo.Body, "đèn núi") {
		t.Fatalf("trả lời: %v %+v", err, r.ReplyTo)
	}
	// Tin gốc bị thu hồi: trích dẫn chỉ còn "đã thu hồi", không lộ chữ cũ.
	if _, err := s.RecallMessage(v.ktv2, q.ID); err != nil {
		t.Fatal(err)
	}
	for _, m := range messagesOf(t, s, v.ktv1, c.ID) {
		if m.ID == r.ID && (m.ReplyTo == nil || !m.ReplyTo.Recalled || m.ReplyTo.Body != "") {
			t.Fatalf("trích dẫn tin đã thu hồi: %+v", m.ReplyTo)
		}
	}

	// Ghim: cuộc cũ hơn nhưng đã ghim nằm trên cùng; bỏ ghim thì về chỗ cũ.
	list, _ := s.ListConversations(v.ktv1)
	if list[0].ID != c.ID {
		t.Fatalf("chưa ghim: cuộc có tin mới nhất phải đứng đầu, got %d", list[0].ID)
	}
	if _, err := s.SetPinned(v.ktv1, c2.ID, true); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListConversations(v.ktv1)
	if list[0].ID != c2.ID || list[0].PinnedAt == nil {
		t.Fatalf("ghim: %d đứng đầu, want %d", list[0].ID, c2.ID)
	}
	if l2, _ := s.ListConversations(v.ops); l2[0].PinnedAt != nil {
		t.Fatal("ghim là riêng từng người")
	}
	_, _ = s.SetPinned(v.ktv1, c2.ID, false)
	if list, _ = s.ListConversations(v.ktv1); list[0].ID != c.ID {
		t.Fatal("bỏ ghim mà vẫn đứng đầu")
	}

	// Tìm: chỉ trong cuộc của mình, không ra tin đã thu hồi.
	_, _ = s.SendMessage(v.ktv1, c.ID, SendInput{Body: "Đơn GẤP cho khách Mỹ"})
	hits, err := s.Search(v.ktv2, "KHÁCH mỹ", 0)
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].Body, "GẤP") {
		t.Fatalf("tìm 'KHÁCH mỹ': %v %+v", err, hits)
	}
	if hits, _ := s.Search(v.ktv2, "cuộc khác", 0); len(hits) != 0 {
		t.Fatal("tìm ra tin ở cuộc mình không ở")
	}
	if hits, _ := s.Search(v.ktv1, "đèn núi", 0); len(hits) != 0 {
		t.Fatal("tìm ra tin đã thu hồi")
	}
	_, err = s.Search(v.ktv1, "a", 0)
	wantChatStatus(t, err, http.StatusBadRequest, "tìm 1 ký tự")
}

// Mã đơn / batch: chỉ mã có thật thành link; mã sản phẩm "_1/2" mở đơn của nó.
func TestChatV2_CodeLinks(t *testing.T) {
	orders, items, batches := extractChatCodes(
		"Đơn 106788, 106788_1/2 và batch #101558. Số lạ: 0912345678, link https://x.com/d/123456, giá 1.500.000, #12 nữa, 107001.")
	if strings.Join(orders, ",") != "106788,107001" || strings.Join(items, ",") != "106788_1/2" ||
		strings.Join(batches, ",") != "#101558" {
		t.Fatalf("tách mã: orders=%v items=%v batches=%v", orders, items, batches)
	}

	v := newV2Fixture(t)
	s := v.svc
	if err := v.db.AutoMigrate(&models.Order{}, &models.OrderItem{}, &models.Batch{}, &models.Material{}); err != nil {
		t.Fatal(err)
	}
	o := &models.Order{InternalCode: "106788", StoreOrderID: "S1", SellerID: 1}
	if err := v.db.Create(o).Error; err != nil {
		t.Fatal(err)
	}
	if err := v.db.Create(&models.OrderItem{OrderID: o.ID, InternalCode: "106788_1/2", LineNo: 1}).Error; err != nil {
		t.Fatal(err)
	}
	b := &models.Batch{Code: "#101558", MaterialID: 1}
	if err := v.db.Create(b).Error; err != nil {
		t.Fatal(err)
	}
	gone := &models.Order{InternalCode: "107001", StoreOrderID: "S2", SellerID: 1}
	v.db.Create(gone)
	v.db.Delete(gone) // đơn đã xoá: không thành link

	c, _ := s.OpenDirect(v.ktv1, v.ktv2.ID)
	m, err := s.SendMessage(v.ktv1, c.ID, SendInput{Body: "Check đơn 106788 (sp 106788_1/2), batch #101558, đơn 999999 và 107001"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ChatRef{}
	for _, r := range m.Refs {
		got[r.Code] = r
	}
	if len(got) != 3 || got["106788"].Kind != "order" || got["106788"].ID != o.ID ||
		got["106788_1/2"].ID != o.ID || got["#101558"].Kind != "batch" || got["#101558"].ID != b.ID {
		t.Fatalf("refs = %+v", m.Refs)
	}
	// Sửa tin thì dò lại.
	e, _ := s.EditMessage(v.ktv1, m.ID, "chỉ còn batch #101558")
	if len(e.Refs) != 1 || e.Refs[0].Code != "#101558" {
		t.Fatalf("refs sau sửa = %+v", e.Refs)
	}
	raw, _ := json.Marshal(e)
	if !strings.Contains(string(raw), `"refs":[{"code":"#101558","kind":"batch"`) {
		t.Fatalf("JSON gửi web: %s", raw)
	}
}
