package models

import "time"

// Carrier integration with THE (thehuman.express): when the factory sends a
// finished order, FFM creates the shipment on THE through THE's customer API,
// gets back the THE tracking + the label, and prints the label here.
//
// Real money: confirming a THE shipment debits the factory's THE wallet. Every
// structure below exists so that a click, a retry, a timeout or a double scan
// can never produce two paid shipments for one order.

// CarrierProviderTHE is the only provider today.
const CarrierProviderTHE = "THE"

// CarrierConfig is the connection to a carrier's API — one row per provider.
// The API token is stored sealed (secretbox); only its last characters
// (TokenHint) ever leave the server, so the settings screen can show which
// token is configured without being able to leak it.
type CarrierConfig struct {
	Base
	Provider    string `json:"provider" gorm:"size:20;uniqueIndex;not null"`
	Enabled     bool   `json:"enabled" gorm:"not null;default:false"`
	BaseURL     string `json:"base_url" gorm:"size:200;not null;default:''"`
	TokenSealed string `json:"-" gorm:"type:text;not null;default:''"`
	TokenHint   string `json:"token_hint" gorm:"size:12;not null;default:''"`
	// ServiceCode is the THE service every shipment is created with (from THE's
	// GET /services list).
	ServiceCode string `json:"service_code" gorm:"size:60;not null;default:''"`
	// DefaultPhone is sent when the order has no recipient phone — almost every
	// imported order (the seller template has no phone column in practice).
	DefaultPhone string `json:"default_phone" gorm:"size:40;not null;default:''"`
	// PackagingWeightG is added once per parcel on top of the products' weight
	// (box, padding).
	PackagingWeightG float64 `json:"packaging_weight_g" gorm:"not null;default:0"`
	// Customs defaults, set by the carrier side (THE): the factory knows its
	// products' weight and box, not THE's HS code table. A SKU that declares
	// its own HS code / value overrides these. "" / 0 = no default.
	DefaultHSCode        string  `json:"default_hs_code" gorm:"size:20;not null;default:''"`
	DefaultDeclaredValue float64 `json:"default_declared_value" gorm:"not null;default:0"`
	UpdatedByID          *uint   `json:"updated_by_id"`
}

func (CarrierConfig) TableName() string { return "carrier_configs" }

// CarrierShipment states. The lifecycle mirrors THE's two calls:
//
//	CREATING  row claimed, POST /packages in flight (or crashed mid-flight)
//	CREATED   THE package exists (pending, not paid) — ExternalID known
//	LABELED   POST /packages/delivery done: paid, THE tracking + label issued
//	CANCELLED cancelled on THE (or abandoned before it existed there)
//	FAILED    THE refused the package; nothing exists there, safe to retry
//
// Only LABELED means "the order has a paid shipment". A CREATING row whose
// call never returned is reconciled against THE before anything is resent.
const (
	ShipmentCreating  = "CREATING"
	ShipmentCreated   = "CREATED"
	ShipmentLabeled   = "LABELED"
	ShipmentCancelled = "CANCELLED"
	ShipmentFailed    = "FAILED"
)

// CarrierShipment is one shipment FFM created (or tried to create) on the
// carrier for one order. At most one row per order is "live" (any state but
// CANCELLED/FAILED) — enforced by a partial unique index (migrate.go), which is
// what makes a double click or a concurrent scan unable to create a second
// paid shipment.
type CarrierShipment struct {
	Base
	OrderID  uint   `json:"order_id" gorm:"not null;index"`
	Provider string `json:"provider" gorm:"size:20;not null"`
	Status   string `json:"status" gorm:"size:20;not null;index"`
	// OrderNumber is what FFM sent as THE's order_number (the order's internal
	// code) — the key to find the package on THE again after a lost response.
	OrderNumber string `json:"order_number" gorm:"size:60;not null"`
	// ExternalID is THE's package id (needed for delivery / cancel).
	ExternalID string `json:"external_id" gorm:"size:40;not null;default:''"`
	// TrackingCode is THE's tracking ("THE…"), issued at delivery.
	TrackingCode     string `json:"tracking_code" gorm:"size:60;not null;default:'';index"`
	LastMileCarrier  string `json:"last_mile_carrier" gorm:"size:40;not null;default:''"`
	LastMileTracking string `json:"last_mile_tracking" gorm:"size:60;not null;default:''"`
	BillCode         string `json:"bill_code" gorm:"size:60;not null;default:''"`
	ServiceCode      string `json:"service_code" gorm:"size:60;not null;default:''"`
	// What was declared to THE for this parcel.
	WeightG       float64 `json:"weight_g" gorm:"not null;default:0"`
	LengthCM      float64 `json:"length_cm" gorm:"not null;default:0"`
	WidthCM       float64 `json:"width_cm" gorm:"not null;default:0"`
	HeightCM      float64 `json:"height_cm" gorm:"not null;default:0"`
	DeclaredValue float64 `json:"declared_value" gorm:"not null;default:0"`
	HSCode        string  `json:"hs_code" gorm:"size:20;not null;default:''"`
	// RequestHash fingerprints the exact create request sent to THE. When the
	// order or its SKU data changes before the paid step (address fixed, weight
	// corrected), the next send sees a different fingerprint and replaces the
	// stale pending package instead of paying for one with the old data.
	RequestHash string `json:"-" gorm:"size:64;not null;default:''"`
	// Cost THE quoted when the package was created (USD).
	Cost *float64 `json:"cost"`
	// Label is the label file THE returned at delivery, kept so printing never
	// depends on THE being reachable at that moment. LabelType is its MIME type.
	Label     []byte `json:"-"`
	LabelType string `json:"label_type" gorm:"size:40;not null;default:''"`
	// Error is the last refusal/failure in the operator's words.
	Error       string     `json:"error" gorm:"size:1000;not null;default:''"`
	CreatedByID *uint      `json:"created_by_id"`
	LabeledAt   *time.Time `json:"labeled_at"`
	CancelledAt *time.Time `json:"cancelled_at"`
}

func (CarrierShipment) TableName() string { return "carrier_shipments" }

// Live reports whether the row still stands for a (possible) shipment on the
// carrier — the states the one-per-order index covers.
func (s *CarrierShipment) Live() bool {
	return s.Status != ShipmentCancelled && s.Status != ShipmentFailed
}
