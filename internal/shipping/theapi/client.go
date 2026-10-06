// Package theapi is a client for THE's customer API
// (https://api.thehuman.express/v1/customer, Bearer API token).
//
// Only what FFM uses: create a package, confirm it (delivery → THE tracking +
// label, debits the wallet), cancel, fetch the label, look a package up again,
// list services, read the wallet balance.
//
// THE's JSON is not strict about types (the same field is a number in one
// response and a string in another — service_code is a string in the request
// and an integer in the create response). Every identifier is therefore decoded
// with Flex, which accepts both, so a type wobble on THE's side never turns a
// PAID shipment into a decode error that FFM would treat as a failure.
package theapi

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
	"time"
)

// DefaultBaseURL is THE's production customer API.
const DefaultBaseURL = "https://api.thehuman.express/v1/customer"

// Client talks to one THE account (one API token).
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a client. baseURL "" = production.
func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// Per-request deadlines come from the caller's context; this is the
		// backstop so a hung connection can never pin a goroutine forever.
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// ---------- errors ----------

// Error is a refusal from THE (HTTP 4xx/5xx), with THE's own messages.
type Error struct {
	Status   int
	Message  string
	Messages []string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(strings.Join(e.Messages, "; "))
	if msg == "" {
		msg = e.Message
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("THE %d: %s", e.Status, msg)
}

// Detail is the human part of the refusal (THE's messages), without the
// status prefix — what the operator reads.
func (e *Error) Detail() string {
	if len(e.Messages) > 0 {
		return strings.Join(e.Messages, "; ")
	}
	if e.Message != "" {
		return e.Message
	}
	return http.StatusText(e.Status)
}

// Unauthorized reports a rejected token.
func (e *Error) Unauthorized() bool { return e.Status == http.StatusUnauthorized }

// ErrTransport wraps failures where THE's answer is UNKNOWN: timeout,
// connection reset, unreadable body. For a create or a delivery this means
// "maybe it happened" — the caller must reconcile, never blindly resend.
var ErrTransport = errors.New("THE: no usable answer")

// IsTransport reports whether err left the outcome unknown.
func IsTransport(err error) bool { return errors.Is(err, ErrTransport) }

// ---------- flexible scalar ----------

// Flex decodes a JSON string, number or null into its text form.
type Flex string

func (f *Flex) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = Flex(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		// booleans and the like: keep the raw token rather than fail the decode
		*f = Flex(string(b))
		return nil
	}
	*f = Flex(n.String())
	return nil
}

func (f Flex) String() string { return string(f) }

// FlexFloat decodes a JSON number or numeric string ("4.89") into a float.
type FlexFloat float64

func (f *FlexFloat) UnmarshalJSON(b []byte) error {
	var s Flex
	if err := s.UnmarshalJSON(b); err != nil {
		return err
	}
	if s == "" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(string(s), 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = FlexFloat(v)
	return nil
}

// ---------- transport ----------

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	raw, ctype, err := c.doRaw(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	_ = ctype
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: unreadable response (%v)", ErrTransport, err)
	}
	return nil
}

func (c *Client) doRaw(ctx context.Context, method, path string, query url.Values, body any) ([]byte, string, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, "", err
		}
		rdr = bytes.NewReader(buf)
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrTransport, err)
	}
	defer resp.Body.Close()
	// Labels are small images/PDFs; 20 MB is a backstop, not a real limit.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, "", fmt.Errorf("%w: reading body: %v", ErrTransport, err)
	}
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode}
		// THE's error body is {"error": "...", "messages": [...]} — except where
		// "messages" is a bare string ("Missing package id"), and except a panic,
		// which is a text/plain "PANIC: …".
		var env struct {
			Error    Flex            `json:"error"`
			Message  Flex            `json:"message"`
			Messages json.RawMessage `json:"messages"`
		}
		if json.Unmarshal(raw, &env) == nil {
			e.Message = firstNonEmpty(string(env.Error), string(env.Message))
			var list []Flex
			var one Flex
			switch {
			case len(env.Messages) == 0:
			case json.Unmarshal(env.Messages, &list) == nil:
				for _, m := range list {
					if s := strings.TrimSpace(string(m)); s != "" {
						e.Messages = append(e.Messages, s)
					}
				}
			case json.Unmarshal(env.Messages, &one) == nil && strings.TrimSpace(string(one)) != "":
				e.Messages = []string{strings.TrimSpace(string(one))}
			}
		} else {
			e.Message = strings.TrimSpace(string(raw))
			if len(e.Message) > 300 {
				e.Message = e.Message[:300]
			}
		}
		// 5xx from THE's edge (502/503/504) says nothing about whether the
		// request was applied behind it: treat as unknown, not as a refusal.
		if resp.StatusCode >= 500 {
			return nil, "", fmt.Errorf("%w: %v", ErrTransport, e)
		}
		return nil, "", e
	}
	return raw, resp.Header.Get("Content-Type"), nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// ---------- packages ----------

