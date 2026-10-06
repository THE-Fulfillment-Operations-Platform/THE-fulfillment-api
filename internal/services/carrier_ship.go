package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/shipping/theapi"
)

// Tạo đơn trên THE cho một đơn FFM đã QC xong — bước "Gửi cho THE" khi kết nối
// THE đang bật. Hai lời gọi THE (đã đối chiếu với source THE, xem hồ sơ):
//
//	POST /packages              tạo đơn "pending" — KHÔNG đụng tiền
//	POST /packages/delivery/id  chốt đơn — TRỪ TIỀN VÍ trong một transaction,
//	                            trả mã THE + label
//
// Luật sắt: một đơn FFM không bao giờ ra hai đơn THE đã trả tiền.
//   - Giữ chỗ trước khi gọi: một dòng carrier_shipments, index unique
//     một-dòng-sống-mỗi-đơn chặn lượt bấm thứ hai / lượt quét trùng.
//   - Lời gọi không rõ kết quả (timeout, mất mạng, 5xx) thì KHÔNG gửi lại mù.
//     Tạo đơn mà không rõ: gửi lại cùng order_number là an toàn (THE trả lại đơn
//     pending cũ, và tạo đơn không mất tiền). Chốt đơn mà không rõ: đợi, hỏi THE
//     trạng thái đơn (pending = chưa trừ tiền, có mã THE = đã trừ) rồi mới làm.
//   - Đơn đã LABELED thì bấm lại chỉ trả lại kết quả cũ, không gọi THE nữa.

const (
	// creatingStaleAfter: a CREATING row younger than this may still be in
	// flight in another FFM request; older, its create is simply re-sent (safe:
	// creating costs nothing and THE de-duplicates a pending order_number).
	creatingStaleAfter = 2 * time.Minute
	// unclearDeliveryWait: after a delivery call with no clear answer, wait this
	// long before asking THE and possibly delivering again. THE's own guard is a
	// 2-minute lock and its label purchase has no timeout, so a stuck first
	// attempt can still complete well after our call gave up.
	unclearDeliveryWait = 10 * time.Minute
	theCreateTimeout    = 45 * time.Second
	// Longer than THE's 70 s server write timeout, so we hear THE's own answer
	// (or its timeout) instead of giving up first and creating an unclear state.
	theDeliverTimeout = 80 * time.Second
	theReadTimeout    = 20 * time.Second
)

// unclearDeliveryPrefix marks a CREATED row whose delivery got no clear answer.
const unclearDeliveryPrefix = "Chưa rõ THE đã chốt đơn chưa: "

// ShipOptions are per-send choices of the operator.
type ShipOptions struct {
	// SkipAddressCheck sends without FFM's USPS address check — for addresses
	// the operator has looked at and knows are right.
	SkipAddressCheck bool `json:"skip_address_check"`
}

// Parcel is what FFM declares to THE for one order.
type Parcel struct {
	Request  theapi.CreatePackageRequest `json:"-"`
	WeightG  float64                     `json:"weight_g"`
	LengthCM float64                     `json:"length_cm"`
	WidthCM  float64                     `json:"width_cm"`
	HeightCM float64                     `json:"height_cm"`
	Value    float64                     `json:"value"`
	HSCode   string                      `json:"hs_code"`
	Units    int                         `json:"units"`
	Country  string                      `json:"country"`
}

// ShipmentOutcome is what one order got on THE.
type ShipmentOutcome struct {
	ShipmentID       uint     `json:"shipment_id"`
	TrackingCode     string   `json:"tracking_code"`
	LastMileTracking string   `json:"last_mile_tracking,omitempty"`
	Cost             *float64 `json:"cost,omitempty"`
	HasLabel         bool     `json:"has_label"`
	// Reused: the order already had a paid shipment; nothing was sent to THE.
	Reused bool `json:"reused,omitempty"`
}

// ShipError is a refusal to send one order, with a stable code the screen
// uses to offer the right next step (e.g. "send without address check").
type ShipError struct {
	Code    string
	Message string
}

func (e *ShipError) Error() string { return e.Message }

// Ship error codes.
const (
	ShipErrData    = "DATA"    // order / SKU data must be fixed in FFM
	ShipErrAddress = "ADDRESS" // USPS check failed — fix, or send skipping the check
	ShipErrTHE     = "THE"     // THE refused (wallet, service, …)
	ShipErrWait    = "WAIT"    // an earlier attempt is unresolved — retry later
	ShipErrUnclear = "UNCLEAR" // no clear answer from THE — retry later, safe
)

func shipErr(code, msg string) error { return &ShipError{Code: code, Message: msg} }

// ---------- active config ----------

