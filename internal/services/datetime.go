package services

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// appLocation is the business timezone used for "STT trong ngày" and OrderDate.
// It is DB_TIMEZONE (matching the DB session timezone), defaulting to
// Asia/Ho_Chi_Minh, and falls back to UTC if the zone can't be loaded. Loaded
// once. cmd/server imports time/tzdata so the zone is always available even on a
// minimal container image without the system zoneinfo database.
var (
	appLocOnce sync.Once
	appLoc     *time.Location
)

// AppLocation returns the configured business timezone.
func AppLocation() *time.Location {
	appLocOnce.Do(func() {
		name := os.Getenv("DB_TIMEZONE")
		if name == "" {
			name = "Asia/Ho_Chi_Minh"
		}
		loc, err := time.LoadLocation(name)
		if err != nil {
			loc = time.UTC
		}
		appLoc = loc
	})
	return appLoc
}

// AppDateString formats t as the business calendar day (YYYY-MM-DD) in the
// business timezone. This is the value stored in Order.OrderDate.
func AppDateString(t time.Time) string {
	return t.In(AppLocation()).Format("2006-01-02")
}

// ---------------------------------------------------------------------------
// Order date parsing (the seller template's "DATE" column)
// ---------------------------------------------------------------------------

// errBadOrderDate is returned for a value that cannot be read as a date at all.
var errBadOrderDate = errors.New("unrecognised date")

var dateSepRe = regexp.MustCompile(`^(\d{1,4})[-/.](\d{1,2})[-/.](\d{1,4})$`)

// excelEpoch is day 0 of the Excel 1900 serial system. Excel's serials are
// off-by-one from a true calendar because it believes 1900 was a leap year, and
// 1899-12-30 is the anchor that cancels the error out for every date after
// 1900-03-01 — i.e. every date any order will ever carry.
var excelEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

// ParseOrderDate reads the seller's "DATE" cell into a business calendar day
// (YYYY-MM-DD). ambiguous=true means the value could be read two ways and the
// day-first reading was taken — the caller surfaces that as a warning rather
// than guessing silently.
//
// Accepted: ISO (2026-08-20, 2026/08/20), day-first (20/08/2026, 20-8-26),
// month-first when the first component cannot be a day (8/20/2026), compact
// 20260820, and a raw Excel serial (46000) for files where the cell was left as
// a number. A trailing time component is ignored.
//
// The day-first default is deliberate: the sellers filling this template write
// Vietnamese dates. For a value like 05/06/2026 no parser on earth can know
// which was meant, so we pick one, say so, and let a human confirm.
func ParseOrderDate(raw string) (date string, ambiguous bool, err error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", false, nil
	}
	// Drop a time component: "2026-08-20 00:00:00", "2026-08-20T00:00:00Z".
	if i := strings.IndexAny(v, " T"); i > 0 && len(v) > 10 {
		v = v[:i]
	}

	// Excel serial: a bare number. Bounded to the plausible range so a compact
	// 20260820 (8 digits) is not mistaken for one.
	if isAllDigits(v) && len(v) <= 6 {
		n, convErr := strconv.Atoi(v)
		if convErr == nil && n >= 1 && n <= 200000 {
			t := excelEpoch.AddDate(0, 0, n)
			return t.Format("2006-01-02"), false, nil
		}
	}
	// Compact yyyymmdd.
	if isAllDigits(v) && len(v) == 8 {
		y, _ := strconv.Atoi(v[:4])
		mo, _ := strconv.Atoi(v[4:6])
		d, _ := strconv.Atoi(v[6:])
		out, buildErr := buildDate(y, mo, d)
		return out, false, buildErr
	}

	m := dateSepRe.FindStringSubmatch(v)
	if m == nil {
		return "", false, fmt.Errorf("%w: %q", errBadOrderDate, raw)
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])

	// Year first (2026-08-20).
	if len(m[1]) == 4 {
		out, buildErr := buildDate(a, b, c)
		return out, false, buildErr
	}
	year := c
	if len(m[3]) <= 2 {
		year = 2000 + c // "26" -> 2026; these files never carry 20th-century orders
	}
	switch {
	case a > 12: // 20/08/2026 — can only be day-first
		out, buildErr := buildDate(year, b, a)
		return out, false, buildErr
	case b > 12: // 8/20/2026 — can only be month-first
		out, buildErr := buildDate(year, a, b)
		return out, false, buildErr
	default: // both <= 12: genuinely ambiguous, take day-first and flag it
		out, buildErr := buildDate(year, b, a)
		return out, buildErr == nil, buildErr
	}
}

func buildDate(y, m, d int) (string, error) {
	if y < 1970 || y > 2200 || m < 1 || m > 12 || d < 1 || d > 31 {
		return "", fmt.Errorf("%w: %04d-%02d-%02d", errBadOrderDate, y, m, d)
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	// Rejects 31/02: Go normalises it to 02 Mar, so the round-trip differs.
	if t.Year() != y || int(t.Month()) != m || t.Day() != d {
		return "", fmt.Errorf("%w: %04d-%02d-%02d", errBadOrderDate, y, m, d)
	}
	return t.Format("2006-01-02"), nil
}

func isAllDigits(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
