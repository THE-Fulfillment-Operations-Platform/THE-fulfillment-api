// Package tracking24h is the HTTP client for the 24hTrack tracking platform
// (https://api.24htrack.com). It speaks the dashboard ("internal") API with a
// JWT obtained from email/password rather than the external /track/v1 API key
// API, for two reasons:
//
//   - the account may hold only ONE active API key at a time, and revoking the
//     existing one would break whatever already uses it;
//   - only the internal API can look a parcel up by its description, which is
//     what lets us find a shipment from a store order id.
//
// The client owns nothing about orders — it maps 1:1 onto provider endpoints and
// leaves normalization to the service layer.
package tracking24h

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the production 24hTrack API.
const DefaultBaseURL = "https://api.24htrack.com"

// ErrNotFound is returned when the provider has no such tracking number under
// the account. Callers treat it as "not registered yet", not as a failure.
var ErrNotFound = errors.New("tracking24h: tracking number not found")

// Client is a concurrency-safe 24hTrack API client. The zero value is not
// usable; build one with New.
type Client struct {
	baseURL  string
	email    string
	password string
	http     *http.Client

	// minInterval throttles every outgoing call. The provider allows 200
	// requests/minute on /tracking and answers 429-equivalents with code -3; a
	// fixed floor between calls keeps a batch sync comfortably under that without
	// needing a token bucket.
	minInterval time.Duration

	mu       sync.Mutex // guards token, tokenExp and lastCall
	token    string
	tokenExp time.Time
	lastCall time.Time

	// loginMu collapses concurrent logins into one. Without it, a burst of
	// callers arriving on an expired token would each POST /auth/login and blow
	// through the provider's 10 requests/minute limit on that route.
	loginMu sync.Mutex
}

// Option customises the client.
type Option func(*Client)

// WithHTTPClient injects a custom http.Client (tests, proxies, timeouts).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithMinInterval sets the floor between two outgoing requests.
func WithMinInterval(d time.Duration) Option { return func(c *Client) { c.minInterval = d } }

// New builds a client. baseURL may be empty to use the production endpoint.
func New(baseURL, email, password string, opts ...Option) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		email:    email,
		password: password,
		// Generous but bounded: the provider occasionally takes seconds to answer a
		// cold parcel, and a hung request would otherwise stall the whole sync loop.
		http:        &http.Client{Timeout: 30 * time.Second},
		minInterval: 350 * time.Millisecond,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ---------- provider payloads ----------

// Detail is a parcel as the provider knows it.
type Detail struct {
	TrackingNumber string     `json:"tracking_number"`
	Carrier        string     `json:"carrier"`
	Description    string     `json:"description"`
	Status         string     `json:"status"`
	RawStatus      string     `json:"rawStatus"`
	Detail         string     `json:"detail"`
	Location       string     `json:"location"`
	Note           string     `json:"note"`
	LastCheck      *time.Time `json:"last_check"`
	DeliveredAt    *time.Time `json:"delivered_at"`
	RegisteredAt   *time.Time `json:"registered_at"`
}

// Event is one carrier scan.
type Event struct {
	EventDate   string `json:"event_date"`
	Location    string `json:"location"`
	Description string `json:"description"`
	StatusHint  string `json:"status_hint"`
}

// RegisterItem asks the provider to start tracking a number. Description is what
// makes the parcel findable again by store order id.
type RegisterItem struct {
	Number      string `json:"number"`
	Carrier     string `json:"carrier,omitempty"`
	Description string `json:"description"`
}

// RegisterResult reports which numbers the provider took and which it refused.
type RegisterResult struct {
	Accepted []struct {
		Number      string `json:"number"`
		Carrier     string `json:"carrier"`
		Description string `json:"description"`
	} `json:"accepted"`
	Rejected []struct {
		Number string `json:"number"`
		Reason string `json:"reason"`
	} `json:"rejected"`
	// QuotaMessage is set when the account's monthly/top-up quota only covered
	// part of the batch. It is a human string meant to reach the operator's log.
	QuotaMessage string `json:"quotaMessage"`
}

// ---------- endpoints ----------