// activeTHE returns the config + client when the integration is on and
// complete; (nil, nil, nil) when it is off (the ship flow then records a plain
// handoff, as before); an error when it is on but broken — sending must stop
// rather than silently skip THE.
func (s *CarrierService) activeTHE() (*models.CarrierConfig, theAPI, error) {
	if s == nil {
		return nil, nil, nil
	}
	c, err := s.loadConfig()
	if err != nil {
		return nil, nil, err
	}
	if !c.Enabled {
		return nil, nil, nil
	}
	v := s.view(c)
	if !v.Ready {
		return nil, nil, apperr.BadRequest("Kết nối THE đang bật nhưng chưa sẵn sàng: " + v.Problem)
	}
	cli, err := s.client(c)
	if err != nil {
		return nil, nil, err
	}
	return c, cli, nil
}

// Enabled reports whether "Gửi cho THE" currently creates THE shipments.
func (s *CarrierService) Enabled() bool {
	if s == nil {
		return false
	}
	c, err := s.loadConfig()
	return err == nil && c.Enabled
}

// ---------- parcel ----------

var usZipRe = regexp.MustCompile(`^\d{5}(-\d{4})?$`)

// BuildParcel turns an order into the THE request, or lists — in the
// operator's words — everything that must be fixed first. No THE call.
func (s *CarrierService) BuildParcel(c *models.CarrierConfig, o *models.Order) (*Parcel, []string) {
	var problems []string
	country := normalizeCountry(o.ShippingCountry)
	us := country == "US"
	if country == "" {
		problems = append(problems, "Thiếu quốc gia")
	}
	name := strings.TrimSpace(o.ShippingName)
	addr1 := strings.TrimSpace(o.ShippingAddress1)
	city := strings.TrimSpace(o.ShippingCity)
	zip := strings.ToUpper(strings.TrimSpace(o.ShippingZip))
	if name == "" {
		problems = append(problems, "Thiếu tên người nhận")
	}
	if addr1 == "" {
		problems = append(problems, "Thiếu địa chỉ")
	}
	if city == "" {
		problems = append(problems, "Thiếu thành phố")
	}
	state := strings.ToUpper(strings.TrimSpace(o.ShippingProvince))
	if us {
		state = usStateCode(o.ShippingProvince)
		if state == "" {
			problems = append(problems, fmt.Sprintf("Mã bang %q không hợp lệ (cần 2 chữ, vd CA)", strings.TrimSpace(o.ShippingProvince)))
		}
		// Excel eats the leading zero of New England zips ("02134" → "2134").
		if len(zip) == 4 && isDigits(zip) {
			zip = "0" + zip
		}
		if !usZipRe.MatchString(zip) {
			problems = append(problems, fmt.Sprintf("Zip %q không hợp lệ (cần 5 số)", zip))
		}
	} else if state == "" {
		problems = append(problems, "Thiếu bang/tỉnh (THE bắt buộc có)")
	}
	if !us && zip == "" {
		problems = append(problems, "Thiếu mã bưu điện")
	}
	phone := strings.TrimSpace(o.ShippingPhone)
	if phone == "" {
		phone = strings.TrimSpace(c.DefaultPhone)
	}
	// THE requires a phone only outside the US.
	if phone == "" && !us {
		problems = append(problems, "Đơn đi nước ngoài cần SĐT người nhận — đơn không có và chưa khai SĐT mặc định (Cài đặt → Kết nối THE)")
	}

	live := activeOrderItems(o.Items)
	if len(live) == 0 {
		problems = append(problems, "Đơn không còn sản phẩm nào")
	}
	codes := make([]string, 0, len(live))
	for _, it := range live {
		codes = append(codes, it.SKUCode)
	}
	specs, missing, err := s.shippingSpecs(c, codes)
	if err != nil {
		problems = append(problems, "Không đọc được thông tin vận chuyển SKU")
	}
	problems = append(problems, missing...)

	p := &Parcel{Country: country}
	type line struct {
		value float64
		hs    string
	}
	var lines []line
	for _, it := range live {
		spec, ok := specs[models.NormalizeCode(it.SKUCode)]
		if !ok || len(spec.Missing()) > 0 {
			continue
		}
		q := float64(it.Quantity)
		if q < 1 {
			q = 1
		}
		p.Units += int(q)
		p.WeightG += q * *spec.WeightG
		p.LengthCM = math.Max(p.LengthCM, *spec.LengthCM)
		p.WidthCM = math.Max(p.WidthCM, *spec.WidthCM)
		// Flat decor products stack: the parcel is as tall as the pile.
		p.HeightCM += q * *spec.HeightCM
		v := q * *spec.DeclaredValue
		p.Value += v
		lines = append(lines, line{value: v, hs: spec.HSCode})
	}
	if len(problems) > 0 {
		return nil, problems
	}
	p.WeightG = math.Round((p.WeightG+c.PackagingWeightG)*10) / 10
	p.Value = math.Round(p.Value*100) / 100
	p.HeightCM = math.Round(p.HeightCM*10) / 10
	if us && p.Value >= 800 {
		return nil, []string{fmt.Sprintf("Giá trị khai báo %.2f USD — THE chỉ nhận đơn đi Mỹ dưới 800 USD", p.Value)}
	}
	// One HS code per parcel on THE: the line worth the most speaks for it.
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].value > lines[j].value })
	p.HSCode = lines[0].hs
	p.Request = theapi.CreatePackageRequest{
		OrderNumber: o.InternalCode,
		Name:        name,
		Phone:       phone,
		Address1:    addr1,
		Address2:    strings.TrimSpace(o.ShippingAddress2),
		City:        city,
		StateCode:   state,
		Zipcode:     zip,
		CountryCode: country,
		Value:       p.Value,
		HSCode:      p.HSCode,
		Weight:      p.WeightG,
		Length:      p.LengthCM,
		Width:       p.WidthCM,
		Height:      p.HeightCM,
		ServiceCode: c.ServiceCode,
		// FFM checks US addresses itself (checkAddress) before creating; THE's
		// own asynchronous check is stricter than USPS (line 1 must match the
		// cleansed line letter for letter) and makes delivery fail "Invalid
		// Address" for real addresses.
		IgnoreAddressCheck: true,
	}
	return p, nil
}

