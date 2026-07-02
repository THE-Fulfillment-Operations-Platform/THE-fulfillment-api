// Package shipping defines the carrier integration boundary. The MVP does NOT
// call any real THE / carrier API: packing + handoff are recorded locally only.
//
// The Carrier interface below is the seam for the later phase. When THE exposes a
// real label/tracking API, implement Carrier with a concrete client and swap the
// NoopCarrier out in main.go — no service code needs to change.
package shipping

import (
	"context"
	"fmt"
	"time"
)

// LabelRequest is what a real carrier would need to create a label.
type LabelRequest struct {
	HandoffCode    string
	OrderCode      string
	RecipientName  string
	Address1       string
	Address2       string
	City           string
	Province       string
	Zip            string
	Country        string
	Phone          string
	WeightGrams    int
	ShippingMethod string
}

// LabelResult is the carrier's response. In phase 2 this carries the real
// tracking number and label URL.
type LabelResult struct {
	TrackingNumber string
	LabelURL       string
	Supported      bool // false in the MVP: no real integration yet
}

// Carrier is the integration boundary for THE / shipping providers.
type Carrier interface {
	// Name identifies the carrier, e.g. "THE".
	Name() string
	// CreateLabel attempts to create a shipping label. In the MVP it returns
	// Supported=false so the handoff is recorded as a manual bàn giao.
	CreateLabel(ctx context.Context, req LabelRequest) (LabelResult, error)
}

// NoopCarrier is the MVP carrier: it never calls an external API. It simply
// records that a handoff happened and reports that label/tracking is not yet
// supported, matching the wireframe ("nếu chưa connect: bàn giao THE").
type NoopCarrier struct {
	carrierName string
}

// NewNoopCarrier builds the placeholder carrier used in the MVP.
func NewNoopCarrier(name string) *NoopCarrier {
	if name == "" {
		name = "THE"
	}
	return &NoopCarrier{carrierName: name}
}

func (c *NoopCarrier) Name() string { return c.carrierName }

// CreateLabel is a deliberate no-op for the MVP. TODO(phase-2): replace with a
// real THE API client that returns a live tracking number + label URL.
func (c *NoopCarrier) CreateLabel(_ context.Context, req LabelRequest) (LabelResult, error) {
	_ = time.Now    // reserved: phase-2 implementation will stamp request time
	_ = fmt.Sprintf // reserved: phase-2 implementation will format provider payloads
	return LabelResult{Supported: false}, nil
}
