package services

import (
	"os"
	"strings"
	"sync"

	"the-fulfillment/backend/internal/models"
)

// Redaction of the transport partner's name from anything a seller can read.
//
// Towards the seller, THE is the shipping company. Which transport partner
// actually flies the parcel is supplier information — the same class of secret
// as what we pay them. Dropping the "shipping company" column and the provider
// deep link closes the structural leaks, but one hole is left: the scan text the
// provider hands back is free-form, and some partners stamp their own name into
// it ("Shipping Partner: …", "Arrived at … facility"). That text is shown to the
// seller verbatim, so it gets filtered here.
//
// The partner's name is deliberately NOT written in this source file. It comes
// from the environment, because this repository is handed over to the customer
// while the .env is not — a name hardcoded here would leak through the code even
// after every screen was cleaned.
//
//	SHIPPING_PARTNER_ALIASES=Acme Express,AcmeExpress,ACME  # spellings to hide
//	SHIPPING_BRAND=THE                                      # what replaces them
//
// The names above are a format example only. Never commit a real partner name
// into this file — that is the leak this whole mechanism exists to prevent.
var (
	partnerOnce    sync.Once
	partnerAliases []string
	partnerBrand   string
)

func loadPartnerConfig() {
	partnerBrand = strings.TrimSpace(os.Getenv("SHIPPING_BRAND"))
	if partnerBrand == "" {
		partnerBrand = "THE"
	}
	for _, a := range strings.Split(os.Getenv("SHIPPING_PARTNER_ALIASES"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			partnerAliases = append(partnerAliases, a)
		}
	}
	// Longest first: hiding "Acme Express" before "Acme" avoids leaving "THE Express".
	for i := 1; i < len(partnerAliases); i++ {
		for j := i; j > 0 && len(partnerAliases[j]) > len(partnerAliases[j-1]); j-- {
			partnerAliases[j], partnerAliases[j-1] = partnerAliases[j-1], partnerAliases[j]
		}
	}
}

// redactPartner replaces every configured spelling of the transport partner with
// our own brand. With no aliases configured it is a pass-through, so the
// integration behaves exactly as before until someone fills the env in.
func redactPartner(s string) string {
	if s == "" {
		return s
	}
	partnerOnce.Do(loadPartnerConfig)
	if len(partnerAliases) == 0 {
		return s
	}
	out := s
	for _, alias := range partnerAliases {
		out = replaceFold(out, alias, partnerBrand)
	}
	// Removing a name mid-sentence leaves double spaces behind ("Arrived at
	// facility"); collapse them so the seller sees an ordinary line.
	return strings.Join(strings.Fields(out), " ")
}

// redactPartnerEvents copies a journey with every scan line redacted. It copies
// rather than mutating: the same rows are also served to internal screens, which
// are entitled to the provider's original wording.
func redactPartnerEvents(events []models.OrderTrackingEvent) []models.OrderTrackingEvent {
	partnerOnce.Do(loadPartnerConfig)
	if len(partnerAliases) == 0 || len(events) == 0 {
		return events
	}
	out := make([]models.OrderTrackingEvent, len(events))
	for i, ev := range events {
		ev.Description = redactPartner(ev.Description)
		ev.Location = redactPartner(ev.Location)
		out[i] = ev
	}
	return out
}

// replaceFold is a case-insensitive strings.ReplaceAll. Partner names arrive
// from the provider in whatever casing that partner uses ("ACMEEXPRESS" in one
// scan, "AcmeExpress" in the next), so an exact match would miss half of them.
// ASCII-only by design: these are company names, and a fold that changed byte
// length would break the index arithmetic below.
func replaceFold(s, old, new string) string {
	if old == "" {
		return s
	}
	lowerS, lowerOld := strings.ToLower(s), strings.ToLower(old)
	if len(lowerS) != len(s) || len(lowerOld) != len(old) {
		return strings.ReplaceAll(s, old, new) // non-ASCII: fall back to exact match
	}
	var b strings.Builder
	for {
		i := strings.Index(lowerS, lowerOld)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(new)
		s, lowerS = s[i+len(old):], lowerS[i+len(old):]
	}
}