// shippingSpecs resolves the effective shipping declaration of each SKU code
// (parent fallback, then the connection's customs defaults) and lists what is
// missing, one message per SKU — pointing whoever must fix it at the right
// screen: weight/box are the factory's (Master Data), HS code / value the
// carrier side's (Settings → Kết nối THE).
func (s *CarrierService) shippingSpecs(c *models.CarrierConfig, codes []string) (map[string]models.ShippingSpec, []string, error) {
	skus, err := s.repo.SKU.ListByCodes(codes)
	if err != nil {
		return nil, nil, err
	}
	byCode := map[string]*models.SKU{}
	var parentIDs []uint
	for i := range skus {
		byCode[skus[i].Code] = &skus[i]
		if skus[i].ParentID != nil {
			parentIDs = append(parentIDs, *skus[i].ParentID)
		}
	}
	parents := map[uint]*models.SKU{}
	if len(parentIDs) > 0 {
		rows, err := s.repo.SKU.ListByIDs(parentIDs)
		if err != nil {
			return nil, nil, err
		}
		for i := range rows {
			parents[rows[i].ID] = &rows[i]
		}
	}
	out := map[string]models.ShippingSpec{}
	var missing []string
	seen := map[string]bool{}
	for _, raw := range codes {
		code := models.NormalizeCode(raw)
		if seen[code] {
			continue
		}
		seen[code] = true
		sk := byCode[code]
		if sk == nil {
			missing = append(missing, fmt.Sprintf("SKU %s không có trong Master Data", code))
			continue
		}
		var parent *models.SKU
		if sk.ParentID != nil {
			parent = parents[*sk.ParentID]
		}
		spec := models.EffectiveShipping(sk, parent).WithDefaults(c.DefaultHSCode, c.DefaultDeclaredValue)
		out[code] = spec
		if m := spec.MissingPhysical(); len(m) > 0 {
			missing = append(missing, fmt.Sprintf("SKU %s chưa khai %s (Master Data → SKU → Vận chuyển)", code, strings.Join(m, ", ")))
		}
		var customs []string
		if spec.HSCode == "" {
			customs = append(customs, "mã HS")
		}
		if spec.DeclaredValue == nil {
			customs = append(customs, "giá trị khai báo")
		}
		if len(customs) > 0 {
			missing = append(missing, fmt.Sprintf("SKU %s chưa có %s — đặt giá trị mặc định ở Cài đặt → Kết nối THE (hoặc khai riêng cho SKU)", code, strings.Join(customs, ", ")))
		}
	}
	return out, missing, nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// normalizeCountry turns the country cell into a 2-letter code where it is
// known; anything else goes to THE as typed (THE also matches country names).
func normalizeCountry(raw string) string {
	k := strings.ToUpper(strings.TrimSpace(raw))
	k = strings.Join(strings.Fields(strings.NewReplacer(".", "", ",", "").Replace(k)), " ")
	switch k {
	case "US", "USA", "UNITED STATES", "UNITED STATES OF AMERICA", "AMERICA", "MỸ", "HOA KỲ", "HOA KY":
		return "US"
	case "GB", "UK", "UNITED KINGDOM", "GREAT BRITAIN", "ENGLAND", "SCOTLAND", "WALES", "NORTHERN IRELAND":
		return "GB"
	case "CA", "CANADA":
		return "CA"
	case "AU", "AUSTRALIA":
		return "AU"
	}
	return k
}

// usStates maps state names to USPS codes; a 2-letter input must be a code.
var usStates = map[string]string{
	"ALABAMA": "AL", "ALASKA": "AK", "ARIZONA": "AZ", "ARKANSAS": "AR", "CALIFORNIA": "CA",
	"COLORADO": "CO", "CONNECTICUT": "CT", "DELAWARE": "DE", "DISTRICT OF COLUMBIA": "DC",
	"FLORIDA": "FL", "GEORGIA": "GA", "HAWAII": "HI", "IDAHO": "ID", "ILLINOIS": "IL",
	"INDIANA": "IN", "IOWA": "IA", "KANSAS": "KS", "KENTUCKY": "KY", "LOUISIANA": "LA",
	"MAINE": "ME", "MARYLAND": "MD", "MASSACHUSETTS": "MA", "MICHIGAN": "MI", "MINNESOTA": "MN",
	"MISSISSIPPI": "MS", "MISSOURI": "MO", "MONTANA": "MT", "NEBRASKA": "NE", "NEVADA": "NV",
	"NEW HAMPSHIRE": "NH", "NEW JERSEY": "NJ", "NEW MEXICO": "NM", "NEW YORK": "NY",
	"NORTH CAROLINA": "NC", "NORTH DAKOTA": "ND", "OHIO": "OH", "OKLAHOMA": "OK", "OREGON": "OR",
	"PENNSYLVANIA": "PA", "RHODE ISLAND": "RI", "SOUTH CAROLINA": "SC", "SOUTH DAKOTA": "SD",
	"TENNESSEE": "TN", "TEXAS": "TX", "UTAH": "UT", "VERMONT": "VT", "VIRGINIA": "VA",
	"WASHINGTON": "WA", "WEST VIRGINIA": "WV", "WISCONSIN": "WI", "WYOMING": "WY",
	"PUERTO RICO": "PR", "GUAM": "GU", "VIRGIN ISLANDS": "VI", "AMERICAN SAMOA": "AS",
	"NORTHERN MARIANA ISLANDS": "MP", "ARMED FORCES AMERICAS": "AA", "ARMED FORCES EUROPE": "AE",
	"ARMED FORCES PACIFIC": "AP",
}

var usStateCodes = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range usStates {
		m[c] = true
	}
	return m
}()

