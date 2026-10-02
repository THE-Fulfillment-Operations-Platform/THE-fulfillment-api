package services

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// OpenAPIService is the business logic behind /api/open/v1 — the endpoints a
// seller's own system calls with an API key instead of a person at a screen.
//
// Three things separate it from the internal order paths, all because the caller
// is a program:
//
//   - The seller is the key's seller. No request field can name another one.
//   - Creating an order is idempotent on the caller's order id: a retry after a
//     timeout returns the order already created instead of a second one.
//   - Nothing is accepted "for a human to fix later". A file import shows its
//     problems on a preview screen; a program has no such screen, so a line with
//     an unknown SKU rejects the whole order with a reason per line.
//
// The item rules themselves are the importer's (validateRow) — one rulebook, so
// an order the file import would refuse is refused here too, and the other way
// round.
type OpenAPIService struct {
	repo    *repositories.Repositories
	audit   *AuditService
	imports *ImportService
}

// MaxOpenOrderItems caps the lines of one order sent through the API.
const MaxOpenOrderItems = 100

// ---------- Request ----------

// OpenShipping is the recipient block, in and out.
type OpenShipping struct {
	Name     string `json:"name"`
	Address1 string `json:"address1"`
	Address2 string `json:"address2"`
	City     string `json:"city"`
	Province string `json:"province"`
	Zip      string `json:"zip"`
	Country  string `json:"country"`
	Phone    string `json:"phone"`
}

// OpenItemInput is one line of an order. Quantity is a pointer so "left out"
// (defaults to 1) can be told from an explicit 0 (an error).
type OpenItemInput struct {
	SKU           string   `json:"sku"`
	Quantity      *FlexInt `json:"quantity"`
	ImageCode     string   `json:"image_code"`
	DesignURL     string   `json:"design_url"`
	BackDesignURL string   `json:"back_design_url"`
	MockupURL     string   `json:"mockup_url"`
	EngraveText   string   `json:"engrave_text"`
}

// OpenOrderInput is the body of POST /api/open/v1/orders.
type OpenOrderInput struct {
	OrderID   string          `json:"order_id"`
	OrderDate string          `json:"order_date"`
	StoreName string          `json:"store_name"`
	Account   string          `json:"account"`
	Shipping  OpenShipping    `json:"shipping"`
	Note      string          `json:"note"`
	Items     []OpenItemInput `json:"items"`
}

// OpenFieldError points at one field of the request. Field is a path into the
// body the caller sent ("shipping.name", "items[2].sku"), so their code can map
// it straight back onto their own data.
type OpenFieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ---------- Response ----------

