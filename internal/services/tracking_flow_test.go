package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/tracking24h"
)

// fakeProvider is a stand-in for api.24htrack.com. It implements just enough of
// the real contract — the { code, data } envelope, the description LIKE filter,
// and register-updates-description — to drive the sync service end to end.
type fakeProvider struct {
	// mu guards the three maps below. The service pushes to the provider from
	// fire-and-forget goroutines (RegisterOrderAsync, ReleaseNumberAsync), so two
	// requests can land in serve() at once — unsynchronised map writes are a fatal
	// runtime throw, which surfaced as a rare random failure of the whole package.
	mu      sync.Mutex
	parcels map[string]*fakeParcel // by tracking number
	events  map[string][]tracking24h.Event
	calls   map[string]int // endpoint → hit count, to assert we don't over-poll
}

type fakeParcel struct {
	Carrier     string
	Description string
	Status      string
	Detail      string
	Location    string
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		parcels: map[string]*fakeParcel{},
		events:  map[string][]tracking24h.Event{},
		calls:   map[string]int{},
	}
}

func (f *fakeProvider) start(t *testing.T) *tracking24h.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	// No inter-call delay: the throttle protects the real provider, and paying it
	// in tests would only make them slow.
	return tracking24h.New(srv.URL, "test@example.com", "secret", tracking24h.WithMinInterval(0))
}

