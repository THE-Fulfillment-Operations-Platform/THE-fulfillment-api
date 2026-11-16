// Package chathub giữ các kết nối WebSocket của chat nội bộ trong RAM.
//
// API chỉ chạy MỘT instance (một droplet) nên hub trong RAM là đủ, không cần
// Redis/pub-sub. Nếu sau này chạy nhiều instance thì phải thay hub này bằng một
// kênh phát chung (vd. Postgres LISTEN/NOTIFY).
//
// Hub chỉ đẩy sự kiện từ server xuống trình duyệt. Trình duyệt gửi lên duy nhất
// "ping" (giữ kết nối); mọi thao tác thật (gửi tin, đánh dấu đã đọc) đi qua REST.
package chathub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 70 * time.Second
	pingPeriod = 30 * time.Second
	maxInbound = 1024
	// Hàng đợi gửi mỗi kết nối. Đầy = máy kia không đọc kịp (mạng chết mà TCP
	// chưa biết) → cắt kết nối; trình duyệt tự nối lại và lấy bù qua REST.
	sendBuffer = 128
	ticketTTL  = 60 * time.Second
)

// Client là một kết nối WebSocket (một tab trình duyệt).
type Client struct {
	conn   *websocket.Conn
	userID uint
	send   chan []byte
	once   sync.Once
}

// Hub giữ mọi kết nối, theo user.
type Hub struct {
	mu      sync.RWMutex
	clients map[uint]map[*Client]struct{}

	tmu     sync.Mutex
	tickets map[string]ticket
}

type ticket struct {
	userID  uint
	expires time.Time
}

// New tạo hub rỗng.
func New() *Hub {
	return &Hub{clients: map[uint]map[*Client]struct{}{}, tickets: map[string]ticket{}}
}

// IssueTicket cấp vé vào WebSocket dùng MỘT lần, hạn 60 giây. Trình duyệt không
// gắn được header Authorization vào WebSocket; để JWT trên query thì JWT nằm
// trong log truy cập. Vé ngắn hạn dùng một lần thì lộ ra log cũng vô hại.
func (h *Hub) IssueTicket(userID uint) string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	t := hex.EncodeToString(b[:])
	now := time.Now()
	h.tmu.Lock()
	for k, v := range h.tickets {
		if now.After(v.expires) {
			delete(h.tickets, k)
		}
	}
	h.tickets[t] = ticket{userID: userID, expires: now.Add(ticketTTL)}
	h.tmu.Unlock()
	return t
}

// RedeemTicket đổi vé lấy user id (vé mất hiệu lực ngay).
func (h *Hub) RedeemTicket(t string) (uint, bool) {
	h.tmu.Lock()
	defer h.tmu.Unlock()
	v, ok := h.tickets[t]
	if !ok {
		return 0, false
	}
	delete(h.tickets, t)
	if time.Now().After(v.expires) {
		return 0, false
	}
	return v.userID, true
}

// SendJSON gửi một sự kiện tới mọi kết nối của các user.
func (h *Hub) SendJSON(userIDs []uint, v interface{}) {
	if len(userIDs) == 0 {
		return
	}
	payload, err := json.Marshal(v)
	if err != nil {
		log.Printf("chathub: marshal: %v", err)
		return
	}
	h.mu.RLock()
	var targets []*Client
	for _, id := range userIDs {
		for c := range h.clients[id] {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.enqueue(payload)
	}
}

func (c *Client) enqueue(payload []byte) {
	defer func() { _ = recover() }() // kênh đã đóng giữa chừng
	select {
	case c.send <- payload:
	default:
		c.close() // không đọc kịp: cắt, để trình duyệt nối lại và lấy bù qua REST
	}
}

func (c *Client) close() {
	c.once.Do(func() { close(c.send) })
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.clients[c.userID]
	if set == nil {
		set = map[*Client]struct{}{}
		h.clients[c.userID] = set
	}
	set[c] = struct{}{}
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.clients[c.userID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.clients, c.userID)
		}
	}
}

// Serve chạy một kết nối đã upgrade tới khi nó đóng (chặn; gọi trong handler).
// Ngay khi vào, trình duyệt nhận "hello" — dấu hiệu để kéo bù tin lỡ qua REST.
func (h *Hub) Serve(conn *websocket.Conn, userID uint) {
	c := &Client{conn: conn, userID: userID, send: make(chan []byte, sendBuffer)}
	h.register(c)
	defer h.unregister(c)
	if b, err := json.Marshal(map[string]interface{}{"type": "hello", "user_id": userID}); err == nil {
		c.enqueue(b)
	}
	done := make(chan struct{})
	go c.writePump(done)
	c.readPump()
	c.close()
	<-done
}

func (c *Client) readPump() {
	c.conn.SetReadLimit(maxInbound)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	pong, _ := json.Marshal(map[string]string{"type": "pong"})
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		var env struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &env) == nil && env.Type == "ping" {
			c.enqueue(pong)
		}
	}
}

func (c *Client) writePump(done chan struct{}) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
		close(done)
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