// CreatePackageRequest is POST /packages. Weight in grams, sizes in cm,
// value in USD.
type CreatePackageRequest struct {
	OrderNumber        string  `json:"order_number"`
	Name               string  `json:"name"`
	Company            string  `json:"company,omitempty"`
	Phone              string  `json:"phone"`
	Address1           string  `json:"address_1"`
	Address2           string  `json:"address_2"`
	City               string  `json:"city"`
	StateCode          string  `json:"state_code"`
	Zipcode            string  `json:"zipcode"`
	CountryCode        string  `json:"country_code"`
	Value              float64 `json:"value"`
	HSCode             string  `json:"hs_code"`
	Weight             float64 `json:"weight"`
	Width              float64 `json:"width"`
	Length             float64 `json:"length"`
	Height             float64 `json:"height"`
	ServiceCode        string  `json:"service_code"`
	IgnoreAddressCheck bool    `json:"ignore_address_check"`
	IncludeBattery     bool    `json:"include_battery"`
}

// Package is THE's package as returned by create / detail / list.
type Package struct {
	// Echoed parcel data. THE de-duplicates a create on order_number while the
	// earlier package is still pending and answers with THAT package's stored
	// data — these fields show whether what came back is what was sent.
	Weight   FlexFloat `json:"weight"`
	Length   FlexFloat `json:"length"`
	Width    FlexFloat `json:"width"`
	Height   FlexFloat `json:"height"`
	Value    FlexFloat `json:"value"`
	Zipcode  Flex      `json:"zipcode"`
	Address1 Flex      `json:"address_1"`
	// HSCode: a fresh create echoes the code sent; a de-duplicated answer
	// carries the OLD package's hs_codes.system_code — "" when THE stored it
	// without one (its create handler keeps going after "Hs Code does not
	// exist" and can leave such a hidden pending package behind).
	HSCode    Flex `json:"hs_code"`
	CreatedAt Flex `json:"created_at"`

	ID          Flex      `json:"id"`
	Code        Flex      `json:"code"` // THE tracking, empty until delivery
	OrderNumber Flex      `json:"order_number"`
	Status      Flex      `json:"status"`
	ServiceCode Flex      `json:"service_code"`
	TotalCost   FlexFloat `json:"total_cost"`
	ShippingFee FlexFloat `json:"shipping_fee"`
	// Last-mile tracking, when THE exposes it on the package.
	LastMileTracking Flex `json:"last_mile_tracking"`
	LastMileCarrier  Flex `json:"last_mile_carrier"`
}

// CreatePackage creates a package (pending; see DeliverPackage for the paid
// step).
func (c *Client) CreatePackage(ctx context.Context, in CreatePackageRequest) (*Package, error) {
	var out struct {
		Package *Package `json:"package"`
	}
	if err := c.do(ctx, http.MethodPost, "/packages", nil, in, &out); err != nil {
		return nil, err
	}
	if out.Package == nil || out.Package.ID == "" {
		return nil, fmt.Errorf("%w: create answered without a package id", ErrTransport)
	}
	return out.Package, nil
}

// DeliverResult is POST /packages/delivery/{id}.
type DeliverResult struct {
	Success     bool   `json:"success"`
	PackageCode Flex   `json:"package_code"` // THE tracking
	BillCode    Flex   `json:"bill_code"`
	Base64Label string `json:"base64_label"`
	// Not sent by THE today (the swagger lists it, the code does not); the
	// last-mile carrier comes from GET /packages/{id}.
	LastMileCarrier Flex `json:"last_mile_carrier"`
}

