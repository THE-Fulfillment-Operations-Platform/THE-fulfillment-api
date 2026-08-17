package services

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// The CS bulk flow: upload the carrier's (order id, tracking number) sheet,
// preview the match against the system, commit only what the operator confirmed.
// The matcher must never guess — every uncertain line becomes an issue.

func newTrackingImportFixture(t *testing.T) (*gorm.DB, *OrderService) {
	t.Helper()
	db := newTrackingDB(t)
	repo := repositories.New(db)
	audit := &AuditService{repo: repo}
	// Nil provider client: the 24hTrack integration reports Enabled()=false and
	// the async pushes are no-ops, which is exactly production-without-provider.
	svc := &OrderService{repo: repo, audit: audit, tracking: NewTrackingSyncService(repo, audit, nil, "FFM", false)}
	return db, svc
}

func seedImportOrder(t *testing.T, db *gorm.DB, code, storeOrder, tracking string, handedAt *time.Time) *models.Order {
	t.Helper()
	o := &models.Order{
		InternalCode: code, StoreOrderID: storeOrder, SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusHandedOff,
		TrackingNumber: tracking, TrackingStatus: models.TrackingNone,
		HandedOverAt: handedAt, ShippingName: "Người nhận " + code,
	}
	if tracking != "" {
		o.TrackingStatus = models.TrackingPending
	}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed order %s: %v", code, err)
	}
	return o
}

// The template the CS downloads must round-trip through our own parser —
// otherwise the "tải file mẫu" button hands out a file the upload rejects.
func TestTrackingImport_TemplateRoundTrips(t *testing.T) {
	_, svc := newTrackingImportFixture(t)
	data, filename, err := svc.TrackingImportTemplateXLSX()
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	if filename == "" || len(data) == 0 {
		t.Fatalf("empty template download: %q, %d bytes", filename, len(data))
	}
	rows, err := ParseTrackingXLSX(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("template does not parse with our own parser: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected the 2 sample rows, got %d", len(rows))
	}
	if rows[0].OrderKey == "" || rows[0].TrackingNumber == "" {
		t.Fatalf("sample row came back empty: %+v", rows[0])
	}
}

// Vietnamese and English headers, in any punctuation, must both be accepted —
// the carrier's export and a hand-made sheet name the columns differently.
func TestTrackingImport_ParseHeaderAliases(t *testing.T) {
	for _, header := range []string{
		"OrderID,Tracking",
		"Mã đơn,Mã vận đơn",
		"order_id,tracking_number",
		"Mã đơn hàng,Mã tracking",
	} {
		rows, err := ParseTrackingCSV(strings.NewReader(header + "\n4141801137,YT123\n"))
		if err != nil {
			t.Fatalf("header %q rejected: %v", header, err)
		}
		if len(rows) != 1 || rows[0].OrderKey != "4141801137" || rows[0].TrackingNumber != "YT123" {
			t.Fatalf("header %q parsed wrong: %+v", header, rows)
		}
	}
	if _, err := ParseTrackingCSV(strings.NewReader("Foo,Bar\n1,2\n")); err == nil {
		t.Fatal("unrecognisable headers must be rejected, not guessed")
	}
}

// Every list in the preview must marshal as a JSON array, never null. A nil Go
// slice becomes `null`, and the client reads these with .length/.filter — one
// null crashed the whole preview screen (including its confirm button), so the
// operator saw a correct API response in devtools and nothing on screen.
func TestTrackingImport_PreviewListsAreNeverNullJSON(t *testing.T) {
	db, svc := newTrackingImportFixture(t)
	seedImportOrder(t, db, "100001", "SO-A", "", nil)

	// A file whose every line matches: no issues, and no date range → the two
	// lists that used to come back nil.
	preview, err := svc.PreviewTrackingImport(Actor{ID: 1, Role: models.RoleCS},
		[]TrackingImportRow{{Row: 1, OrderKey: "SO-A", TrackingNumber: "TN-A"}}, nil, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(preview.Issues) != 0 || len(preview.ScopeMissing) != 0 {
		t.Fatalf("fixture should produce empty lists, got %+v", preview.Summary)
	}
	blob, err := json.Marshal(preview)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"matches", "issues", "scope_missing"} {
		if bytes.Contains(blob, []byte(`"`+field+`":null`)) {
			t.Errorf("%s marshalled as null — the client reads it as an array: %s", field, blob)
		}
	}
}