func (f *fakeProvider) serve(w http.ResponseWriter, r *http.Request) {
	// One handler at a time: every branch below reads and writes the shared maps.
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	switch {
	case path == "/auth/login":
		f.calls["login"]++
		// A far-future exp so the client never re-logs in mid-test.
		writeJSON(map[string]any{"code": 0, "token": testJWT, "user": map[string]any{"id": 1}})

	case path == "/tracking/register":
		f.calls["register"]++
		var body struct {
			Numbers []tracking24h.RegisterItem `json:"numbers"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		accepted := []map[string]string{}
		for _, it := range body.Numbers {
			p, ok := f.parcels[it.Number]
			if !ok {
				p = &fakeParcel{Status: "Not Found"}
				f.parcels[it.Number] = p
			}
			p.Description = it.Description
			accepted = append(accepted, map[string]string{"number": it.Number, "description": it.Description})
		}
		writeJSON(map[string]any{"code": 0, "data": map[string]any{"accepted": accepted, "rejected": []any{}}})

	case path == "/tracking":
		f.calls["list"]++
		want := r.URL.Query().Get("description")
		items := []map[string]any{}
		for number, p := range f.parcels {
			// The real filter is a LIKE, so the fake must be one too — that is
			// precisely what the service has to defend against.
			if want != "" && !strings.Contains(p.Description, want) {
				continue
			}
			items = append(items, parcelJSON(number, p))
		}
		writeJSON(map[string]any{"code": 0, "data": map[string]any{
			"items": items, "total": len(items), "page": 1, "pages": 1,
		}})

	case strings.HasSuffix(path, "/events"):
		f.calls["events"]++
		number := strings.TrimSuffix(strings.TrimPrefix(path, "/tracking/"), "/events")
		writeJSON(map[string]any{"code": 0, "data": f.events[number]})

	case strings.HasPrefix(path, "/tracking/"):
		f.calls["detail"]++
		number := strings.TrimPrefix(path, "/tracking/")
		p, ok := f.parcels[number]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(map[string]any{"code": -1, "message": "Tracking not found"})
			return
		}
		writeJSON(map[string]any{"code": 0, "data": parcelJSON(number, p)})

	default:
		w.WriteHeader(http.StatusNotFound)
		writeJSON(map[string]any{"code": -1, "message": "Endpoint not found"})
	}
}

func parcelJSON(number string, p *fakeParcel) map[string]any {
	return map[string]any{
		"tracking_number": number,
		"carrier":         p.Carrier,
		"description":     p.Description,
		"status":          p.Status,
		"rawStatus":       p.Status,
		"detail":          p.Detail,
		"location":        p.Location,
		// is_archived is a NUMBER in the real API, not a bool — keeping it here
		// guards the client against a decode regression.
		"is_archived": 0,
	}
}

// A token whose exp is in 2286 (10_000_000_000). Only the exp claim is read.
const testJWT = "eyJhbGciOiJIUzI1NiJ9." +
	"eyJpZCI6MSwiZXhwIjoxMDAwMDAwMDAwMH0." +
	"signature-is-never-verified"

func newTrackingDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Pin the pool to ONE connection. Every ":memory:" connection is its own
	// empty database, so the moment database/sql opens a second one — which it
	// does as soon as two queries overlap — that query hits a schema-less DB and
	// fails with "no such table". The tracking service pushes to the provider
	// from fire-and-forget goroutines, so overlapping queries are routine here;
	// this was a rare, random failure of the whole package.
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	// StatusHistory: a sync that moves the order forward records the transition,
	// so without this table every tracking test logs a failed insert and the
	// advance path is never really exercised.
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Order{}, &models.OrderItem{},
		&models.OrderTrackingEvent{}, &models.StatusHistory{}, &models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedTrackedOrder(t *testing.T, db *gorm.DB, code, storeOrder, trackingNumber string) *models.Order {
	t.Helper()
	o := &models.Order{
		InternalCode: code, StoreOrderID: storeOrder, SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusShipped,
		TrackingNumber: trackingNumber, TrackingStatus: models.TrackingPending,
	}
	if trackingNumber == "" {
		o.TrackingStatus = models.TrackingNone
	}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return o
}

func newSync(t *testing.T, db *gorm.DB, f *fakeProvider) *TrackingSyncService {
	t.Helper()
	repo := repositories.New(db)
	return NewTrackingSyncService(repo, &AuditService{repo: repo}, f.start(t), "FFM", true)
}

// The core promise of the feature: an order that ships gets its carrier status
// and its full journey mirrored into our database.
func TestSync_MirrorsStatusAndJourney(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	f.parcels["9400111899223456789012"] = &fakeParcel{
		Carrier: "USPS", Status: "In Transit",
		Detail: "In Transit to Next Facility", Location: "JAMAICA, NY 11434",
	}
	f.events["9400111899223456789012"] = []tracking24h.Event{
		{EventDate: "July 27, 2026 9:53 PM", Location: "JAMAICA, NY", Description: "In Transit to Next Facility"},
		{EventDate: "July 26, 2026 8:10 AM", Location: "GRESHAM, OR", Description: "Shipping Label Created", StatusHint: "Info Received"},
	}
	sync := newSync(t, db, f)
	order := seedTrackedOrder(t, db, "ORD-1", "SO-1", "9400111899223456789012")

	changed, err := sync.syncOrder(t.Context(), order)
	if err != nil {
		t.Fatalf("syncOrder: %v", err)
	}
	if !changed {
		t.Fatal("PENDING → IN_TRANSIT should have been reported as a change")
	}

	var got models.Order
	db.First(&got, order.ID)
	if got.TrackingStatus != models.TrackingInTransit {
		t.Fatalf("tracking_status = %s, want IN_TRANSIT", got.TrackingStatus)
	}
	if got.TrackingLocation != "JAMAICA, NY 11434" {
		t.Fatalf("location not mirrored: %+v", got)
	}
	if got.TrackingSyncedAt == nil || got.TrackingUpdatedAt == nil {
		t.Fatal("sync/update timestamps not stamped")
	}
	if got.TrackingURL == "" || !strings.Contains(got.TrackingURL, "9400111899223456789012") {
		t.Fatalf("tracking_url not filled in: %q", got.TrackingURL)
	}

	events, err := repositories.New(db).Tracking.EventsByOrder(order.ID, order.TrackingNumber)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("stored %d events, want 2", len(events))
	}
	// Newest scan first — the UI renders the list top-down.
	if events[0].Description != "In Transit to Next Facility" {
		t.Fatalf("timeline not newest-first: %+v", events[0])
	}
	if events[0].EventAt == nil {
		t.Fatal("event time was not parsed")
	}
}

// Every poll returns the WHOLE timeline. Without the fingerprint the journey
// would grow a duplicate copy of itself on each pass.
func TestSync_IsIdempotent(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	f.parcels["940011"] = &fakeParcel{Carrier: "USPS", Status: "In Transit"}
	f.events["940011"] = []tracking24h.Event{
		{EventDate: "July 27, 2026 9:53 PM", Location: "JAMAICA, NY", Description: "In Transit"},
	}
	sync := newSync(t, db, f)
	order := seedTrackedOrder(t, db, "ORD-2", "SO-2", "940011")

	for i := 0; i < 3; i++ {
		if _, err := sync.syncOrder(t.Context(), order); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	var count int64
	db.Model(&models.OrderTrackingEvent{}).Where("order_id = ?", order.ID).Count(&count)
	if count != 1 {
		t.Fatalf("timeline holds %d rows after 3 syncs, want 1", count)
	}

	// A genuinely new scan still lands.
	f.events["940011"] = append([]tracking24h.Event{
		{EventDate: "July 28, 2026 10:00 AM", Location: "TORRANCE, CA", Description: "Delivered"},
	}, f.events["940011"]...)
	if _, err := sync.syncOrder(t.Context(), order); err != nil {
		t.Fatalf("sync after new scan: %v", err)
	}
	db.Model(&models.OrderTrackingEvent{}).Where("order_id = ?", order.ID).Count(&count)
	if count != 2 {
		t.Fatalf("timeline holds %d rows after a new scan, want 2", count)
	}
}

// The feature the user asked for: an order uploaded with only a store order id
// finds its parcel on 24hTrack.
func TestResolve_FindsParcelByStoreOrderID(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	f.parcels["940022"] = &fakeParcel{Carrier: "UPS", Description: "FFM:SO-777", Status: "Delivered"}
	f.events["940022"] = []tracking24h.Event{
		{EventDate: "July 27, 2026 9:53 PM", Location: "TORRANCE, CA", Description: "Delivered, In/At Mailbox"},
	}
	sync := newSync(t, db, f)
	order := seedTrackedOrder(t, db, "ORD-3", "SO-777", "")

	number, err := sync.ResolveOrder(t.Context(), order)
	if err != nil {
		t.Fatalf("ResolveOrder: %v", err)
	}
	if number != "940022" {
		t.Fatalf("resolved %q, want 940022", number)
	}

	var got models.Order
	db.First(&got, order.ID)
	if got.TrackingNumber != "940022" {
		t.Fatalf("order did not adopt the parcel: %+v", got)
	}
	// Resolving also pulls the state in, so the screen shows a journey at once.
	if got.TrackingStatus != models.TrackingDelivered {
		t.Fatalf("tracking_status = %s, want DELIVERED", got.TrackingStatus)
	}
	var count int64
	db.Model(&models.OrderTrackingEvent{}).Where("order_id = ?", order.ID).Count(&count)
	if count != 1 {
		t.Fatalf("journey has %d rows, want 1", count)
	}
}

// The provider's description filter is a LIKE: searching "FFM:SO-1" also returns
// "FFM:SO-12". Adopting that parcel would attach the wrong journey to an order.
func TestResolve_RejectsPrefixMatch(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	f.parcels["940033"] = &fakeParcel{Carrier: "USPS", Description: "FFM:SO-12", Status: "In Transit"}
	sync := newSync(t, db, f)
	order := seedTrackedOrder(t, db, "ORD-4", "SO-1", "")

	number, err := sync.ResolveOrder(t.Context(), order)
	if err != nil {
		t.Fatalf("ResolveOrder: %v", err)
	}
	if number != "" {
		t.Fatalf("adopted %q on a mere prefix match", number)
	}
	var got models.Order
	db.First(&got, order.ID)
	if got.TrackingNumber != "" {
		t.Fatalf("order picked up a tracking number it should not have: %q", got.TrackingNumber)
	}
	// The attempt is stamped so the scheduler rotates on instead of re-asking.
	if got.TrackingSyncedAt == nil {
		t.Fatal("a fruitless resolve should still stamp tracking_synced_at")
	}
}

// Two parcels carrying the same store order id is a data problem a human must
// settle — guessing one would silently mislabel a shipment.
func TestResolve_LeavesAmbiguityAlone(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	f.parcels["940044"] = &fakeParcel{Description: "FFM:SO-9", Status: "In Transit"}
	f.parcels["940055"] = &fakeParcel{Description: "FFM:SO-9", Status: "Delivered"}
	sync := newSync(t, db, f)
	order := seedTrackedOrder(t, db, "ORD-5", "SO-9", "")

	number, err := sync.ResolveOrder(t.Context(), order)
	if err != nil || number != "" {
		t.Fatalf("ResolveOrder = (%q, %v), want no adoption", number, err)
	}
	var got models.Order
	db.First(&got, order.ID)
	if got.TrackingNumber != "" {
		t.Fatalf("adopted %q despite ambiguity", got.TrackingNumber)
	}
	if !strings.Contains(got.TrackingSyncError, "2") {
		t.Fatalf("ambiguity was not surfaced to the operator: %q", got.TrackingSyncError)
	}
}

// Registering is what tags a parcel with its store order id — the link the whole
// lookup depends on.
func TestRegisterOrder_TagsParcelWithStoreOrderID(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	sync := newSync(t, db, f)
	order := seedTrackedOrder(t, db, "ORD-6", "SO-42", "940066")

	if err := sync.RegisterOrder(t.Context(), order); err != nil {
		t.Fatalf("RegisterOrder: %v", err)
	}
	if got := f.parcels["940066"].Description; got != "FFM:SO-42" {
		t.Fatalf("provider description = %q, want FFM:SO-42", got)
	}
}

// A parcel the provider does not know must not look like a healthy shipment: the
// failure has to be visible on the order, and the timestamp must still advance so
// the scheduler does not spin on it.
func TestSync_RecordsProviderFailure(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider() // knows nothing
	sync := newSync(t, db, f)
	order := seedTrackedOrder(t, db, "ORD-7", "SO-7", "940077")

	if _, err := sync.syncOrder(t.Context(), order); err == nil {
		t.Fatal("expected an error for an unknown parcel")
	}
	var got models.Order
	db.First(&got, order.ID)
	if got.TrackingSyncError == "" {
		t.Fatal("failure was not recorded on the order")
	}
	if got.TrackingSyncedAt == nil {
		t.Fatal("failed sync must still stamp tracking_synced_at")
	}
	if got.TrackingStatus != models.TrackingPending {
		t.Fatalf("status changed to %s on a failed sync", got.TrackingStatus)
	}
}

// A delivered parcel never moves again; continuing to poll it would spend the
// provider budget that live shipments need.
func TestDueForSync_SkipsTerminalParcels(t *testing.T) {
	db := newTrackingDB(t)
	repo := repositories.New(db)

	live := seedTrackedOrder(t, db, "ORD-8", "SO-8", "940088")
	done := seedTrackedOrder(t, db, "ORD-9", "SO-9", "940099")
	db.Model(done).Update("tracking_status", models.TrackingDelivered)
	noNumber := seedTrackedOrder(t, db, "ORD-10", "SO-10", "")

	due, err := repo.Tracking.DueForSync(50, timeNowPlusHour())
	if err != nil {
		t.Fatalf("DueForSync: %v", err)
	}
	ids := map[uint]bool{}
	for _, o := range due {
		ids[o.ID] = true
	}
	if !ids[live.ID] {
		t.Fatal("a live parcel was not due for sync")
	}
	if ids[done.ID] {
		t.Fatal("a delivered parcel is still being polled")
	}
	if ids[noNumber.ID] {
		t.Fatal("an order without a tracking number was queued for a detail call")
	}
}

// RunOnce is what the scheduler calls: it must both adopt and refresh in one go.
func TestRunOnce_ResolvesAndSyncs(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	f.parcels["940111"] = &fakeParcel{Carrier: "USPS", Status: "In Transit"}
	f.parcels["940222"] = &fakeParcel{Carrier: "UPS", Description: "FFM:SO-B", Status: "Delivered"}
	sync := newSync(t, db, f)

	seedTrackedOrder(t, db, "ORD-A", "SO-A", "940111") // already tracked → refresh
	seedTrackedOrder(t, db, "ORD-B", "SO-B", "")       // untracked → resolve

	stats := sync.RunOnce(t.Context(), 40)
	if stats.Resolved != 1 {
		t.Fatalf("resolved %d orders, want 1 (%+v)", stats.Resolved, stats)
	}
	if stats.Synced != 1 || stats.Failed != 0 {
		t.Fatalf("unexpected pass result: %+v", stats)
	}
}

// timeNowPlusHour is the "stale before" bound used by the due-list test: every
// row seeded here has a NULL tracking_synced_at, so any future instant selects
// the ones that qualify on status alone.
func timeNowPlusHour() time.Time { return time.Now().Add(time.Hour) }
