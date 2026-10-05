package services

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/secretbox"
	"the-fulfillment/backend/internal/shipping/theapi"
)

// Kết nối THE: token API của tài khoản THE của xưởng, dịch vụ gửi mặc định, số
// điện thoại dự phòng. Chỉ Owner/Admin đọc/sửa được. Token lưu niêm phong
// (secretbox); màn hình chỉ thấy vài ký tự cuối.

// theAPI is what FFM uses of THE's customer API — an interface so the tests run
// against a fake THE instead of a wallet.
type theAPI interface {
	CreatePackage(ctx context.Context, in theapi.CreatePackageRequest) (*theapi.Package, error)
	DeliverPackage(ctx context.Context, id string) (*theapi.DeliverResult, error)
	GetPackage(ctx context.Context, id string) (*theapi.PackageDetail, error)
	CancelPackages(ctx context.Context, ids []string) error
	ValidateAddress(ctx context.Context, in theapi.AddressCheckRequest) (*theapi.AddressCheck, error)
	FetchLabel(ctx context.Context, code string) ([]byte, string, error)
	ListServices(ctx context.Context) ([]theapi.Service, error)
	GetBalance(ctx context.Context) (*theapi.Balance, error)
}

// CarrierOptions wires the THE integration. Box seals the API token; NewClient
// builds a client for a base URL + token (tests swap in a fake).
type CarrierOptions struct {
	Box       *secretbox.Box
	NewClient func(baseURL, token string) theAPI
}

// DefaultTHEClient is the production client factory.
func DefaultTHEClient(baseURL, token string) theAPI { return theapi.New(baseURL, token) }

// CarrierService owns the THE connection and the shipments FFM creates on THE.
type CarrierService struct {
	repo     *repositories.Repositories
	audit    *AuditService
	tracking *TrackingSyncService
	opts     CarrierOptions
}

// CarrierConfigView is what the settings screen sees — never the token.
type CarrierConfigView struct {
	Provider         string  `json:"provider"`
	Enabled          bool    `json:"enabled"`
	BaseURL          string  `json:"base_url"`
	HasToken         bool    `json:"has_token"`
	TokenHint        string  `json:"token_hint"`
	TokenUnreadable  bool    `json:"token_unreadable"`
	ServiceCode      string  `json:"service_code"`
	DefaultPhone     string  `json:"default_phone"`
	PackagingWeightG float64 `json:"packaging_weight_g"`
	// Customs defaults set by the carrier side; a SKU overrides them.
	DefaultHSCode        string  `json:"default_hs_code"`
	DefaultDeclaredValue float64 `json:"default_declared_value"`
	// Ready = enabled and everything a shipment needs is configured.
	Ready bool `json:"ready"`
	// Problem says, in the operator's words, why it is not ready.
	Problem string `json:"problem,omitempty"`
}

// CarrierConfigInput updates the connection. Pointer fields: omitted = keep.
// Token: omitted/"" = keep the stored token (the screen never shows it, so it
// cannot send it back); ClearToken removes it.
type CarrierConfigInput struct {
	Enabled          *bool    `json:"enabled"`
	Token            *string  `json:"token"`
	ClearToken       bool     `json:"clear_token"`
	ServiceCode      *string  `json:"service_code"`
	DefaultPhone     *string  `json:"default_phone"`
	PackagingWeightG *float64 `json:"packaging_weight_g"`
	BaseURL          *string  `json:"base_url"`
	// "" / 0 clears the default (every SKU must then declare its own).
	DefaultHSCode        *string  `json:"default_hs_code"`
	DefaultDeclaredValue *float64 `json:"default_declared_value"`
}

func canManageCarrier(a Actor) bool {
	return a.Role == models.RoleOwner || a.Role == models.RoleAdmin
}

func (s *CarrierService) loadConfig() (*models.CarrierConfig, error) {
	c, err := s.repo.Carrier.Config(models.CarrierProviderTHE)
	if err != nil {
		return nil, apperr.Internal("could not read carrier config").Wrap(err)
	}
	if c == nil {
		c = &models.CarrierConfig{Provider: models.CarrierProviderTHE}
	}
	return c, nil
}

// token opens the sealed token ("" when none). ErrUnreadable when the server
// secret changed since it was saved.
func (s *CarrierService) token(c *models.CarrierConfig) (string, error) {
	if c.TokenSealed == "" {
		return "", nil
	}
	if s.opts.Box == nil {
		return "", secretbox.ErrUnreadable
	}
	return s.opts.Box.Open(c.TokenSealed)
}

