package tracking24h

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// These tests talk to the REAL 24hTrack API and are skipped unless credentials
// are supplied, so `go test ./...` stays offline and deterministic:
//
//	TRACK24H_EMAIL=… TRACK24H_PASSWORD=… go test ./internal/tracking24h -run Live -v
//
// They exist because the provider's contract (envelope shape, description
// filter, "registering a number we already own costs no quota") is the part of
// this integration that no unit test can prove.
func liveClient(t *testing.T) *Client {
	t.Helper()
	email, password := os.Getenv("TRACK24H_EMAIL"), os.Getenv("TRACK24H_PASSWORD")
	if email == "" || password == "" {
		t.Skip("set TRACK24H_EMAIL and TRACK24H_PASSWORD to run live provider tests")
	}
	return New(os.Getenv("TRACK24H_BASE_URL"), email, password)
}

func TestLiveLoginAndList(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// An empty description matches everything, so this doubles as "can we log in
	// and read the account at all".
	page, err := c.SearchByDescription(ctx, "", 1, 5)
	if err != nil {
		t.Fatalf("SearchByDescription: %v", err)
	}
	if page.Total <= 0 || len(page.Items) == 0 {
		t.Fatalf("expected the account to hold parcels, got total=%d items=%d", page.Total, len(page.Items))
	}
	for _, it := range page.Items {
		if strings.TrimSpace(it.TrackingNumber) == "" {
			t.Fatal("provider returned a parcel with no tracking number")
		}
	}
}

func TestLiveDetailAndEvents(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	page, err := c.SearchByDescription(ctx, "", 1, 1)
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("cannot pick a sample parcel: %v", err)
	}
	number := page.Items[0].TrackingNumber

	detail, err := c.Get(ctx, number)
	if err != nil {
		t.Fatalf("Get(%s): %v", number, err)
	}
	if detail.TrackingNumber != number {
		t.Fatalf("Get returned %q, asked for %q", detail.TrackingNumber, number)
	}
	if detail.Status == "" {
		t.Fatal("Get returned an empty status")
	}

	events, err := c.Events(ctx, number)
	if err != nil {
		t.Fatalf("Events(%s): %v", number, err)
	}
	// A parcel may legitimately have no scans yet; only the shape has to hold.
	for _, e := range events {
		if e.Description == "" && e.EventDate == "" {
			t.Fatal("provider returned an entirely empty event")
		}
	}
	t.Logf("parcel %s: carrier=%s status=%q events=%d", number, detail.Carrier, detail.Status, len(events))
}

// An unknown number must surface as ErrNotFound, not as a generic failure — the
// sync treats the two very differently.
func TestLiveUnknownNumber(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, err := c.Get(ctx, "ZZ0000000000000000TEST")
	if err == nil {
		t.Fatal("expected an error for a number the account does not track")
	}
	t.Logf("unknown number → %v (ErrNotFound=%v)", err, err == ErrNotFound)
}

// The whole store-order→parcel link rests on this: writing a description on a
// parcel we already own, then finding it back by that description.
func TestLiveDescriptionRoundTrip(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	page, err := c.SearchByDescription(ctx, "", 1, 1)
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("cannot pick a sample parcel: %v", err)
	}
	number := page.Items[0].TrackingNumber
	original := page.Items[0].Description
	marker := "FFM-SELFTEST-" + number[max(0, len(number)-6):]

	// Always put the parcel back the way we found it, whatever happens below.
	defer func() {
		_, _ = c.Register(context.Background(), []RegisterItem{{Number: number, Description: original}})
	}()

	res, err := c.Register(ctx, []RegisterItem{{Number: number, Description: marker}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(res.Accepted) != 1 {
		t.Fatalf("Register accepted %d items, rejected %d", len(res.Accepted), len(res.Rejected))
	}

	found, err := c.SearchByDescription(ctx, marker, 1, 10)
	if err != nil {
		t.Fatalf("SearchByDescription(%s): %v", marker, err)
	}
	var hit bool
	for _, it := range found.Items {
		if it.TrackingNumber == number && it.Description == marker {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("parcel %s was not found back by its description %q", number, marker)
	}
}