// OpenOrder is the order as the open API shows it. It is a contract with other
// people's code, so it is its own type rather than a reuse of the seller
// portal's view: that one grows UI fields whenever a screen needs them.
type OpenOrder struct {
	// Code is our internal order code — the number printed on the factory's labels
	// and the one to quote when contacting the factory.
	Code string `json:"code"`
	// OrderID is the caller's own order id, as they sent it.
	OrderID    string `json:"order_id"`
	Status     string `json:"status"`
	StatusText string `json:"status_text"`
	// ReviewNote is what the factory wrote when it sent the order back or refused it.
	ReviewNote         string        `json:"review_note,omitempty"`
	CancellationReason string        `json:"cancellation_reason,omitempty"`
	OrderDate          string        `json:"order_date"`
	StoreName          string        `json:"store_name"`
	Account            string        `json:"account"`
	Shipping           OpenShipping  `json:"shipping"`
	Note               string        `json:"note"`
	Tracking           *OpenTracking `json:"tracking"`
	Items              []OpenItem    `json:"items"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
}

// OpenTracking is the parcel's journey. Like the seller portal, it never names
// the transport partner and carries no provider link.
type OpenTracking struct {
	Number      string     `json:"number"`
	Status      string     `json:"status"`
	Detail      string     `json:"detail"`
	Location    string     `json:"location"`
	UpdatedAt   *time.Time `json:"updated_at"`
	DeliveredAt *time.Time `json:"delivered_at"`
}

// OpenItem is one line of an order.
type OpenItem struct {
	Line          int    `json:"line"`
	SKU           string `json:"sku"`
	ProductName   string `json:"product_name"`
	Quantity      int    `json:"quantity"`
	Status        string `json:"status"` // ACTIVE | CANCELLED
	ImageCode     string `json:"image_code"`
	DesignURL     string `json:"design_url"`
	BackDesignURL string `json:"back_design_url"`
	MockupURL     string `json:"mockup_url"`
	EngraveText   string `json:"engrave_text"`
}

// OpenCreateResult is the answer to a create call. Created=false means this
// order id had already been sent: Order is the existing order, untouched.
type OpenCreateResult struct {
	Order    OpenOrder        `json:"order"`
	Created  bool             `json:"created"`
	Warnings []OpenFieldError `json:"warnings"`
}

// The single status the open API reports. An order has a review state and a
// production state internally; a caller polling for progress needs one answer.
const (
	OpenStatusPendingReview   = "PENDING_REVIEW"
	OpenStatusNeedsCorrection = "NEEDS_CORRECTION"
	OpenStatusRejected        = "REJECTED"
	OpenStatusCancelled       = "CANCELLED"
	OpenStatusInProduction    = "IN_PRODUCTION"
	OpenStatusPacked          = "PACKED"
	OpenStatusHandedOff       = "HANDED_OFF"
	OpenStatusShipped         = "SHIPPED"
	OpenStatusDelivered       = "DELIVERED"
)

var openStatusText = map[string]string{
	OpenStatusPendingReview:   "Chờ xưởng duyệt",
	OpenStatusNeedsCorrection: "Cần sửa thông tin",
	OpenStatusRejected:        "Xưởng từ chối",
	OpenStatusCancelled:       "Đã huỷ",
	OpenStatusInProduction:    "Đang sản xuất",
	OpenStatusPacked:          "Đã đóng gói",
	OpenStatusHandedOff:       "Đã xuất xưởng",
	OpenStatusShipped:         "Đang vận chuyển",
	OpenStatusDelivered:       "Đã giao",
}

// openStatus folds the review state and the production state into one. Review
// decides until the order is approved (every cancellation path also lands on
// review CANCELLED); after that the production phase speaks.
func openStatus(o *models.Order) string {
	switch o.ReviewStatus {
	case models.ReviewCancelled:
		return OpenStatusCancelled
	case models.ReviewPending:
		return OpenStatusPendingReview
	case models.ReviewNeedsFix:
		return OpenStatusNeedsCorrection
	case models.ReviewRejected:
		return OpenStatusRejected
	}
	if itemCancelled(o.CancellationStatus) {
		return OpenStatusCancelled
	}
	switch o.SellerStatus {
	case models.SellerStatusPacked:
		return OpenStatusPacked
	case models.SellerStatusHandedOff:
		return OpenStatusHandedOff
	case models.SellerStatusShipped:
		return OpenStatusShipped
	case models.SellerStatusDelivered:
		return OpenStatusDelivered
	}
	return OpenStatusInProduction
}

// openStatusFilter is openStatus run backwards, for the list's status filter.
// ok=false means the caller asked for a status that does not exist.
func openStatusFilter(status string, f *repositories.OrderFilter) (ok bool) {
	switch status {
	case "":
	case OpenStatusPendingReview:
		f.ReviewStatus = string(models.ReviewPending)
	case OpenStatusNeedsCorrection:
		f.ReviewStatus = string(models.ReviewNeedsFix)
	case OpenStatusRejected:
		f.ReviewStatus = string(models.ReviewRejected)
	case OpenStatusCancelled:
		f.ReviewStatus = string(models.ReviewCancelled)
	case OpenStatusInProduction:
		f.ReviewStatus, f.SellerStatus = string(models.ReviewApproved), string(models.SellerStatusProduction)
	case OpenStatusPacked:
		f.ReviewStatus, f.SellerStatus = string(models.ReviewApproved), string(models.SellerStatusPacked)
	case OpenStatusHandedOff:
		f.ReviewStatus, f.SellerStatus = string(models.ReviewApproved), string(models.SellerStatusHandedOff)
	case OpenStatusShipped:
		f.ReviewStatus, f.SellerStatus = string(models.ReviewApproved), string(models.SellerStatusShipped)
	case OpenStatusDelivered:
		f.ReviewStatus, f.SellerStatus = string(models.ReviewApproved), string(models.SellerStatusDelivered)
	default:
		return false
	}
	return true
}

// toOpenOrder renders an order for the open API. productName resolves a line's
// product name: from the preloaded SKU on the read paths, from the SKU lookup on
// the create path (which has no reason to read the order back).
func toOpenOrder(o *models.Order, productName func(*models.OrderItem) string) OpenOrder {
	status := openStatus(o)
	v := OpenOrder{
		Code: o.InternalCode, OrderID: o.StoreOrderID,
		Status: status, StatusText: openStatusText[status],
		ReviewNote: o.ReviewNote,
		OrderDate:  o.OrderDate, StoreName: o.StoreName, Account: o.Account,
		Shipping: OpenShipping{
			Name: o.ShippingName, Address1: o.ShippingAddress1, Address2: o.ShippingAddress2,
			City: o.ShippingCity, Province: o.ShippingProvince, Zip: o.ShippingZip,
			Country: o.ShippingCountry, Phone: o.ShippingPhone,
		},
		Note:      o.Note,
		Items:     make([]OpenItem, 0, len(o.Items)),
		CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	}
	if status == OpenStatusCancelled {
		v.CancellationReason = o.CancellationReason
	}
	hasState := o.TrackingStatus != "" && o.TrackingStatus != models.TrackingNone
	if o.TrackingNumber != "" || hasState {
		t := &OpenTracking{
			Number: o.TrackingNumber, Detail: redactPartner(o.TrackingDetail),
			UpdatedAt: o.TrackingUpdatedAt, DeliveredAt: o.TrackingDeliveredAt,
		}
		// NONE is "nothing recorded", not a shipment state; a location only means
		// something next to a real one. Same rule as the seller portal.
		if hasState {
			t.Status = string(o.TrackingStatus)
			t.Location = redactPartner(o.TrackingLocation)
		}
		v.Tracking = t
	}
	for i := range o.Items {
		it := &o.Items[i]
		line := OpenItem{
			Line: it.LineNo, SKU: it.SKUCode, ProductName: productName(it), Quantity: it.Quantity,
			Status:    "ACTIVE",
			ImageCode: it.ImageCode, DesignURL: it.DesignURL, BackDesignURL: it.BackDesignURL,
			MockupURL: it.MockupURL, EngraveText: it.EngraveText,
		}
		if itemCancelled(it.CancellationStatus) {
			line.Status = "CANCELLED"
		}
		v.Items = append(v.Items, line)
	}
	return v
}

func preloadedProductName(it *models.OrderItem) string { return skuProductName(it.SKU) }

// ---------- Validation ----------

type openFieldErrors []OpenFieldError

func (e *openFieldErrors) add(field, code, message string) {
	*e = append(*e, OpenFieldError{Field: field, Code: code, Message: message})
}

// text checks one free-text field: required-ness and the column's length (in
// characters — the database counts characters, and these fields hold Vietnamese).
func (e *openFieldErrors) text(field, value string, required bool, max int) {
	if value == "" {
		if required {
			e.add(field, "REQUIRED", "Thiếu "+field)
		}
		return
	}
	if utf8.RuneCountInString(value) > max {
		e.add(field, "TOO_LONG", fmt.Sprintf("%s dài quá %d ký tự", field, max))
	}
}

// openItemRule maps the importer's row verdict onto the open API's vocabulary:
// the field as the caller named it, a code that says what to do about it, and a
// message written for the seller's developer rather than for the factory's
// master-data operator (the importer's own suggestions point at screens the
// caller cannot open).
var openItemRule = map[string]struct{ field, code, message string }{
	"QTY_INVALID":         {"quantity", "QUANTITY_INVALID", "Số lượng phải là số nguyên từ 1 trở lên"},
	"SKU_MISSING":         {"sku", "REQUIRED", "Thiếu mã SKU"},
	"SKU_UNMAPPED":        {"sku", "SKU_NOT_FOUND", "SKU không có trên hệ thống của xưởng — kiểm tra lại mã, hoặc báo xưởng khai SKU này"},
	"SKU_NO_MATERIAL":     {"sku", "SKU_NOT_READY", "SKU có trên hệ thống nhưng xưởng chưa khai nguyên vật liệu nên chưa sản xuất được — báo xưởng"},
	"MOCKUP_INVALID":      {"mockup_url", "URL_INVALID", "mockup_url phải là link http(s) hợp lệ"},
	"DESIGN_INVALID":      {"design_url", "URL_INVALID", "design_url phải là link http(s) hợp lệ"},
	"BACK_DESIGN_INVALID": {"back_design_url", "URL_INVALID", "back_design_url phải là link http(s) hợp lệ"},
	"DESIGN_SIDE_DUP":     {"back_design_url", "DESIGN_SIDE_DUPLICATE", "design_url và back_design_url đang trùng nhau — mỗi mặt một file, hoặc bỏ trống mặt sau"},
}

// normalize trims every string of the request in place, so validation, the
// idempotency key and what gets stored all see the same value.
func (in *OpenOrderInput) normalize() {
	t := strings.TrimSpace
	in.OrderID, in.OrderDate, in.StoreName, in.Account, in.Note = t(in.OrderID), t(in.OrderDate), t(in.StoreName), t(in.Account), t(in.Note)
	s := &in.Shipping
	s.Name, s.Address1, s.Address2, s.City = t(s.Name), t(s.Address1), t(s.Address2), t(s.City)
	s.Province, s.Zip, s.Country, s.Phone = t(s.Province), t(s.Zip), t(s.Country), t(s.Phone)
	for i := range in.Items {
		it := &in.Items[i]
		it.SKU, it.ImageCode, it.EngraveText = t(it.SKU), t(it.ImageCode), t(it.EngraveText)
		it.DesignURL, it.BackDesignURL, it.MockupURL = t(it.DesignURL), t(it.BackDesignURL), t(it.MockupURL)
	}
}

// orderDate resolves the business day the order files under: the caller's
// order_date, or today in the factory's timezone. Strictly YYYY-MM-DD — the
// importer's forgiving date reader exists for people typing into Excel; a
// program should not be guessed at.
func (in *OpenOrderInput) orderDate(now time.Time, errs *openFieldErrors) string {
	if in.OrderDate == "" {
		return AppDateString(now)
	}
	if _, err := time.Parse("2006-01-02", in.OrderDate); err != nil {
		errs.add("order_date", "DATE_INVALID", "order_date phải theo dạng YYYY-MM-DD, ví dụ 2026-10-01")
		return ""
	}
	return in.OrderDate
}

// rows converts the request into importer rows, one per item — the shape the
// shared item rules and the shared item builder both take.
func (in *OpenOrderInput) rows() []ImportRow {
	rows := make([]ImportRow, 0, len(in.Items))
	for _, it := range in.Items {
		qty := FlexInt(1)
		if it.Quantity != nil {
			qty = *it.Quantity
		}
		rows = append(rows, ImportRow{
			StoreOrderID: in.OrderID, Account: in.Account, StoreName: in.StoreName,
			Quantity: qty, SKU: it.SKU, ImageCode: it.ImageCode,
			FrontDesign: it.DesignURL, BackDesign: it.BackDesignURL, Mockup: it.MockupURL,
			EngraveText:  it.EngraveText,
			ShippingName: in.Shipping.Name, ShippingAddress1: in.Shipping.Address1,
			ShippingAddress2: in.Shipping.Address2, ShippingCity: in.Shipping.City,
			ShippingZip: in.Shipping.Zip, ShippingProvince: in.Shipping.Province,
			ShippingCountry: in.Shipping.Country, ShippingPhone: in.Shipping.Phone,
			Note: in.Note,
		})
	}
	return rows
}

// validate checks the whole request and reports every problem at once, so the
// caller fixes their payload in one round instead of one error per attempt.
func (s *OpenAPIService) validate(in *OpenOrderInput, rows []ImportRow, skus map[string]repositories.SKUInfo, seller repositories.SellerIdentity, now time.Time) (orderDate string, errs openFieldErrors) {
	errs.text("order_id", in.OrderID, true, 120)
	orderDate = in.orderDate(now, &errs)
	errs.text("store_name", in.StoreName, false, 160)
	errs.text("account", in.Account, false, 120)
	errs.text("note", in.Note, false, 1000)
	errs.text("shipping.name", in.Shipping.Name, true, 160)
	errs.text("shipping.address1", in.Shipping.Address1, true, 255)
	errs.text("shipping.address2", in.Shipping.Address2, false, 255)
	errs.text("shipping.city", in.Shipping.City, false, 120)
	errs.text("shipping.province", in.Shipping.Province, false, 120)
	errs.text("shipping.zip", in.Shipping.Zip, false, 40)
	errs.text("shipping.country", in.Shipping.Country, true, 80)
	errs.text("shipping.phone", in.Shipping.Phone, false, 60)

	switch {
	case len(in.Items) == 0:
		errs.add("items", "REQUIRED", "Đơn phải có ít nhất 1 sản phẩm")
	case len(in.Items) > MaxOpenOrderItems:
		errs.add("items", "TOO_MANY", fmt.Sprintf("Một đơn tối đa %d dòng sản phẩm", MaxOpenOrderItems))
		return orderDate, errs
	}

	for i, row := range rows {
		at := func(field string) string { return fmt.Sprintf("items[%d].%s", i, field) }
		it := in.Items[i]
		errs.text(at("sku"), it.SKU, false, 48) // "missing" is the importer's SKU_MISSING below
		errs.text(at("image_code"), it.ImageCode, false, 120)
		errs.text(at("design_url"), it.DesignURL, false, 500)
		errs.text(at("back_design_url"), it.BackDesignURL, false, 500)
		errs.text(at("mockup_url"), it.MockupURL, false, 500)
		errs.text(at("engrave_text"), it.EngraveText, false, 500)

		// The importer's rulebook, item rules only: the order-level fields were
		// judged above (with the caller's field names), so they are stubbed valid
		// here and can never be what validateRow stops on.
		row.StoreOrderID, row.OrderDateRaw, row.SellerRef = "-", "", ""
		row.ShippingName, row.ShippingAddress1, row.ShippingCountry = "-", "-", "-"
		if rowErr := s.imports.validateRow(i+1, row, skus, seller); rowErr != nil {
			if rule, ok := openItemRule[rowErr.ErrorCode]; ok {
				errs.add(at(rule.field), rule.code, rule.message)
			} else {
				errs.add(at(strings.ToLower(rowErr.Field)), rowErr.ErrorCode, rowErr.Message)
			}
		}
	}
	return orderDate, errs
}

func openValidationError(errs openFieldErrors) error {
	return apperr.New(http.StatusUnprocessableEntity, "VALIDATION_ERROR",
		fmt.Sprintf("Đơn chưa hợp lệ: %d lỗi — xem error.details", len(errs))).WithDetails([]OpenFieldError(errs))
}

// ---------- Create ----------

func apiActor(p *models.APIPrincipal, ip string) Actor {
	sellerID := p.SellerID
	// No user id: nobody is logged in. The key's prefix is what the audit trail
	// names, so a revoked key's past orders stay attributable.
	return Actor{Email: "api-key:" + p.KeyPrefix, Role: models.RoleSeller, SellerID: &sellerID, IP: ip}
}

// CreateOrder creates one order for the key's seller, or returns the order this
// order id already created.
func (s *OpenAPIService) CreateOrder(p *models.APIPrincipal, ip string, in OpenOrderInput) (*OpenCreateResult, error) {
	in.normalize()
	seller := repositories.SellerIdentity{ID: p.SellerID, Code: p.SellerCode, Name: p.SellerName}

	// Idempotency first: a replay of an order that already exists must succeed
	// even if master data changed since (a SKU retired yesterday must not turn
	// today's retry of last week's order into an error).
	if in.OrderID != "" {
		existing, err := s.existing(p.SellerID, in.OrderID)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return &OpenCreateResult{Order: *existing, Created: false, Warnings: []OpenFieldError{}}, nil
		}
	}

	rows := in.rows()
	skus, err := s.imports.skuInfoForRows(rows)
	if err != nil {
		return nil, apperr.Internal("could not look up SKUs").Wrap(err)
	}
	now := time.Now()
	orderDate, errs := s.validate(&in, rows, skus, seller, now)
	if len(errs) > 0 {
		return nil, openValidationError(errs)
	}

	actor := apiActor(p, ip)
	var order models.Order
	var items []models.OrderItem
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		seq, seqErr := txRepo.Order.NextDailySeq(orderDate, now)
		if seqErr != nil {
			return seqErr
		}
		// internal_code is UNIQUE and derived from the id the insert assigns, so the
		// insert carries a throwaway that cannot collide with a concurrent request's.
		placeholder, phErr := openCodePlaceholder()
		if phErr != nil {
			return phErr
		}
		ref := in.OrderID
		order = models.Order{
			InternalCode: placeholder,
			StoreOrderID: in.OrderID, StoreOrderRef: in.OrderID, APIRef: &ref,
			SellerID: p.SellerID, StoreName: in.StoreName, Account: in.Account,
			ShippingName: in.Shipping.Name, ShippingAddress1: in.Shipping.Address1,
			ShippingAddress2: in.Shipping.Address2, ShippingCity: in.Shipping.City,
			ShippingZip: in.Shipping.Zip, ShippingProvince: in.Shipping.Province,
			ShippingCountry: in.Shipping.Country, ShippingPhone: in.Shipping.Phone,
			Note:         in.Note,
			SellerStatus: models.SellerStatusProduction,
			// Same door as every other new order: reviewed by the factory before it
			// enters design/production.
			ReviewStatus:       models.ReviewPending,
			CancellationStatus: models.CancellationNone,
			TrackingStatus:     models.TrackingNone,
			OrderDate:          orderDate, DailySeq: seq,
		}
		if err := txRepo.Order.Create(&order); err != nil {
			return err
		}
		order.InternalCode = internalBaseCode(order.ID)
		if err := tx.Model(&models.Order{}).Where("id = ?", order.ID).
			UpdateColumn("internal_code", order.InternalCode).Error; err != nil {
			return err
		}
		items = orderItemsFromRows(order.ID, rows, skus)
		if err := tx.Create(&items).Error; err != nil {
			return err
		}
		assets, notes := itemAssetsAndNotes(items, actor)
		if len(assets) > 0 {
			if err := tx.Create(&assets).Error; err != nil {
				return err
			}
		}
		if len(notes) > 0 {
			if err := tx.Create(&notes).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Two identical requests can both pass the lookup above; the unique index
		// on (seller, api_ref) lets one insert through and fails the other. The
		// loser is a replay like any other — answer it with the winner's order.
		if existing, lookupErr := s.existing(p.SellerID, in.OrderID); lookupErr == nil && existing != nil {
			return &OpenCreateResult{Order: *existing, Created: false, Warnings: []OpenFieldError{}}, nil
		}
		return nil, apperr.Internal("could not create order").Wrap(err)
	}

	s.audit.Log(actor, "ORDER_CREATE_API", "order", &order.ID,
		"Created order "+order.InternalCode+" via API ("+in.OrderID+")",
		map[string]interface{}{"api_key_id": p.KeyID, "api_key_name": p.KeyName, "items": len(items)})

	order.Items = items
	names := func(it *models.OrderItem) string { return skus[it.SKUCode].ProductName }
	res := &OpenCreateResult{Order: toOpenOrder(&order, names), Created: true, Warnings: []OpenFieldError{}}
	// Accepted, but worth a second look — the importer warns about the same thing.
	if zipStateSwapped(in.Shipping.Zip, in.Shipping.Province) {
		res.Warnings = append(res.Warnings, OpenFieldError{
			Field: "shipping.zip", Code: "ZIP_STATE_SWAPPED",
			Message: "shipping.zip đang là mã bang còn shipping.province là dãy số — có vẻ hai trường bị điền ngược",
		})
	}
	return res, nil
}

func openCodePlaceholder() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "TMP-API-" + hex.EncodeToString(buf), nil
}

// existing returns the order the seller's system already created under ref, or
// nil when there is none.
func (s *OpenAPIService) existing(sellerID uint, ref string) (*OpenOrder, error) {
	_, found, err := s.repo.Order.IDByAPIRef(sellerID, ref)
	if err != nil {
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	if !found {
		return nil, nil
	}
	return s.GetOrder(sellerID, ref)
}

// ---------- Read ----------

var errOpenOrderNotFound = apperr.New(http.StatusNotFound, "ORDER_NOT_FOUND", "Không tìm thấy đơn này")

// GetOrder returns one of the seller's orders by the caller's order id (or our
// internal code). Another seller's order is "not found", never "forbidden":
// the answer must not confirm that an id exists elsewhere.
func (s *OpenAPIService) GetOrder(sellerID uint, ref string) (*OpenOrder, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errOpenOrderNotFound
	}
	o, err := s.repo.Order.FindForOpenAPI(sellerID, ref)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errOpenOrderNotFound
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	v := toOpenOrder(o, preloadedProductName)
	return &v, nil
}

// OpenOrderQuery is the list's filter, as the caller sends it.
type OpenOrderQuery struct {
	Page repositories.Page
	// OrderID narrows the list to the one order with this id (exact, same
	// resolution as GetOrder); the other filters are then ignored.
	OrderID     string
	Status      string
	CreatedFrom *time.Time
	CreatedTo   *time.Time
}

// MaxOpenPageSize caps a list page. The internal API's "page_size=-1 means
// everything" is not offered here: a caller that polls would pull the seller's
// whole history every time.
const MaxOpenPageSize = 100

// ListOrders pages the seller's orders, newest first.
func (s *OpenAPIService) ListOrders(sellerID uint, q OpenOrderQuery) ([]OpenOrder, repositories.Page, int64, error) {
	page := q.Page
	if page.PageSize < 1 {
		page.PageSize = 20
	}
	if page.PageSize > MaxOpenPageSize {
		page.PageSize = MaxOpenPageSize
	}
	page = page.Normalize()

	if ref := strings.TrimSpace(q.OrderID); ref != "" {
		o, err := s.GetOrder(sellerID, ref)
		if err != nil {
			if errors.Is(err, errOpenOrderNotFound) {
				return []OpenOrder{}, page, 0, nil
			}
			return nil, page, 0, err
		}
		return []OpenOrder{*o}, page, 1, nil
	}

	f := repositories.OrderFilter{Page: page, SellerID: &sellerID, DateFrom: q.CreatedFrom, DateTo: q.CreatedTo}
	if !openStatusFilter(strings.ToUpper(strings.TrimSpace(q.Status)), &f) {
		return nil, page, 0, apperr.New(http.StatusBadRequest, "STATUS_INVALID",
			"status không hợp lệ — xem danh sách trạng thái trong tài liệu")
	}
	rows, total, err := s.repo.Order.ListForOpenAPI(f)
	if err != nil {
		return nil, page, 0, apperr.Internal("could not list orders").Wrap(err)
	}
	out := make([]OpenOrder, 0, len(rows))
	for i := range rows {
		out = append(out, toOpenOrder(&rows[i], preloadedProductName))
	}
	return out, page, total, nil
}