// Register starts (or updates) provider tracking for up to 40 numbers. Calling it
// for a number the account already owns does NOT consume quota — it just updates
// the description, which is how a shipment gets tagged with its store order id
// after the fact.
func (c *Client) Register(ctx context.Context, items []RegisterItem) (*RegisterResult, error) {
	if len(items) == 0 {
		return &RegisterResult{}, nil
	}
	if len(items) > MaxRegisterBatch {
		return nil, fmt.Errorf("tracking24h: register accepts at most %d items, got %d", MaxRegisterBatch, len(items))
	}
	var out RegisterResult
	body := map[string]any{"numbers": items}
	if err := c.do(ctx, http.MethodPost, "/tracking/register", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MaxRegisterBatch is the provider's per-request cap for /tracking/register.
const MaxRegisterBatch = 40

// Get returns the current provider state of one tracking number, or ErrNotFound
// when the account does not track it.
func (c *Client) Get(ctx context.Context, number string) (*Detail, error) {
	var out Detail
	if err := c.do(ctx, http.MethodGet, "/tracking/"+url.PathEscape(number), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Events returns the parcel's timeline, newest first (provider order preserved).
func (c *Client) Events(ctx context.Context, number string) ([]Event, error) {
	var out []Event
	if err := c.do(ctx, http.MethodGet, "/tracking/"+url.PathEscape(number)+"/events", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListPage is one page of a description search.
type ListPage struct {
	Items []Detail `json:"items"`
	Total int      `json:"total"`
	Page  int      `json:"page"`
	Pages int      `json:"pages"`
}

// SearchByDescription finds parcels whose description CONTAINS the given text —
// the provider filter is a LIKE, not an equality test, so callers that need an
// exact match must still compare the returned descriptions themselves.
func (c *Client) SearchByDescription(ctx context.Context, description string, page, limit int) (*ListPage, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 100
	}
	q := url.Values{}
	q.Set("description", description)
	q.Set("page", strconv.Itoa(page))
	q.Set("limit", strconv.Itoa(limit))
	var out ListPage
	if err := c.do(ctx, http.MethodGet, "/tracking", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- transport ----------

// envelope is the provider's uniform response shape: code 0 plus data on
// success, a negative code plus message on failure.
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// do performs one authenticated request, transparently (re-)logging in. A 401 is
// retried exactly once with a fresh token: access tokens live 15 minutes, so a
// long sync loop will inevitably cross an expiry mid-run.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if err := c.ensureToken(ctx, false); err != nil {
		return err
	}
	status, raw, err := c.roundTrip(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		if err := c.ensureToken(ctx, true); err != nil {
			return err
		}
		if status, raw, err = c.roundTrip(ctx, method, path, query, body); err != nil {
			return err
		}
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("tracking24h: %s %s: bad response (http %d): %w", method, path, status, err)
	}
	if env.Code != 0 {
		if status == http.StatusNotFound || strings.Contains(strings.ToLower(env.Message), "not found") {
			return ErrNotFound
		}
		return fmt.Errorf("tracking24h: %s %s: provider error %d: %s", method, path, env.Code, env.Message)
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("tracking24h: %s %s: cannot decode data: %w", method, path, err)
	}
	return nil
}

// roundTrip sends one request and returns the status + raw body. It never
// interprets the payload; do() owns that.
func (c *Client) roundTrip(ctx context.Context, method, path string, query url.Values, body any) (int, []byte, error) {
	c.throttle()

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("tracking24h: encode body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("tracking24h: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("tracking24h: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	// Cap the read: a proxy returning an HTML error page must not be buffered
	// without bound just because it claimed a huge Content-Length.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("tracking24h: %s %s: read body: %w", method, path, err)
	}
	return resp.StatusCode, raw, nil
}

// throttle blocks until minInterval has elapsed since the previous call. Holding
// the mutex across the sleep is deliberate: it serialises callers, which is
// exactly the property that keeps concurrent syncs inside the rate limit.
func (c *Client) throttle() {
	if c.minInterval <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.minInterval - time.Since(c.lastCall); wait > 0 && !c.lastCall.IsZero() {
		time.Sleep(wait)
	}
	c.lastCall = time.Now()
}

// ensureToken logs in when there is no usable token. force discards the current
// one (used after a 401).
func (c *Client) ensureToken(ctx context.Context, force bool) error {
	c.mu.Lock()
	// A 60s safety margin: a token that expires while the request is in flight
	// would come back 401 and cost an extra round-trip.
	fresh := c.token != "" && time.Now().Add(time.Minute).Before(c.tokenExp)
	if fresh && !force {
		c.mu.Unlock()
		return nil
	}
	if force {
		c.token, c.tokenExp = "", time.Time{}
	}
	c.mu.Unlock()

	return c.login(ctx)
}

// login exchanges email/password for an access token. The refresh token is
// deliberately ignored: the provider rotates refresh tokens and revokes the
// whole family if one is ever replayed, which is a real hazard for a server that
// may run several instances. Re-logging in every ~15 minutes stays far inside
// the 10 requests/minute limit on /auth.
func (c *Client) login(ctx context.Context) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	// Another goroutine may have logged in while we waited for the lock.
	c.mu.Lock()
	fresh := c.token != "" && time.Now().Add(time.Minute).Before(c.tokenExp)
	c.mu.Unlock()
	if fresh {
		return nil
	}

	c.throttle()

	payload, _ := json.Marshal(map[string]string{"email": c.email, "password": c.password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/auth/login", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("tracking24h: build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tracking24h: login: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("tracking24h: login: read body: %w", err)
	}

	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Token   string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("tracking24h: login: bad response (http %d): %w", resp.StatusCode, err)
	}
	if out.Code != 0 || out.Token == "" {
		msg := out.Message
		if msg == "" {
			msg = "no token returned"
		}
		return fmt.Errorf("tracking24h: login failed (http %d): %s", resp.StatusCode, msg)
	}

	exp := jwtExpiry(out.Token)
	if exp.IsZero() {
		// Unparseable token: assume the documented 15-minute lifetime rather than
		// treating it as permanently valid.
		exp = time.Now().Add(15 * time.Minute)
	}
	c.mu.Lock()
	c.token, c.tokenExp = out.Token, exp
	c.mu.Unlock()
	return nil
}

// jwtExpiry reads the exp claim without verifying the signature — we are the
// bearer, not the verifier; we only need to know when to ask for a new one.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}