func (s *CarrierService) view(c *models.CarrierConfig) *CarrierConfigView {
	v := &CarrierConfigView{
		Provider: c.Provider, Enabled: c.Enabled, BaseURL: c.BaseURL,
		HasToken: c.TokenSealed != "", TokenHint: c.TokenHint,
		ServiceCode: c.ServiceCode, DefaultPhone: c.DefaultPhone, PackagingWeightG: c.PackagingWeightG,
		DefaultHSCode: c.DefaultHSCode, DefaultDeclaredValue: c.DefaultDeclaredValue,
	}
	if v.BaseURL == "" {
		v.BaseURL = theapi.DefaultBaseURL
	}
	if v.HasToken {
		if _, err := s.token(c); err != nil {
			v.TokenUnreadable = true
		}
	}
	switch {
	case !c.Enabled:
		v.Problem = "Chưa bật kết nối THE — bấm Gửi cho THE chỉ ghi nhận bàn giao, không tạo đơn bên THE"
	case !v.HasToken:
		v.Problem = "Chưa nhập API token của tài khoản THE"
	case v.TokenUnreadable:
		v.Problem = "Token đã lưu không đọc được nữa (khoá máy chủ đã đổi) — nhập lại token"
	case c.ServiceCode == "":
		v.Problem = "Chưa chọn dịch vụ THE"
	default:
		v.Ready = true
	}
	return v
}

// GetConfig returns the connection as the settings screen shows it.
func (s *CarrierService) GetConfig(actor Actor) (*CarrierConfigView, error) {
	if !canManageCarrier(actor) {
		return nil, apperr.Forbidden("Chỉ Owner/Admin xem được cấu hình kết nối THE")
	}
	c, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	return s.view(c), nil
}

// UpdateConfig saves the connection.
func (s *CarrierService) UpdateConfig(actor Actor, in CarrierConfigInput) (*CarrierConfigView, error) {
	if !canManageCarrier(actor) {
		return nil, apperr.Forbidden("Chỉ Owner/Admin sửa được cấu hình kết nối THE")
	}
	c, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	changes := models.JSONMap{}
	if in.ClearToken {
		c.TokenSealed, c.TokenHint = "", ""
		changes["token"] = "cleared"
	}
	if in.Token != nil {
		tok := strings.TrimSpace(*in.Token)
		tok = strings.TrimPrefix(tok, "Bearer ")
		if tok != "" {
			if len(tok) < 16 || strings.ContainsAny(tok, " \t\r\n") {
				return nil, apperr.BadRequest("Token THE không hợp lệ — dán đúng chuỗi API token của tài khoản THE")
			}
			if s.opts.Box == nil {
				return nil, apperr.Internal("secret box not configured")
			}
			sealed, err := s.opts.Box.Seal(tok)
			if err != nil {
				return nil, apperr.Internal("could not seal token").Wrap(err)
			}
			c.TokenSealed = sealed
			c.TokenHint = "…" + tok[len(tok)-4:]
			changes["token"] = "replaced " + c.TokenHint
		}
	}
	if in.ServiceCode != nil {
		c.ServiceCode = strings.TrimSpace(*in.ServiceCode)
		changes["service_code"] = c.ServiceCode
	}
	if in.DefaultPhone != nil {
		c.DefaultPhone = strings.TrimSpace(*in.DefaultPhone)
		changes["default_phone"] = c.DefaultPhone
	}
	if in.PackagingWeightG != nil {
		w := *in.PackagingWeightG
		if w < 0 || w > 5000 {
			return nil, apperr.BadRequest("Cân nặng bao bì phải từ 0 đến 5000 g")
		}
		c.PackagingWeightG = w
		changes["packaging_weight_g"] = w
	}
	if in.DefaultHSCode != nil {
		code, err := normalizeHSCode(*in.DefaultHSCode)
		if err != nil {
			return nil, err
		}
		c.DefaultHSCode = code
		changes["default_hs_code"] = code
	}
	if in.DefaultDeclaredValue != nil {
		v := *in.DefaultDeclaredValue
		if v < 0 || v > maxDeclaredValue {
			return nil, apperr.BadRequest(fmt.Sprintf("Giá trị khai báo mặc định phải từ 0 đến %d USD", maxDeclaredValue))
		}
		c.DefaultDeclaredValue = math.Round(v*100) / 100
		changes["default_declared_value"] = c.DefaultDeclaredValue
	}
	if in.BaseURL != nil {
		u := strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
		// https only — except a local mock THE for testing on a developer machine.
		local := strings.HasPrefix(u, "http://localhost:") || strings.HasPrefix(u, "http://127.0.0.1:")
		if u != "" && !strings.HasPrefix(u, "https://") && !local {
			return nil, apperr.BadRequest("Địa chỉ API THE phải là https://")
		}
		c.BaseURL = u
		changes["base_url"] = u
	}
	if in.Enabled != nil {
		c.Enabled = *in.Enabled
		changes["enabled"] = c.Enabled
	}
	if c.Enabled {
		// Turning it on half-configured would make every "Gửi cho THE" fail; say
		// what is missing now instead.
		if c.TokenSealed == "" {
			return nil, apperr.BadRequest("Nhập API token THE trước khi bật kết nối")
		}
		if c.ServiceCode == "" {
			return nil, apperr.BadRequest("Chọn dịch vụ THE trước khi bật kết nối")
		}
	}
	c.UpdatedByID = actor.IDPtr()
	if err := s.repo.Carrier.SaveConfig(c); err != nil {
		return nil, apperr.Internal("could not save carrier config").Wrap(err)
	}
	s.audit.Log(actor, "CARRIER_CONFIG_UPDATE", "carrier_config", &c.ID, "Cập nhật kết nối THE", changes)
	return s.view(c), nil
}

