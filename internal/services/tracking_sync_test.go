package services

import (
	"testing"
	"time"

	"the-fulfillment/backend/internal/models"
)

// The provider hands back human date strings whose shape varies per carrier.
// Getting these wrong silently scrambles the order of a shipment's journey, so
// every observed shape is pinned here.
func TestParseProviderTime(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time // zero ⇒ expect nil
	}{
		{"usps with PM", "July 27, 2026 9:53 PM", time.Date(2026, 7, 27, 21, 53, 0, 0, time.UTC)},
		{"comma before time", "March 17, 2026, 2:15 pm", time.Date(2026, 3, 17, 14, 15, 0, 0, time.UTC)},
		{"midnight is 12 AM", "January 1, 2026, 12:05 AM", time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)},
		{"noon stays 12 PM", "January 1, 2026, 12:05 PM", time.Date(2026, 1, 1, 12, 5, 0, 0, time.UTC)},
		{"date only", "December 6, 2025", time.Date(2025, 12, 6, 0, 0, 0, 0, time.UTC)},
		{"trailing carrier noise", "July 26, 2026 12:46 PM Shipping Partner:", time.Date(2026, 7, 26, 12, 46, 0, 0, time.UTC)},
		{"24h clock without meridiem", "July 26, 2026 18:46", time.Date(2026, 7, 26, 18, 46, 0, 0, time.UTC)},
		{"abbreviated month", "Sep 3, 2026 7:00 AM", time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC)},
		{"iso", "2026-03-08T01:08:00", time.Date(2026, 3, 8, 1, 8, 0, 0, time.UTC)},
		{"empty", "", time.Time{}},
		{"unparseable", "sometime last week", time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseProviderTime(tc.in)
			if tc.want.IsZero() {
				if got != nil {
					t.Fatalf("parseProviderTime(%q) = %v, want nil", tc.in, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("parseProviderTime(%q) = nil, want %v", tc.in, tc.want)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("parseProviderTime(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// An unknown provider status must never reach the database as an invalid enum
// value — the column is what the UI badge switches on.
func TestNormalizeProviderStatus(t *testing.T) {
	cases := map[string]models.TrackingStatus{
		"Delivered":        models.TrackingDelivered,
		"In Transit":       models.TrackingInTransit,
		"Out for Delivery": models.TrackingOutForDelivery,
		"Pick Up":          models.TrackingPickUp,
		"Info Received":    models.TrackingPreTransit,
		"Undelivered":      models.TrackingUndelivered,
		"Alert":            models.TrackingException,
		"Expired":          models.TrackingExpired,
		"Not Found":        models.TrackingPending,
		"Queued":           models.TrackingPending,
		"  delivered  ":    models.TrackingDelivered,
		"":                 models.TrackingPending,
		"something new":    models.TrackingPending,
	}
	for in, want := range cases {
		got := normalizeProviderStatus(in)
		if got != want {
			t.Errorf("normalizeProviderStatus(%q) = %s, want %s", in, got, want)
		}
		if !got.Valid() {
			t.Errorf("normalizeProviderStatus(%q) produced invalid status %s", in, got)
		}
	}
}

// Only states the carrier will never move on from may stop the sync loop —
// marking a live parcel terminal would freeze its journey.
func TestTrackingStatusTerminal(t *testing.T) {
	terminal := []models.TrackingStatus{models.TrackingDelivered, models.TrackingExpired, models.TrackingCancelled}
	live := []models.TrackingStatus{
		models.TrackingNone, models.TrackingPending, models.TrackingPreTransit,
		models.TrackingInTransit, models.TrackingOutForDelivery, models.TrackingPickUp,
		models.TrackingUndelivered, models.TrackingException,
	}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range live {
		if s.Terminal() {
			t.Errorf("%s should NOT be terminal", s)
		}
	}
}

// The fingerprint is the only thing stopping every poll from re-inserting the
// whole timeline, so it must be stable AND discriminating.
func TestEventFingerprint(t *testing.T) {
	base := models.EventFingerprint("940011", "July 27, 2026 9:53 PM", "In Transit", "JAMAICA, NY")
	if base != models.EventFingerprint("940011", "July 27, 2026 9:53 PM", "In Transit", "JAMAICA, NY") {
		t.Fatal("fingerprint is not stable for identical input")
	}
	// Whitespace differences from the provider must not create a duplicate scan.
	if base != models.EventFingerprint(" 940011 ", " July 27, 2026 9:53 PM ", " In Transit ", " JAMAICA, NY ") {
		t.Fatal("fingerprint should ignore surrounding whitespace")
	}
	// The same description at a different facility is a different scan.
	if base == models.EventFingerprint("940011", "July 27, 2026 9:53 PM", "In Transit", "TORRANCE, CA") {
		t.Fatal("fingerprint must distinguish locations")
	}
	if base == models.EventFingerprint("940011", "July 28, 2026 9:53 PM", "In Transit", "JAMAICA, NY") {
		t.Fatal("fingerprint must distinguish scan times")
	}
}

// The description is the ONLY link between a store order id and a parcel; a
// change in its shape orphans everything already tagged.
func TestDescriptionFor(t *testing.T) {
	s := NewTrackingSyncService(nil, nil, nil, "FFM", true)
	got := s.descriptionFor(&models.Order{StoreOrderID: " SO-1234 "})
	if got != "FFM:SO-1234" {
		t.Fatalf("descriptionFor = %q, want %q", got, "FFM:SO-1234")
	}
	// An empty tag must still produce a usable prefix rather than ":SO-1234".
	if d := NewTrackingSyncService(nil, nil, nil, "  ", true).descriptionFor(&models.Order{StoreOrderID: "SO-1"}); d != "FFM:SO-1" {
		t.Fatalf("empty tag fell back to %q", d)
	}
}

// A disabled integration must be inert rather than panicking on a nil client —
// every push/resolve entry point is called unconditionally by its caller.
func TestDisabledIntegrationIsInert(t *testing.T) {
	var s *TrackingSyncService
	if s.Enabled() {
		t.Fatal("nil service reported enabled")
	}
	s.RegisterOrderAsync(&models.Order{TrackingNumber: "940011"}) // must not panic

	off := NewTrackingSyncService(nil, nil, nil, "FFM", true)
	if off.Enabled() {
		t.Fatal("service without a client reported enabled")
	}
	off.RegisterOrderAsync(&models.Order{TrackingNumber: "940011"})
	if err := off.RegisterOrder(t.Context(), &models.Order{TrackingNumber: "940011"}); err != nil {
		t.Fatalf("RegisterOrder on a disabled service returned %v", err)
	}
	if number, err := off.ResolveOrder(t.Context(), &models.Order{StoreOrderID: "SO-1"}); err != nil || number != "" {
		t.Fatalf("ResolveOrder on a disabled service returned (%q, %v)", number, err)
	}
}
