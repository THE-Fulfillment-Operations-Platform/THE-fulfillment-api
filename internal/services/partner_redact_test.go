package services

import (
	"encoding/json"
	"strings"
	"testing"

	"the-fulfillment/backend/internal/models"
)

// withPartnerConfig installs a redaction config for one test. partnerOnce is
// bypassed deliberately: the production path loads env once at first use, but a
// test needs to swap the list per case.
func withPartnerConfig(t *testing.T, brand string, aliases ...string) {
	t.Helper()
	prevBrand, prevAliases := partnerBrand, partnerAliases
	partnerOnce.Do(func() {}) // mark as loaded so redactPartner does not re-read env
	partnerBrand, partnerAliases = brand, aliases
	t.Cleanup(func() { partnerBrand, partnerAliases = prevBrand, prevAliases })
}

// The seller must never learn who actually carries the parcel, whatever casing
// the provider used when it stamped its own name into a scan line.
func TestRedactPartner_HidesEverySpelling(t *testing.T) {
	withPartnerConfig(t, "THE", "Acme Express", "AcmeExpress", "ACME")

	cases := map[string]string{
		"Arrived at AcmeExpress facility":  "Arrived at THE facility",
		"Shipping Partner: ACME":           "Shipping Partner: THE",
		"Handed to acme express for final": "Handed to THE for final",
		"In Transit to Next Facility":      "In Transit to Next Facility",
		"":                                 "",
	}
	for in, want := range cases {
		if got := redactPartner(in); got != want {
			t.Errorf("redactPartner(%q) = %q, want %q", in, got, want)
		}
	}
}

// "Acme Express" must be matched before the shorter "ACME", otherwise the
// leftover "THE Express" still points straight at the partner.
func TestRedactPartner_LongestAliasWins(t *testing.T) {
	partnerOnce.Do(func() {})
	prevBrand, prevAliases := partnerBrand, partnerAliases
	t.Cleanup(func() { partnerBrand, partnerAliases = prevBrand, prevAliases })

	partnerBrand, partnerAliases = "THE", nil
	t.Setenv("SHIPPING_BRAND", "THE")
	t.Setenv("SHIPPING_PARTNER_ALIASES", "ACME,Acme Express")
	loadPartnerConfig() // re-run the real loader, including its ordering pass

	if got := redactPartner("Departed Acme Express hub"); got != "Departed THE hub" {
		t.Fatalf("short alias matched first: got %q", got)
	}
}

// With nothing configured the integration must behave exactly as before —
// silently mangling provider text would be worse than showing it.
func TestRedactPartner_PassThroughWhenUnconfigured(t *testing.T) {
	withPartnerConfig(t, "THE")

	const in = "Arrived at AcmeExpress facility"
	if got := redactPartner(in); got != in {
		t.Fatalf("redacted with no aliases configured: %q", got)
	}
}

// The journey is redacted as a copy: ops screens read the same rows and are
// entitled to the provider's original wording.
func TestRedactPartnerEvents_DoesNotMutateInput(t *testing.T) {
	withPartnerConfig(t, "THE", "ACME")

	events := []models.OrderTrackingEvent{
		{Description: "Picked up by ACME", Location: "ACME HUB, SZX"},
	}
	out := redactPartnerEvents(events)

	if events[0].Description != "Picked up by ACME" || events[0].Location != "ACME HUB, SZX" {
		t.Fatalf("input mutated: %+v", events[0])
	}
	if out[0].Description != "Picked up by THE" || out[0].Location != "THE HUB, SZX" {
		t.Fatalf("copy not redacted: %+v", out[0])
	}
}

// Regression guard for the whole point of this change: nothing in a seller
// response may name the transport partner or link to the provider's page.
func TestSellerView_CarriesNoPartnerIdentity(t *testing.T) {
	withPartnerConfig(t, "THE", "ACME")

	o := models.Order{
		InternalCode: "100001", StoreOrderID: "ETSY-1",
		TrackingNumber: "940011", TrackingStatus: models.TrackingInTransit,
		TrackingDetail:   "Arrived at ACME facility",
		TrackingLocation: "ACME HUB, SZX",
		TrackingURL:      "https://tracking-provider.example.com/track?ids=940011",
	}
	v := toSellerView(o, false, false)

	// The seller still gets a usable shipment state...
	if v.TrackingNumber != "940011" || v.TrackingStatus != models.TrackingInTransit {
		t.Fatalf("seller lost the tracking state: %+v", v)
	}
	// ...with the partner's name filtered out of the free-form provider text.
	if strings.Contains(v.TrackingDetail, "ACME") || strings.Contains(v.TrackingLocation, "ACME") {
		t.Fatalf("partner name reached the seller: detail=%q location=%q", v.TrackingDetail, v.TrackingLocation)
	}
	// The provider deep link must not be in the payload at all — the page behind
	// it names the partner regardless of what we redact on our side. Checked on
	// the marshalled JSON, because that is what actually crosses the wire.
	payload, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal seller view: %v", err)
	}
	for _, banned := range []string{"tracking-provider.example.com", "tracking_url", "tracking_carrier"} {
		if strings.Contains(string(payload), banned) {
			t.Fatalf("seller payload still carries %q: %s", banned, payload)
		}
	}
}