// DeliverPackage confirms a package: THE debits the wallet, issues its
// tracking and the label.
func (c *Client) DeliverPackage(ctx context.Context, id string) (*DeliverResult, error) {
	var out DeliverResult
	if err := c.do(ctx, http.MethodPost, "/packages/delivery/"+url.PathEscape(id), nil, map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Label decodes the delivery's label. THE returns base64 of a PNG (its own
// label), a PDF or GIF (a carrier label), sometimes a bare URL, sometimes
// nothing. Only a decodable PDF/PNG/GIF/JPEG is returned; anything else is
// (nil, "") and the caller fetches the label by THE tracking instead.
func (d *DeliverResult) Label() ([]byte, string) {
	s := strings.TrimSpace(d.Base64Label)
	if i := strings.Index(s, ","); strings.HasPrefix(s, "data:") && i > 0 {
		s = s[i+1:]
	}
	if s == "" || strings.HasPrefix(s, "http") {
		return nil, ""
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, ""
	}
	t := http.DetectContentType(b)
	switch t {
	case "application/pdf", "image/png", "image/gif", "image/jpeg":
		return b, t
	}
	return nil, ""
}

// PackageDetail is GET /packages/{id}: the package plus its last-mile
// tracking once the carrier label exists.
type PackageDetail struct {
	Package
	BillCode Flex `json:"bill_code"`
	Tracking *struct {
		TrackingNumber  Flex `json:"tracking_number"`
		LastMileCarrier Flex `json:"last_mile_carrier"`
		LabelURL        Flex `json:"label_url"`
	} `json:"tracking"`
}

// LastMile returns the last-mile carrier tracking (e.g. USPS), "" until known.
func (p *PackageDetail) LastMile() (number, carrier string) {
	if p.Tracking != nil {
		number, carrier = strings.TrimSpace(string(p.Tracking.TrackingNumber)), strings.TrimSpace(string(p.Tracking.LastMileCarrier))
	}
	if number == "" {
		number = strings.TrimSpace(string(p.LastMileTracking))
	}
	if carrier == "" {
		carrier = strings.TrimSpace(string(p.LastMileCarrier))
	}
	return number, carrier
}

// GetPackage reads one package by THE id.
func (c *Client) GetPackage(ctx context.Context, id string) (*PackageDetail, error) {
	var out struct {
		Package *PackageDetail `json:"package"`
	}
	if err := c.do(ctx, http.MethodGet, "/packages/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Package == nil {
		return nil, &Error{Status: http.StatusNotFound, Message: "package not found"}
	}
	return out.Package, nil
}

// CancelPackages cancels packages by THE id: a pending package is archived
// (no money involved), a delivered one (pre-transit) is cancelled and refunded
// by THE. THE wants the ids as JSON integers.
func (c *Client) CancelPackages(ctx context.Context, ids []string) error {
	nums := make([]int64, 0, len(ids))
	for _, id := range ids {
		n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
		if err != nil {
			return fmt.Errorf("THE package id %q is not numeric", id)
		}
		nums = append(nums, n)
	}
	return c.do(ctx, http.MethodPut, "/packages/cancel", nil, map[string]any{"ids": nums}, nil)
}

// AddressCheckRequest is POST /packages/address/validate (US only).
type AddressCheckRequest struct {
	Address1    string `json:"address_1"`
	Address2    string `json:"address_2"`
	City        string `json:"city"`
	StateCode   string `json:"state_code"`
	Zipcode     string `json:"zipcode"`
	CountryCode string `json:"country_code"`
}

// AddressCheck is THE's (USPS-backed) cleansed version of an address.
type AddressCheck struct {
	Line1         Flex `json:"line1"`
	Line2         Flex `json:"line2"`
	City          Flex `json:"city"`
	State         Flex `json:"state_province"`
	Zip5          Flex `json:"zip5"`
	Zip4          Flex `json:"zip4"`
	AddressExists Flex `json:"address_exists"`
}

// Exists: the address service found the address ("N" or no line = not found).
func (a *AddressCheck) Exists() bool {
	return strings.TrimSpace(a.Line1.String()) != "" && !strings.EqualFold(strings.TrimSpace(a.AddressExists.String()), "N")
}

// ValidateAddress asks THE to cleanse a US address. Read-only, no money.
func (c *Client) ValidateAddress(ctx context.Context, in AddressCheckRequest) (*AddressCheck, error) {
	var out AddressCheck
	if err := c.do(ctx, http.MethodPost, "/packages/address/validate", nil, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FetchLabel downloads the label of a delivered package by its THE tracking.
func (c *Client) FetchLabel(ctx context.Context, code string) ([]byte, string, error) {
	raw, ctype, err := c.doRaw(ctx, http.MethodGet, "/packages/label/"+url.PathEscape(code), nil, nil)
	if err != nil {
		return nil, "", err
	}
	if ctype == "" || strings.HasPrefix(ctype, "application/octet-stream") {
		ctype = http.DetectContentType(raw)
	}
	return raw, ctype, nil
}

// ---------- account ----------

// Service is one entry of GET /services.
type Service struct {
	ID   Flex `json:"id"`
	Code Flex `json:"code"`
	Name Flex `json:"name"`
}

// ListServices lists the services this account can ship with.
func (c *Client) ListServices(ctx context.Context) ([]Service, error) {
	var out struct {
		Services []Service `json:"services"`
	}
	if err := c.do(ctx, http.MethodGet, "/services", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Services, nil
}

// Balance is GET /users/balance.
type Balance struct {
	Balance       FlexFloat `json:"balance"`
	Debt          FlexFloat `json:"debt"`
	DebtMaxAmount FlexFloat `json:"debt_max_amount"`
}

// GetBalance reads the wallet — also the cheapest way to check a token.
func (c *Client) GetBalance(ctx context.Context) (*Balance, error) {
	var out Balance
	if err := c.do(ctx, http.MethodGet, "/users/balance", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