func usStateCode(raw string) string {
	k := strings.ToUpper(strings.TrimSpace(raw))
	k = strings.Join(strings.Fields(strings.ReplaceAll(k, ".", "")), " ")
	if usStateCodes[k] {
		return k
	}
	return usStates[k]
}

// requestHash fingerprints a create request (everything THE stores).
func requestHash(r theapi.CreatePackageRequest) string {
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---------- address ----------

// checkAddress asks THE's (USPS-backed) address service about a US address.
// It refuses only what USPS does not know, or a zip/state that differs from
// USPS — not a line 1 written "Street" where USPS writes "ST".
func (s *CarrierService) checkAddress(ctx context.Context, cli theAPI, p *Parcel) error {
	if p.Country != "US" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, theReadTimeout)
	defer cancel()
	r := p.Request
	res, err := cli.ValidateAddress(cctx, theapi.AddressCheckRequest{
		Address1: r.Address1, Address2: r.Address2, City: r.City,
		StateCode: r.StateCode, Zipcode: r.Zipcode, CountryCode: "US",
	})
	if err != nil {
		var te *theapi.Error
		if errors.As(err, &te) && te.Status < 500 && !te.Unauthorized() {
			return shipErr(ShipErrAddress, "Địa chỉ không qua kiểm tra USPS: "+te.Detail()+" — sửa địa chỉ, hoặc gửi bỏ qua kiểm tra địa chỉ")
		}
		return shipErr(ShipErrAddress, "Chưa kiểm tra được địa chỉ với THE ("+theErrorMessage(err)+") — thử lại, hoặc gửi bỏ qua kiểm tra địa chỉ")
	}
	suggest := strings.TrimSpace(strings.Join([]string{
		res.Line1.String(), res.City.String(), strings.TrimSpace(res.State.String() + " " + res.Zip5.String()),
	}, ", "))
	if !res.Exists() {
		return shipErr(ShipErrAddress, "USPS không tìm thấy địa chỉ này — kiểm tra lại với khách, hoặc gửi bỏ qua kiểm tra địa chỉ")
	}
	zip5 := r.Zipcode
	if len(zip5) > 5 {
		zip5 = zip5[:5]
	}
	if z := strings.TrimSpace(res.Zip5.String()); z != "" && z != zip5 {
		return shipErr(ShipErrAddress, fmt.Sprintf("Zip lệch với USPS (đơn ghi %s) — USPS gợi ý: %s", r.Zipcode, suggest))
	}
	if st := strings.ToUpper(strings.TrimSpace(res.State.String())); st != "" && st != r.StateCode {
		return shipErr(ShipErrAddress, fmt.Sprintf("Bang lệch với USPS (đơn ghi %s) — USPS gợi ý: %s", r.StateCode, suggest))
	}
	return nil
}

// ---------- ship ----------

