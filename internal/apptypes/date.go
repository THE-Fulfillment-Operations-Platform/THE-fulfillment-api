// Package apptypes holds small shared types used at the JSON/API boundary.
package apptypes

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Date is a JSON date/datetime that decodes leniently from the formats API
// clients actually send: a bare calendar date "YYYY-MM-DD" (e.g. from an HTML
// <input type="date">), an HTML datetime-local value, or full RFC3339. Go's
// default time.Time JSON decoding accepts *only* RFC3339, which makes plain
// date inputs fail with `cannot parse "" as "T"`.
//
// Use Date in request DTOs (never in GORM models — those keep time.Time for the
// DB column) and convert to storage with TimePtr. A JSON null or empty string
// decodes to the zero value, which TimePtr reports as "no date".
type Date struct {
	time.Time
}

// acceptedLayouts are tried in order: RFC3339 (the canonical wire format) first,
// then the shorter browser-native forms, most specific to least.
var acceptedLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02",
}

// UnmarshalJSON parses any accepted layout. A bare date is interpreted at UTC
// midnight. A JSON null or empty string yields the zero value.
func (d *Date) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		d.Time = time.Time{}
		return nil
	}
	for _, layout := range acceptedLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			d.Time = t
			return nil
		}
	}
	return fmt.Errorf("invalid date %q: expected YYYY-MM-DD or RFC3339", s)
}

// MarshalJSON emits RFC3339, or null for the zero value, so responses stay in
// the canonical format regardless of what the client sent.
func (d Date) MarshalJSON() ([]byte, error) {
	if d.Time.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(d.Time.Format(time.RFC3339))
}

// TimePtr returns a *time.Time for storage: nil when unset (nil receiver or the
// zero value), otherwise a pointer to the parsed time. Nil-safe so callers can
// write in.DueDate.TimePtr() without guarding against a missing field.
func (d *Date) TimePtr() *time.Time {
	if d == nil || d.Time.IsZero() {
		return nil
	}
	t := d.Time
	return &t
}