// CarrierCheck is the result of "Kiểm tra kết nối": the token works, the
// wallet, and the services the account can use.
type CarrierCheck struct {
	OK       bool               `json:"ok"`
	Message  string             `json:"message,omitempty"`
	Balance  float64            `json:"balance"`
	Debt     float64            `json:"debt"`
	Services []THEServiceOption `json:"services"`
}

// THEServiceOption is one THE service as the settings dropdown needs it.
type THEServiceOption struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// client builds a THE client from the stored config, or explains why not.
func (s *CarrierService) client(c *models.CarrierConfig) (theAPI, error) {
	tok, err := s.token(c)
	if err != nil {
		return nil, apperr.BadRequest("Token THE đã lưu không đọc được nữa — vào Cài đặt nhập lại token")
	}
	if tok == "" {
		return nil, apperr.BadRequest("Chưa nhập API token THE (Cài đặt → Kết nối THE)")
	}
	factory := s.opts.NewClient
	if factory == nil {
		factory = DefaultTHEClient
	}
	return factory(c.BaseURL, tok), nil
}

// CheckConnection calls THE read-only (balance + services): proves the token
// and fills the service dropdown. Never creates anything.
func (s *CarrierService) CheckConnection(ctx context.Context, actor Actor) (*CarrierCheck, error) {
	if !canManageCarrier(actor) {
		return nil, apperr.Forbidden("Chỉ Owner/Admin kiểm tra được kết nối THE")
	}
	c, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	cli, err := s.client(c)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out := &CarrierCheck{Services: []THEServiceOption{}}
	bal, err := cli.GetBalance(ctx)
	if err != nil {
		out.Message = theErrorMessage(err)
		return out, nil
	}
	out.Balance, out.Debt = float64(bal.Balance), float64(bal.Debt)
	svcs, err := cli.ListServices(ctx)
	if err != nil {
		out.Message = "Token dùng được nhưng không lấy được danh sách dịch vụ: " + theErrorMessage(err)
		return out, nil
	}
	for _, sv := range svcs {
		code := strings.TrimSpace(sv.Code.String())
		if code == "" {
			continue
		}
		name := strings.TrimSpace(sv.Name.String())
		if name == "" {
			name = code
		}
		out.Services = append(out.Services, THEServiceOption{Code: code, Name: name})
	}
	out.OK = true
	return out, nil
}

// theErrorMessage turns a THE client error into the operator's words.
func theErrorMessage(err error) string {
	var te *theapi.Error
	if errors.As(err, &te) {
		if te.Unauthorized() {
			return "THE từ chối token (401) — token sai hoặc đã bị đổi trên THE"
		}
		return "THE từ chối: " + te.Detail()
	}
	if theapi.IsTransport(err) {
		return "Không nhận được trả lời rõ ràng từ THE (mạng / THE quá tải) — thử lại sau"
	}
	return err.Error()
}