// ShipOrderOnTHE makes sure the order has a PAID THE shipment (LABELED) and
// returns it. Idempotent: an order that already has one gets it back
// untouched. An error is the operator-facing reason the order was not sent
// (a *ShipError when the screen can offer a next step).
func (s *CarrierService) ShipOrderOnTHE(ctx context.Context, actor Actor, c *models.CarrierConfig, cli theAPI, o *models.Order, opts ShipOptions) (*ShipmentOutcome, error) {
	live, err := s.repo.Carrier.LiveShipment(o.ID)
	if err != nil {
		return nil, apperr.Internal("could not read shipments").Wrap(err)
	}
	if live != nil && live.Status == models.ShipmentLabeled {
		return outcomeOf(live, true), nil
	}
	if live != nil && live.Status == models.ShipmentCreating && time.Since(live.UpdatedAt) < creatingStaleAfter {
		return nil, shipErr(ShipErrWait, "Đơn này vừa được gửi tạo trên THE ở lượt khác — đợi 2 phút rồi gửi lại")
	}

	// What the order looks like NOW.
	parcel, problems := s.BuildParcel(c, o)
	if len(problems) > 0 {
		return nil, shipErr(ShipErrData, strings.Join(problems, " · "))
	}
	hash := requestHash(parcel.Request)

	// A pending package created from older data (address or weights fixed
	// since) must not be the one that gets paid for: archive it, start over.
	if live != nil && live.Status == models.ShipmentCreated && live.RequestHash != hash &&
		!strings.HasPrefix(live.Error, unclearDeliveryPrefix) {
		if err := s.abandonPending(ctx, cli, live, "Dữ liệu đơn đã đổi — thay đơn THE cũ"); err != nil {
			return nil, err
		}
		if live.Status == models.ShipmentLabeled {
			// THE says the "pending" package was delivered (paid) after all: that
			// is the order's shipment. Creating another would pay twice.
			return outcomeOf(live, true), nil
		}
		live = nil
	}

	if live == nil {
		if !opts.SkipAddressCheck {
			if err := s.checkAddress(ctx, cli, parcel); err != nil {
				return nil, err
			}
		}
		live = &models.CarrierShipment{
			OrderID: o.ID, Provider: models.CarrierProviderTHE, Status: models.ShipmentCreating,
			OrderNumber: o.InternalCode, ServiceCode: c.ServiceCode, RequestHash: hash,
			WeightG: parcel.WeightG, LengthCM: parcel.LengthCM, WidthCM: parcel.WidthCM, HeightCM: parcel.HeightCM,
			DeclaredValue: parcel.Value, HSCode: parcel.HSCode, CreatedByID: actor.IDPtr(),
		}
		if err := s.repo.Carrier.ClaimShipment(live); err != nil {
			if other, _ := s.repo.Carrier.LiveShipment(o.ID); other != nil {
				return nil, shipErr(ShipErrWait, "Đơn này đang được tạo trên THE ở lượt khác — đợi vài giây rồi làm mới")
			}
			return nil, apperr.Internal("could not claim shipment").Wrap(err)
		}
	}

	if live.Status == models.ShipmentCreating {
		// New claim, or a stale one whose create never answered: (re)send the
		// create. Safe either way — creating costs nothing and THE returns the
		// pending package it already has for this order number.
		if live.RequestHash != hash {
			_ = s.repo.Carrier.UpdateShipment(live.ID, map[string]any{"request_hash": hash})
			live.RequestHash = hash
		}
		if err := s.createOnTHE(ctx, cli, live, parcel); err != nil {
			return nil, err
		}
	}
	if live.Status == models.ShipmentCreated {
		if err := s.deliverOnTHE(ctx, cli, live); err != nil {
			return nil, err
		}
	}
	if live.Status != models.ShipmentLabeled {
		return nil, apperr.Internal("shipment in unexpected state " + live.Status)
	}
	s.audit.Log(actor, "THE_SHIPMENT_LABELED", "order", &o.ID,
		fmt.Sprintf("Đơn %s tạo trên THE: %s", o.InternalCode, live.TrackingCode),
		models.JSONMap{"shipment_id": live.ID, "the_id": live.ExternalID, "cost": live.Cost})
	return outcomeOf(live, false), nil
}

// FollowUp runs after the handoff is written: the last-mile number is often
// there right after delivery, so ask once now and put it on the order. (Not
// before the handoff — that write saves the whole order row it loaded earlier
// and would wipe a number set in between.) The poll catches the rest.
func (s *CarrierService) FollowUp(ctx context.Context, cli theAPI, orderID uint, out *ShipmentOutcome) {
	if s == nil || cli == nil {
		return
	}
	sh, err := s.repo.Carrier.LiveShipment(orderID)
	if err != nil || sh == nil {
		return
	}
	o, err := s.repo.Order.FindByID(orderID)
	if err != nil {
		return
	}
	s.pullLastMile(ctx, cli, sh, o)
	if out != nil && sh.LastMileTracking != "" {
		out.LastMileTracking = sh.LastMileTracking
	}
}

func outcomeOf(sh *models.CarrierShipment, reused bool) *ShipmentOutcome {
	return &ShipmentOutcome{
		ShipmentID: sh.ID, TrackingCode: sh.TrackingCode, LastMileTracking: sh.LastMileTracking,
		Cost: sh.Cost, HasLabel: sh.TrackingCode != "", Reused: reused,
	}
}

