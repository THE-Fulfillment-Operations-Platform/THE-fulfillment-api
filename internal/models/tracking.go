package models

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// OrderTrackingEvent is one carrier scan in a shipment's journey, mirrored from
// the tracking provider (24hTrack) into our own database.
//
// The timeline is stored rather than fetched per page view for two reasons: the
// provider rate-limits per minute (so a list screen rendering 50 orders could
// never call it inline), and a shipment's history must stay readable after the
// provider expires or archives the parcel.
type OrderTrackingEvent struct {
	Base
	OrderID uint `json:"order_id" gorm:"index;not null"`
	// TrackingNumber is denormalised so the timeline survives a tracking-number
	// correction on the order: the old number's scans stay attached to the number
	// they actually happened on, and SyncOrder replaces only the current number's.
	TrackingNumber string `json:"tracking_number" gorm:"size:120;index;not null"`

	// EventAt is the scan time parsed into a real timestamp for sorting. The
	// provider returns human strings ("July 27, 2026 9:53 PM") whose format varies
	// per carrier, so RawDate keeps the original for display when parsing fails.
	EventAt     *time.Time `json:"event_at" gorm:"index"`
	RawDate     string     `json:"raw_date" gorm:"size:120"`
	Location    string     `json:"location" gorm:"size:255"`
	Description string     `json:"description" gorm:"size:500"`
	StatusHint  string     `json:"status_hint" gorm:"size:120"`

	// Fingerprint dedupes re-imported scans. Every poll returns the WHOLE
	// timeline, so without a unique key each sync would append the same events
	// again. It hashes the fields that identify a scan; the unique index lets the
	// insert use ON CONFLICT DO NOTHING instead of a read-then-write race.
	Fingerprint string `json:"-" gorm:"size:64;uniqueIndex;not null"`
}

func (OrderTrackingEvent) TableName() string { return "order_tracking_events" }

// EventFingerprint builds the dedupe key for a scan. Location is included
// because carriers legitimately repeat a description ("In Transit") at different
// facilities, and the raw date because the same facility can scan twice a day.
func EventFingerprint(trackingNumber, rawDate, description, location string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.TrimSpace(trackingNumber),
		strings.TrimSpace(rawDate),
		strings.TrimSpace(description),
		strings.TrimSpace(location),
	}, "|")))
	return hex.EncodeToString(sum[:])
}
