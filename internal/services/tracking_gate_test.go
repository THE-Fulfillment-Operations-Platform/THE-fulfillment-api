package services

import (
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// seedOrderAtStage creates an order sitting at a given production stage, with or
// without a tracking number.
func seedOrderAtStage(t *testing.T, db *gorm.DB, code string, status models.SellerStatus, tracking string) *models.Order {
	t.Helper()
	o := &models.Order{
		InternalCode: code, StoreOrderID: "SO-" + code, SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: status,
		TrackingNumber: tracking, TrackingStatus: models.TrackingNone,
	}
	if tracking != "" {
		o.TrackingStatus = models.TrackingPending
	}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return o
}

// Production and shipping are two separate halves of an order's life. Until the
// packing station hands the parcel over, the carrier has never seen it — polling
// the provider then can only burn rate limit and write a misleading state.
func TestSyncQueue_OnlyIncludesHandedOverOrders(t *testing.T) {
	db := newTrackingDB(t)
	repo := repositories.New(db)

	inProduction := seedOrderAtStage(t, db, "100001", models.SellerStatusProduction, "940011")
	packed := seedOrderAtStage(t, db, "100002", models.SellerStatusPacked, "940022")
	handedOff := seedOrderAtStage(t, db, "100003", models.SellerStatusHandedOff, "940033")
	shipped := seedOrderAtStage(t, db, "100004", models.SellerStatusShipped, "940044")

	due, err := repo.Tracking.DueForSync(50, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("DueForSync: %v", err)
	}
	queued := map[uint]bool{}
	for _, o := range due {
		queued[o.ID] = true
	}
	if queued[inProduction.ID] {
		t.Error("an order still in production was queued for a provider call")
	}
	if queued[packed.ID] {
		t.Error("a packed-but-not-handed-over order was queued for a provider call")
	}
	if !queued[handedOff.ID] || !queued[shipped.ID] {
		t.Errorf("handed-over orders missing from the queue: %v", queued)
	}
}

// Same boundary for the reverse lookup: an order still in production has no
// parcel out there to match its store order id against.
func TestResolveQueue_OnlyIncludesHandedOverOrders(t *testing.T) {
	db := newTrackingDB(t)
	repo := repositories.New(db)

	inProduction := seedOrderAtStage(t, db, "100001", models.SellerStatusProduction, "")
	handedOff := seedOrderAtStage(t, db, "100002", models.SellerStatusHandedOff, "")

	rows, err := repo.Tracking.AwaitingTrackingNumber(50, time.Now().AddDate(0, 0, -60))
	if err != nil {
		t.Fatalf("AwaitingTrackingNumber: %v", err)
	}
	ids := map[uint]bool{}
	for _, o := range rows {
		ids[o.ID] = true
	}
	if ids[inProduction.ID] {
		t.Error("an order still in production was queued for a reverse lookup")
	}
	if !ids[handedOff.ID] {
		t.Error("a handed-over order without a tracking number was not queued for a reverse lookup")
	}
}

// CS may type a tracking number in while the order is still being made. That is
// ours to keep — but it must NOT reach the provider yet, both because the parcel
// does not exist for the carrier and because registering it spends quota.
func TestRegisterOrder_WaitsForHandover(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	sync := newSync(t, db, f)

	early := seedOrderAtStage(t, db, "100001", models.SellerStatusProduction, "940011")
	if err := sync.RegisterOrder(t.Context(), early); err != nil {
		t.Fatalf("RegisterOrder: %v", err)
	}
	if f.calls["register"] != 0 {
		t.Fatalf("provider was called %d time(s) for an order still in production", f.calls["register"])
	}

	// Handing over is what starts the journey.
	early.SellerStatus = models.SellerStatusHandedOff
	if err := sync.RegisterOrder(t.Context(), early); err != nil {
		t.Fatalf("RegisterOrder after handover: %v", err)
	}
	if f.calls["register"] != 1 {
		t.Fatalf("expected exactly 1 register call after handover, got %d", f.calls["register"])
	}
	if got := f.parcels["940011"].Description; got != "FFM:SO-100001" {
		t.Fatalf("parcel description = %q, want FFM:SO-100001", got)
	}
}

// Regression: the push path does TWO things — register, then pull the parcel's
// current state. An early return from the first that did not also stop the
// second let a "Delivered" land on an order still sitting in production.
func TestSyncOrder_WaitsForHandover(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	f.parcels["940011"] = &fakeParcel{Carrier: "USPS", Status: "Delivered", Location: "AUSTIN, TX"}
	sync := newSync(t, db, f)

	early := seedOrderAtStage(t, db, "100001", models.SellerStatusProduction, "940011")
	changed, err := sync.syncOrder(t.Context(), early)
	if err != nil {
		t.Fatalf("syncOrder: %v", err)
	}
	if changed {
		t.Fatal("syncOrder reported a change for an order that has not been handed over")
	}
	if f.calls["detail"] != 0 {
		t.Fatalf("provider was asked for the parcel state (%d call(s))", f.calls["detail"])
	}

	var got models.Order
	db.First(&got, early.ID)
	if got.TrackingStatus != models.TrackingPending {
		t.Fatalf("carrier state leaked onto a not-yet-handed-over order: %s", got.TrackingStatus)
	}
	if got.TrackingSyncedAt != nil {
		t.Fatal("order was stamped as synced without any provider call")
	}
}

// Same regression through the real entry point: CS attaches a tracking number
// while the order is still being made. The number is ours to keep, but nothing
// may reach the provider until handover.
func TestRegisterOrderAsync_WaitsForHandover(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	f.parcels["940011"] = &fakeParcel{Carrier: "USPS", Status: "Delivered"}
	sync := newSync(t, db, f)

	early := seedOrderAtStage(t, db, "100001", models.SellerStatusProduction, "940011")
	sync.RegisterOrderAsync(early)
	// The goroutine, had the gate been missing, would have called out immediately;
	// a short wait makes the assertion meaningful rather than a race we always win.
	time.Sleep(150 * time.Millisecond)

	if f.calls["register"] != 0 || f.calls["detail"] != 0 {
		t.Fatalf("provider was contacted (register=%d detail=%d)", f.calls["register"], f.calls["detail"])
	}
	var got models.Order
	db.First(&got, early.ID)
	if got.TrackingStatus != models.TrackingPending {
		t.Fatalf("tracking_status became %s before handover", got.TrackingStatus)
	}
}

// The reverse lookup must not even ask about a not-yet-handed-over order.
func TestResolveOrder_WaitsForHandover(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	f.parcels["940022"] = &fakeParcel{Description: "FFM:SO-100001", Status: "In Transit"}
	sync := newSync(t, db, f)

	early := seedOrderAtStage(t, db, "100001", models.SellerStatusProduction, "")
	number, err := sync.ResolveOrder(t.Context(), early)
	if err != nil {
		t.Fatalf("ResolveOrder: %v", err)
	}
	if number != "" {
		t.Fatalf("adopted parcel %q before the order was handed over", number)
	}
	if f.calls["list"] != 0 {
		t.Fatalf("provider was queried %d time(s) for an order still in production", f.calls["list"])
	}

	// After handover the same lookup finds the parcel.
	early.SellerStatus = models.SellerStatusHandedOff
	number, err = sync.ResolveOrder(t.Context(), early)
	if err != nil {
		t.Fatalf("ResolveOrder after handover: %v", err)
	}
	if number != "940022" {
		t.Fatalf("resolved %q after handover, want 940022", number)
	}
}

// Pressing "Tra tracking" on an order that has not been handed over must SAY so.
// Returning an unchanged order would read as "24hTrack has no data" when the real
// answer is "this parcel has not left the factory yet".
func TestSyncOrderByID_ExplainsMissingHandover(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	sync := newSync(t, db, f)
	early := seedOrderAtStage(t, db, "100001", models.SellerStatusProduction, "940011")

	_, err := sync.SyncOrderByID(t.Context(), Actor{Role: models.RoleCS}, early.ID)
	if err == nil {
		t.Fatal("expected an error for an order that has not been handed over")
	}
	if !strings.Contains(err.Error(), "bàn giao") {
		t.Fatalf("error does not point at the handover step: %v", err)
	}
	if f.calls["detail"] != 0 || f.calls["register"] != 0 {
		t.Fatalf("provider was contacted anyway (detail=%d register=%d)", f.calls["detail"], f.calls["register"])
	}
}

// A full scheduled pass must leave not-yet-handed-over orders alone entirely.
func TestRunOnce_SkipsOrdersStillInProduction(t *testing.T) {
	db := newTrackingDB(t)
	f := newFakeProvider()
	f.parcels["940011"] = &fakeParcel{Carrier: "USPS", Status: "In Transit"}
	sync := newSync(t, db, f)

	seedOrderAtStage(t, db, "100001", models.SellerStatusProduction, "940011")
	seedOrderAtStage(t, db, "100002", models.SellerStatusPacked, "")

	stats := sync.RunOnce(t.Context(), 40)
	if stats.Synced != 0 || stats.Resolved != 0 || stats.Failed != 0 {
		t.Fatalf("pass touched orders it should not have: %+v", stats)
	}
	if f.calls["detail"] != 0 || f.calls["list"] != 0 {
		t.Fatalf("provider was contacted (detail=%d list=%d)", f.calls["detail"], f.calls["list"])
	}
}