// createOnTHE runs POST /packages for a CREATING row.
func (s *CarrierService) createOnTHE(ctx context.Context, cli theAPI, sh *models.CarrierShipment, p *Parcel) error {
	for attempt := 0; attempt < 2; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, theCreateTimeout)
		pkg, err := cli.CreatePackage(cctx, p.Request)
		cancel()
		if err != nil {
			if theapi.IsTransport(err) {
				_ = s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{"error": "Chưa rõ THE đã tạo đơn chưa: " + err.Error()})
				return shipErr(ShipErrUnclear, "Không nhận được trả lời rõ ràng từ THE khi tạo đơn — gửi lại sau 2 phút (không tạo trùng, chưa mất tiền)")
			}
			msg := theErrorMessage(err)
			_ = s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{"status": models.ShipmentFailed, "error": msg})
			sh.Status = models.ShipmentFailed
			return shipErr(ShipErrTHE, msg)
		}
		// THE answers a create for an order number that still has a PENDING
		// package with THAT package's stored data. If it is not what we sent
		// (left over from an earlier attempt with other data), archive it and
		// create again.
		if stale := staleEcho(pkg, p.Request); stale != "" && attempt == 0 {
			cctx, cancel := context.WithTimeout(ctx, theReadTimeout)
			cerr := cli.CancelPackages(cctx, []string{pkg.ID.String()})
			cancel()
			if cerr != nil {
				msg := "THE trả về một đơn cũ khác dữ liệu (" + stale + ") và chưa huỷ được: " + theErrorMessage(cerr)
				_ = s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{"status": models.ShipmentFailed, "error": msg})
				sh.Status = models.ShipmentFailed
				return shipErr(ShipErrTHE, msg)
			}
			continue
		} else if stale != "" {
			msg := "THE vẫn trả về đơn cũ khác dữ liệu (" + stale + ") — liên hệ quản trị"
			_ = s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{"status": models.ShipmentFailed, "error": msg})
			sh.Status = models.ShipmentFailed
			return shipErr(ShipErrTHE, msg)
		}
		fields := map[string]any{"status": models.ShipmentCreated, "external_id": pkg.ID.String(), "error": ""}
		if cost := float64(pkg.TotalCost); cost > 0 {
			fields["cost"] = cost
			sh.Cost = &cost
		}
		if err := s.repo.Carrier.UpdateShipment(sh.ID, fields); err != nil {
			// THE has the package but FFM could not note it: the row stays
			// CREATING and the next send re-creates — which returns this pending
			// package, nothing is duplicated.
			return apperr.Internal("THE đã tạo đơn nhưng FFM chưa ghi được — gửi lại").Wrap(err)
		}
		sh.Status, sh.ExternalID, sh.Error = models.ShipmentCreated, pkg.ID.String(), ""
		return nil
	}
	return shipErr(ShipErrTHE, "Không tạo được đơn THE")
}

// staleEcho compares what THE says the package holds with what was sent. ""
// = consistent. Tolerances absorb THE's rounding (it rounds sizes up).
func staleEcho(pkg *theapi.Package, r theapi.CreatePackageRequest) string {
	near := func(a, b, tol float64) bool { return math.Abs(a-b) <= tol }
	switch {
	case pkg.Weight > 0 && !near(float64(pkg.Weight), r.Weight, 1):
		return fmt.Sprintf("cân nặng %v ≠ %v", float64(pkg.Weight), r.Weight)
	case pkg.Length > 0 && !near(float64(pkg.Length), r.Length, 1):
		return "kích thước"
	case pkg.Width > 0 && !near(float64(pkg.Width), r.Width, 1):
		return "kích thước"
	case pkg.Height > 0 && !near(float64(pkg.Height), r.Height, 1):
		return "kích thước"
	case pkg.Value > 0 && !near(float64(pkg.Value), r.Value, 0.011):
		return "giá trị khai báo"
	}
	if z := strings.TrimSpace(pkg.Zipcode.String()); z != "" && !strings.EqualFold(z, r.Zipcode) {
		return "zip"
	}
	if a := strings.TrimSpace(pkg.Address1.String()); a != "" && !strings.EqualFold(a, r.Address1) {
		return "địa chỉ"
	}
	// The HS code is compared even when THE's answer has none: an empty one is
	// exactly the hidden package THE leaves behind after refusing an unknown
	// code — paying for it would ship without a customs code.
	if r.HSCode != "" && strings.TrimLeft(strings.TrimSpace(pkg.HSCode.String()), "0") != strings.TrimLeft(r.HSCode, "0") {
		return "mã HS"
	}
	return ""
}

