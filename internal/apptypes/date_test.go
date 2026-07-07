package apptypes

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDateUnmarshalJSON(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantNil bool      // TimePtr() should be nil
		want    time.Time // expected parsed time when not nil
	}{
		{"bare date (input type=date)", `"2026-07-07"`, false, time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)},
		{"datetime-local", `"2026-07-07T09:30"`, false, time.Date(2026, 7, 7, 9, 30, 0, 0, time.UTC)},
		{"datetime without tz", `"2026-07-07T09:30:15"`, false, time.Date(2026, 7, 7, 9, 30, 15, 0, time.UTC)},
		{"full RFC3339 UTC", `"2026-07-07T09:30:15Z"`, false, time.Date(2026, 7, 7, 9, 30, 15, 0, time.UTC)},
		{"RFC3339 with offset", `"2026-07-07T09:30:15+07:00"`, false, time.Date(2026, 7, 7, 2, 30, 15, 0, time.UTC)},
		{"empty string", `""`, true, time.Time{}},
		{"json null", `null`, true, time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var d Date
			if err := json.Unmarshal([]byte(c.json), &d); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			ptr := d.TimePtr()
			if c.wantNil {
				if ptr != nil {
					t.Fatalf("TimePtr() = %v, want nil", ptr)
				}
				return
			}
			if ptr == nil {
				t.Fatalf("TimePtr() = nil, want %v", c.want)
			}
			if !ptr.Equal(c.want) {
				t.Errorf("parsed = %v, want %v", ptr.UTC(), c.want)
			}
		})
	}
}

func TestDateUnmarshalInvalid(t *testing.T) {
	var d Date
	if err := json.Unmarshal([]byte(`"07/07/2026"`), &d); err == nil {
		t.Fatal("expected error for unsupported date format, got nil")
	}
}

func TestDateMarshalJSON(t *testing.T) {
	// Zero value → null.
	var zero Date
	if b, _ := json.Marshal(zero); string(b) != "null" {
		t.Errorf("zero MarshalJSON = %s, want null", b)
	}
	// Round-trips a bare date back out as RFC3339.
	var d Date
	if err := json.Unmarshal([]byte(`"2026-07-07"`), &d); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	if string(b) != `"2026-07-07T00:00:00Z"` {
		t.Errorf("MarshalJSON = %s, want %q", b, `"2026-07-07T00:00:00Z"`)
	}
}

// TimePtr must be nil-safe on a nil receiver (optional DTO field absent).
func TestDateTimePtrNilSafe(t *testing.T) {
	var d *Date
	if got := d.TimePtr(); got != nil {
		t.Errorf("nil receiver TimePtr() = %v, want nil", got)
	}
}