func TestTrackingImport_PreviewMatchesAndIssues(t *testing.T) {
	db, svc := newTrackingImportFixture(t)
	yesterday := time.Now().Add(-24 * time.Hour)

	a := seedImportOrder(t, db, "100001", "SO-A", "", &yesterday)
	b := seedImportOrder(t, db, "100002", "DUP", "", &yesterday)
	seedImportOrder(t, db, "100003", "DUP", "", &yesterday) // makes "DUP" ambiguous
	d := seedImportOrder(t, db, "100004", "SO-D", "OLD1", &yesterday)
	seedImportOrder(t, db, "100005", "SO-E", "SAME", &yesterday)
	f := seedImportOrder(t, db, "100006", "SO-F", "", &yesterday)

	rows := []TrackingImportRow{
		{Row: 1, OrderKey: "SO-A", TrackingNumber: "TN-A"},     // store id → but conflicts with row 8
		{Row: 2, OrderKey: "100002", TrackingNumber: "TN-B"},   // internal code wins over the DUP store id
		{Row: 3, OrderKey: "DUP", TrackingNumber: "TN-C"},      // ambiguous store id
		{Row: 4, OrderKey: "SO-D", TrackingNumber: "TN-D"},     // overwrite OLD1
		{Row: 5, OrderKey: "SO-E", TrackingNumber: "SAME"},     // unchanged
		{Row: 6, OrderKey: "NOPE", TrackingNumber: "TN-X"},     // not found
		{Row: 7, OrderKey: "SO-F", TrackingNumber: "OLD1"},     // number already on order D
		{Row: 8, OrderKey: "100001", TrackingNumber: "TN-A2"},  // second, different number for order A
		{Row: 9, OrderKey: "", TrackingNumber: "TN-EMPTY"},     // no order key
		{Row: 10, OrderKey: "SO-A", TrackingNumber: ""},        // no tracking number
	}

	preview, err := svc.PreviewTrackingImport(Actor{ID: 1, Role: models.RoleCS}, rows, nil, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	byAction := map[string][]TrackingImportMatch{}
	for _, m := range preview.Matches {
		byAction[m.Action] = append(byAction[m.Action], m)
	}
	if len(byAction[TrackingActionAssign]) != 1 || byAction[TrackingActionAssign][0].OrderID != b.ID {
		t.Errorf("expected exactly order %d (100002) as ASSIGN, got %+v", b.ID, byAction[TrackingActionAssign])
	}
	if len(byAction[TrackingActionOverwrite]) != 1 || byAction[TrackingActionOverwrite][0].OrderID != d.ID ||
		byAction[TrackingActionOverwrite][0].CurrentTracking != "OLD1" {
		t.Errorf("expected order %d as OVERWRITE of OLD1, got %+v", d.ID, byAction[TrackingActionOverwrite])
	}
	if len(byAction[TrackingActionUnchanged]) != 1 {
		t.Errorf("expected one UNCHANGED, got %+v", byAction[TrackingActionUnchanged])
	}

	codes := map[string]int{}
	for _, is := range preview.Issues {
		codes[is.Code]++
	}
	want := map[string]int{
		TrackingIssueAmbiguous: 1, TrackingIssueNotFound: 1, TrackingIssueTakenTracking: 1,
		TrackingIssueConflict: 2, TrackingIssueEmptyOrder: 1, TrackingIssueEmptyTracking: 1,
	}
	for code, n := range want {
		if codes[code] != n {
			t.Errorf("issue %s: want %d, got %d (all: %v)", code, n, codes[code], codes)
		}
	}
	if preview.Summary.Assign != 1 || preview.Summary.Overwrite != 1 || preview.Summary.Unchanged != 1 {
		t.Errorf("summary counts wrong: %+v", preview.Summary)
	}
	if preview.Summary.ScopeTotal != -1 || preview.Summary.ScopeMissing != -1 {
		t.Errorf("scope must be 'not computed' (-1) without a date range: %+v", preview.Summary)
	}
	// Order A ended in conflict — it must not be silently assigned.
	var reloaded models.Order
	if err := db.First(&reloaded, a.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.TrackingNumber != "" {
		t.Errorf("preview must write nothing, but order A now has %q", reloaded.TrackingNumber)
	}
	_ = f
}

// The count alert: with a date range, the preview reports how many handed-over
// orders still without a number the file does NOT cover.
func TestTrackingImport_PreviewScopeComparison(t *testing.T) {
	db, svc := newTrackingImportFixture(t)
	inRange := time.Now().Add(-24 * time.Hour)
	outOfRange := time.Now().Add(-30 * 24 * time.Hour)

	covered := seedImportOrder(t, db, "100001", "SO-A", "", &inRange)
	missing1 := seedImportOrder(t, db, "100002", "SO-B", "", &inRange)
	missing2 := seedImportOrder(t, db, "100003", "SO-C", "", &inRange)
	seedImportOrder(t, db, "100004", "SO-OLD", "", &outOfRange)   // outside the range
	seedImportOrder(t, db, "100005", "SO-DONE", "HAS1", &inRange) // already has a number

	from := time.Now().Add(-48 * time.Hour)
	to := time.Now().Add(time.Hour)
	preview, err := svc.PreviewTrackingImport(Actor{ID: 1, Role: models.RoleCS},
		[]TrackingImportRow{{Row: 1, OrderKey: "SO-A", TrackingNumber: "TN-A"}}, &from, &to)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.Summary.ScopeTotal != 3 {
		t.Errorf("scope total: want 3 (A, B, C), got %d", preview.Summary.ScopeTotal)
	}
	if preview.Summary.ScopeMissing != 2 {
		t.Errorf("scope missing: want 2 (B, C), got %d", preview.Summary.ScopeMissing)
	}
	missingIDs := map[uint]bool{}
	for _, ref := range preview.ScopeMissing {
		missingIDs[ref.ID] = true
	}
	if missingIDs[covered.ID] || !missingIDs[missing1.ID] || !missingIDs[missing2.ID] {
		t.Errorf("scope-missing list wrong: %v", missingIDs)
	}
}

func TestTrackingImport_CommitAppliesAndReportsFailures(t *testing.T) {
	db, svc := newTrackingImportFixture(t)
	o1 := seedImportOrder(t, db, "100001", "SO-A", "", nil)
	o2 := seedImportOrder(t, db, "100002", "SO-B", "", nil)

	res, err := svc.CommitTrackingImport(Actor{ID: 1, Role: models.RoleCS}, TrackingImportCommitInput{
		Assignments: []TrackingAssignment{
			{OrderID: o1.ID, TrackingNumber: "TN-1"},
			{OrderID: o2.ID, TrackingNumber: "TN-2"},
			{OrderID: 999999, TrackingNumber: "TN-3"}, // vanished between preview and commit
			{OrderID: o1.ID, TrackingNumber: "TN-DUP"},
		},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Updated != 2 {
		t.Errorf("updated: want 2, got %d", res.Updated)
	}
	if len(res.Failed) != 2 {
		t.Errorf("failed: want 2 (missing order + duplicate), got %+v", res.Failed)
	}
	var reloaded models.Order
	if err := db.First(&reloaded, o1.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.TrackingNumber != "TN-1" || reloaded.TrackingStatus != models.TrackingPending {
		t.Errorf("order 1 not updated correctly: %q / %s", reloaded.TrackingNumber, reloaded.TrackingStatus)
	}
	if reloaded.TrackingUpdatedAt == nil {
		t.Error("tracking_updated_at not stamped")
	}
}

// Same role boundary as the single-order edit.
func TestTrackingImport_RoleGate(t *testing.T) {
	_, svc := newTrackingImportFixture(t)
	if _, err := svc.PreviewTrackingImport(Actor{ID: 1, Role: models.RoleProduction}, nil, nil, nil); err == nil {
		t.Error("preview must refuse roles outside the tracking editors")
	}
	if _, err := svc.CommitTrackingImport(Actor{ID: 1, Role: models.RoleProduction}, TrackingImportCommitInput{
		Assignments: []TrackingAssignment{{OrderID: 1, TrackingNumber: "X"}},
	}); err == nil {
		t.Error("commit must refuse roles outside the tracking editors")
	}
}