// abandonPending archives a pending (unpaid) THE package and frees the order's
// claim. Refused if THE says it is not pending any more (then it may be paid —
// never walk away from that).
func (s *CarrierService) abandonPending(ctx context.Context, cli theAPI, sh *models.CarrierShipment, why string) error {
	cctx, cancel := context.WithTimeout(ctx, theReadTimeout)
	defer cancel()
	d, err := cli.GetPackage(cctx, sh.ExternalID)
	if err == nil && strings.TrimSpace(d.Code.String()) != "" {
		// Delivered after all: adopt instead of abandoning.
		return s.markLabeled(cctx, cli, sh, d.Code.String(), d.BillCode.String(), "", nil, "")
	}
	if err != nil && !isNotFound(err) {
		return shipErr(ShipErrUnclear, "Chưa kiểm tra được đơn THE cũ: "+theErrorMessage(err)+" — thử lại sau")
	}
	if err == nil {
		if cerr := cli.CancelPackages(cctx, []string{sh.ExternalID}); cerr != nil {
			return shipErr(ShipErrTHE, "Không huỷ được đơn THE cũ (pending): "+theErrorMessage(cerr))
		}
	}
	now := time.Now()
	if err := s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{
		"status": models.ShipmentCancelled, "cancelled_at": now, "error": why,
	}); err != nil {
		return apperr.Internal("could not release shipment").Wrap(err)
	}
	sh.Status = models.ShipmentCancelled
	return nil
}

// deliverOnTHE runs POST /packages/delivery/{id} for a CREATED row — the paid
// step. After an unclear answer it first waits, then asks THE.
func (s *CarrierService) deliverOnTHE(ctx context.Context, cli theAPI, sh *models.CarrierShipment) error {
	if strings.HasPrefix(sh.Error, unclearDeliveryPrefix) {
		if left := unclearDeliveryWait - time.Since(sh.UpdatedAt); left > 0 {
			return shipErr(ShipErrWait, fmt.Sprintf(
				"Lần chốt đơn trước với THE chưa rõ kết quả — đợi %d phút nữa rồi gửi lại (để chắc chắn không trừ tiền 2 lần)",
				int(math.Ceil(left.Minutes()))))
		}
		rctx, cancel := context.WithTimeout(ctx, theReadTimeout)
		d, err := cli.GetPackage(rctx, sh.ExternalID)
		cancel()
		switch {
		case err != nil && isNotFound(err):
			_ = s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{"status": models.ShipmentFailed, "error": "Đơn THE không còn"})
			sh.Status = models.ShipmentFailed
			return shipErr(ShipErrTHE, "Đơn THE cũ không còn trên THE — gửi lại để tạo đơn mới")
		case err != nil:
			return shipErr(ShipErrUnclear, "Chưa kiểm tra được với THE: "+theErrorMessage(err)+" — thử lại sau")
		case strings.TrimSpace(d.Code.String()) != "":
			lctx, cancel := context.WithTimeout(ctx, theReadTimeout)
			defer cancel()
			return s.markLabeled(lctx, cli, sh, d.Code.String(), d.BillCode.String(), "", nil, "")
		case !strings.EqualFold(d.Status.String(), "pending"):
			msg := "Đơn THE đang ở trạng thái " + d.Status.String() + " — không chốt lại"
			_ = s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{"status": models.ShipmentFailed, "error": msg})
			sh.Status = models.ShipmentFailed
			return shipErr(ShipErrTHE, msg)
		}
		// Still pending: the earlier attempt did not charge. Deliver now.
	}

	cctx, cancel := context.WithTimeout(ctx, theDeliverTimeout)
	defer cancel()
	res, err := cli.DeliverPackage(cctx, sh.ExternalID)
	if err != nil {
		var te *theapi.Error
		// "Order … is processing" = another delivery of this package is running
		// on THE right now — as unclear as a timeout.
		if theapi.IsTransport(err) || (errors.As(err, &te) && strings.Contains(strings.ToLower(te.Detail()+" "+te.Message), "processing")) {
			_ = s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{"error": unclearDeliveryPrefix + err.Error()})
			sh.Error = unclearDeliveryPrefix
			return shipErr(ShipErrUnclear, "Không nhận được trả lời rõ ràng từ THE khi chốt đơn — gửi lại sau 10 phút, hệ thống sẽ kiểm tra với THE trước (không trừ tiền 2 lần)")
		}
		msg := theErrorMessage(err)
		// Still pending on THE (unpaid): the next send reuses it (or replaces it
		// when the order's data changed in between).
		_ = s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{"error": msg})
		sh.Error = msg
		return shipErr(ShipErrTHE, msg)
	}
	code := strings.TrimSpace(res.PackageCode.String())
	if code == "" {
		_ = s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{"error": unclearDeliveryPrefix + "THE trả lời không kèm mã"})
		sh.Error = unclearDeliveryPrefix
		return shipErr(ShipErrUnclear, "THE chốt đơn nhưng chưa trả mã — gửi lại sau 10 phút để lấy mã")
	}
	label, ltype := res.Label()
	lctx, lcancel := context.WithTimeout(ctx, theReadTimeout)
	defer lcancel()
	return s.markLabeled(lctx, cli, sh, code, res.BillCode.String(), "", label, ltype)
}

func (s *CarrierService) markLabeled(ctx context.Context, cli theAPI, sh *models.CarrierShipment, code, bill, carrier string, label []byte, ltype string) error {
	code = strings.TrimSpace(code)
	if len(label) == 0 {
		// No usable label in the answer (or an adopted package): fetch it by the
		// THE tracking; printing can also fetch it later.
		if b, t, err := cli.FetchLabel(ctx, code); err == nil && len(b) > 0 {
			label, ltype = b, t
		}
	}
	now := time.Now()
	fields := map[string]any{
		"status": models.ShipmentLabeled, "tracking_code": code, "bill_code": strings.TrimSpace(bill),
		"labeled_at": now, "error": "",
	}
	if carrier != "" {
		fields["last_mile_carrier"] = carrier
	}
	if len(label) > 0 {
		fields["label"] = label
		fields["label_type"] = ltype
	}
	if err := s.repo.Carrier.UpdateShipment(sh.ID, fields); err != nil {
		// Paid on THE but not noted here: keep the "unclear" marker so the next
		// send asks THE (finds the code) instead of paying again.
		_ = s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{"error": unclearDeliveryPrefix + "lưu kết quả thất bại"})
		return apperr.Internal("THE đã chốt đơn " + code + " nhưng FFM chưa ghi được — gửi lại để đồng bộ").Wrap(err)
	}
	sh.Status, sh.TrackingCode, sh.BillCode, sh.LabeledAt, sh.Error = models.ShipmentLabeled, code, bill, &now, ""
	if len(label) > 0 {
		sh.Label, sh.LabelType = label, ltype
	}
	return nil
}

func isNotFound(err error) bool {
	var te *theapi.Error
	return errors.As(err, &te) && te.Status == 404
}

// ---------- last-mile tracking ----------

// pullLastMile asks THE for the last-mile (USPS…) tracking of a labeled
// shipment and, once it exists, puts it on the order — the number the seller
// and CS track by. Best effort: failures leave it for the poll.
func (s *CarrierService) pullLastMile(ctx context.Context, cli theAPI, sh *models.CarrierShipment, o *models.Order) {
	if sh.Status != models.ShipmentLabeled || sh.LastMileTracking != "" || sh.ExternalID == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, theReadTimeout)
	defer cancel()
	d, err := cli.GetPackage(cctx, sh.ExternalID)
	if err != nil {
		return
	}
	number, carrier := d.LastMile()
	if number == "" {
		return
	}
	fields := map[string]any{"last_mile_tracking": number}
	if carrier != "" {
		fields["last_mile_carrier"] = carrier
	}
	if err := s.repo.Carrier.UpdateShipment(sh.ID, fields); err != nil {
		return
	}
	sh.LastMileTracking = number
	s.setOrderTracking(o, number)
}

// setOrderTracking puts the carrier's number on the order unless CS already
// attached one (theirs wins — never overwrite a human's entry).
func (s *CarrierService) setOrderTracking(o *models.Order, number string) {
	if o == nil {
		return
	}
	fresh, err := s.repo.Order.FindByID(o.ID)
	if err != nil || strings.TrimSpace(fresh.TrackingNumber) != "" {
		return
	}
	now := time.Now()
	if err := s.repo.Order.UpdateTracking(fresh.ID, map[string]interface{}{
		"tracking_number": number, "tracking_status": models.TrackingPending, "tracking_updated_at": now,
		"tracking_detail": "", "tracking_location": "", "tracking_raw_status": "",
		"tracking_delivered_at": nil, "tracking_synced_at": nil, "tracking_sync_error": "",
	}); err != nil {
		return
	}
	s.audit.Log(Actor{}, "ORDER_TRACKING_FROM_THE", "order", &fresh.ID,
		"Gắn mã vận đơn từ THE cho "+fresh.InternalCode, models.JSONMap{"tracking_number": number})
	fresh.TrackingNumber = number
	if s.tracking != nil {
		s.tracking.RegisterOrderAsync(fresh)
	}
}

// StartLastMilePoll runs SyncLastMile every interval until ctx ends. It does
// nothing while the integration is off.
func (s *CarrierService) StartLastMilePoll(ctx context.Context, interval time.Duration) {
	if s == nil || interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_, _ = s.SyncLastMile(ctx, 50)
			}
		}
	}()
}

// SyncLastMile is the poll: labeled shipments still without a last-mile
// number ask THE again. Returns how many got one.
func (s *CarrierService) SyncLastMile(ctx context.Context, limit int) (int, error) {
	_, cli, err := s.activeTHE()
	if err != nil || cli == nil {
		return 0, err
	}
	rows, err := s.repo.Carrier.AwaitingLastMile(limit)
	if err != nil {
		return 0, err
	}
	got := 0
	for i := range rows {
		sh := &rows[i]
		// Two weeks without one: the parcel is not going to get one from here.
		if sh.LabeledAt != nil && time.Since(*sh.LabeledAt) > 14*24*time.Hour {
			continue
		}
		o, err := s.repo.Order.FindByID(sh.OrderID)
		if err != nil {
			continue
		}
		s.pullLastMile(ctx, cli, sh, o)
		if sh.LastMileTracking != "" {
			got++
		}
	}
	return got, nil
}
