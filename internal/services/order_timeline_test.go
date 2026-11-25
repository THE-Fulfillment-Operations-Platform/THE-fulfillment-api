package services

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newTimelineDB(t *testing.T) (*gorm.DB, *repositories.Repositories) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Seller{}, &models.User{}, &models.Material{}, &models.Order{}, &models.OrderItem{},
		&models.Batch{}, &models.BatchItem{}, &models.StatusHistory{}, &models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db, repositories.New(db)
}

var tBase = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

func at(h int) time.Time { return tBase.Add(time.Duration(h) * time.Hour) }

func seedHist(t *testing.T, db *gorm.DB, et models.EntityType, id uint, to string, by *uint, when time.Time) {
	t.Helper()
	row := &models.StatusHistory{EntityType: et, EntityID: id, ToStatus: to, ChangedByID: by}
	row.CreatedAt = when
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("seed history: %v", err)
	}
}

func stepOf(t *testing.T, tl *Timeline, key string) TimelineStep {
	t.Helper()
	for _, s := range tl.Steps {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no step %q in %+v", key, tl.Steps)
	return TimelineStep{}
}

func wantAt(t *testing.T, tl *Timeline, key string, want time.Time) TimelineStep {
	t.Helper()
	s := stepOf(t, tl, key)
	if s.At == nil || !s.At.Equal(want) {
		t.Errorf("%s = %v, want %v", key, s.At, want)
	}
	return s
}

// Two items: the order is through a stage only when its LAST item is, "lần đầu"
// is kept even when QC sent an item back, and a cancelled line is ignored.
func TestOrderTimeline_TwoItemsLastItemCounts(t *testing.T) {
	db, repo := newTimelineDB(t)
	svc := &OrderService{repo: repo}
	ops := &models.User{Email: "o@x.test", FullName: "Thợ Ops", Role: models.RoleOps, PasswordHash: "x"}
	cut := &models.User{Email: "c@x.test", FullName: "Thợ Cắt", Role: models.RoleOps, PasswordHash: "x"}
	db.Create(ops)
	db.Create(cut)

	o := &models.Order{InternalCode: "200001", StoreOrderID: "SO-9", SellerID: 1, CreatedByID: &ops.ID}
	o.CreatedAt = at(0)
	db.Create(o)
	a := &models.OrderItem{OrderID: o.ID, InternalCode: "200001_1/2"}
	b := &models.OrderItem{OrderID: o.ID, InternalCode: "200001_2/2"}
	gone := &models.OrderItem{OrderID: o.ID, InternalCode: "200001_x", CancellationStatus: models.CancellationApproved}
	for _, it := range []*models.OrderItem{a, b, gone} {
		db.Create(it)
	}
	seedHist(t, db, models.EntityOrder, o.ID, "APPROVED", &ops.ID, at(1))
	seedHist(t, db, models.EntityOrder, o.ID, "HANDED_OFF", &ops.ID, at(30))

	ba := &models.BatchItem{BatchID: 1, OrderItemID: a.ID, MaterialID: 1}
	ba.CreatedAt = at(2)
	bb := &models.BatchItem{BatchID: 2, OrderItemID: b.ID, MaterialID: 1}
	bb.CreatedAt = at(5)
	db.Create(ba)
	db.Create(bb)
	seedHist(t, db, models.EntityBatchItem, bb.ID, "PENDING", &cut.ID, at(5))

	// Item A: printed 3h, cut 4h, QC 6h, then a QC fail sends it back to PENDING
	// and it is printed again at 20h — the first time (3h) is the one that counts.
	seedHist(t, db, models.EntityOrderItem, a.ID, "PRINTED", &ops.ID, at(3))
	seedHist(t, db, models.EntityOrderItem, a.ID, "CUT", &ops.ID, at(4))
	seedHist(t, db, models.EntityOrderItem, a.ID, "QC_PASSED", &ops.ID, at(6))
	seedHist(t, db, models.EntityOrderItem, a.ID, "PENDING", &ops.ID, at(10))
	seedHist(t, db, models.EntityOrderItem, a.ID, "PRINTED", &ops.ID, at(20))
	// Item B jumps straight PENDING -> CUT at 8h (print passed at that moment),
	// QC at 9h by the cutter.
	seedHist(t, db, models.EntityOrderItem, b.ID, "CUT", &ops.ID, at(8))
	seedHist(t, db, models.EntityOrderItem, b.ID, "QC_PASSED", &cut.ID, at(9))
	// The cancelled line was never produced; must not hold any step back.

	tl, err := svc.OrderTimeline(o.ID)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	wantAt(t, tl, "import", at(0))
	wantAt(t, tl, "approved", at(1))
	s := wantAt(t, tl, "batched", at(5)) // last item into a batch
	if s.ByName != "Thợ Cắt" {
		t.Errorf("batched by %q", s.ByName)
	}
	wantAt(t, tl, "printed", at(8)) // A at 3h, B (via CUT) at 8h -> last = 8h
	wantAt(t, tl, "cut", at(8))
	s = wantAt(t, tl, "qc_passed", at(9))
	if s.ByName != "Thợ Cắt" {
		t.Errorf("qc by %q", s.ByName)
	}
	wantAt(t, tl, "handed_off", at(30))
	if s := stepOf(t, tl, "import"); s.ByName != "Thợ Ops" {
		t.Errorf("import by %q", s.ByName)
	}
}

// One item that has not reached a stage keeps the whole order from claiming it.
func TestOrderTimeline_UnfinishedItemLeavesStageEmpty(t *testing.T) {
	db, repo := newTimelineDB(t)
	svc := &OrderService{repo: repo}
	o := &models.Order{InternalCode: "200002", StoreOrderID: "SO-10", SellerID: 1}
	o.CreatedAt = at(0)
	db.Create(o)
	a := &models.OrderItem{OrderID: o.ID, InternalCode: "200002_1/2"}
	b := &models.OrderItem{OrderID: o.ID, InternalCode: "200002_2/2"}
	db.Create(a)
	db.Create(b)
	seedHist(t, db, models.EntityOrderItem, a.ID, "PRINTED", nil, at(3))

	tl, err := svc.OrderTimeline(o.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"approved", "batched", "printed", "cut", "qc_passed", "handed_off"} {
		if s := stepOf(t, tl, k); s.At != nil {
			t.Errorf("%s should be empty, got %v", k, s.At)
		}
	}
	if s := stepOf(t, tl, "import"); s.At == nil {
		t.Error("import is always known")
	}
}

// Without an ORDER -> HANDED_OFF row, orders.handed_over_at answers.
func TestOrderTimeline_HandoffFallsBackToHandedOverAt(t *testing.T) {
	db, repo := newTimelineDB(t)
	svc := &OrderService{repo: repo}
	h := at(40)
	o := &models.Order{InternalCode: "200003", StoreOrderID: "SO-11", SellerID: 1, HandedOverAt: &h}
	db.Create(o)
	tl, err := svc.OrderTimeline(o.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantAt(t, tl, "handed_off", h)
	if _, err := svc.OrderTimeline(9999); err == nil {
		t.Error("missing order must 404")
	}
}

func TestBatchTimeline_FirstTimeEachStatus(t *testing.T) {
	db, repo := newTimelineDB(t)
	svc := &BatchService{repo: repo}
	u := &models.User{Email: "p@x.test", FullName: "Thợ In", Role: models.RoleOps, PasswordHash: "x"}
	db.Create(u)
	b := &models.Batch{Code: "#103000", MaterialID: 1, CreatedByID: &u.ID}
	b.CreatedAt = at(0)
	db.Create(b)
	seedHist(t, db, models.EntityBatch, b.ID, "PRINTED", &u.ID, at(2))
	seedHist(t, db, models.EntityBatch, b.ID, "CUT", nil, at(4))
	seedHist(t, db, models.EntityBatch, b.ID, "PRINTED", nil, at(12)) // reopened later
	tl, err := svc.BatchTimeline(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantAt(t, tl, "created", at(0))
	if s := wantAt(t, tl, "printed", at(2)); s.ByName != "Thợ In" {
		t.Errorf("printed by %q", s.ByName)
	}
	wantAt(t, tl, "cut", at(4))
	if s := stepOf(t, tl, "qc_passed"); s.At != nil {
		t.Errorf("qc should be empty, got %v", s.At)
	}
}

// ---- audit search ----

func addAudit(t *testing.T, db *gorm.DB, action, et string, id uint, email string, when time.Time) {
	t.Helper()
	row := &models.AuditLog{Action: action, EntityType: et, EntityID: &id, ActorEmail: email, Summary: action}
	row.CreatedAt = when
	if err := db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
}

func TestAuditSearch_ByOrderBatchActorAndDate(t *testing.T) {
	db, repo := newTimelineDB(t)
	svc := &AuditService{repo: repo}
	u := &models.User{Email: "lan@x.test", FullName: "Lan Anh", Role: models.RoleOps, PasswordHash: "x"}
	db.Create(u)

	o1 := &models.Order{InternalCode: "300001", StoreOrderID: "ETSY-77", SellerID: 1}
	o2 := &models.Order{InternalCode: "300002", StoreOrderID: "ETSY-78", SellerID: 1}
	db.Create(o1)
	db.Create(o2)
	i1 := &models.OrderItem{OrderID: o1.ID, InternalCode: "300001"}
	i2 := &models.OrderItem{OrderID: o2.ID, InternalCode: "300002"}
	db.Create(i1)
	db.Create(i2)
	bt := &models.Batch{Code: "#103000", MaterialID: 1}
	db.Create(bt)
	bi1 := &models.BatchItem{BatchID: bt.ID, OrderItemID: i1.ID, MaterialID: 1}
	bi2 := &models.BatchItem{BatchID: bt.ID, OrderItemID: i2.ID, MaterialID: 1}
	db.Create(bi1)
	db.Create(bi2)

	addAudit(t, db, "ORDER_EDIT", "order", o1.ID, "a@x.test", at(1))
	addAudit(t, db, "QC_PASS", "order_item", i1.ID, "lan@x.test", at(2))
	addAudit(t, db, "SOMETHING", "batch_item", bi1.ID, "a@x.test", at(3))
	addAudit(t, db, "ORDER_EDIT", "order", o2.ID, "a@x.test", at(4)) // other order
	addAudit(t, db, "BATCH_STATUS_UPDATE", "batch", bt.ID, "a@x.test", at(5))
	// Same numeric id as o1 but another entity type must not leak in.
	addAudit(t, db, "NOTE_CREATE", "note", o1.ID, "a@x.test", at(6))
	// Written by the user's id but with no e-mail: found through the full name.
	nameOnly := &models.AuditLog{Action: "BY_ID", EntityType: "sku", ActorID: &u.ID}
	nameOnly.CreatedAt = at(7)
	db.Create(nameOnly)

	count := func(in AuditSearch) int {
		t.Helper()
		_, n, err := svc.Search(in)
		if err != nil {
			t.Fatal(err)
		}
		return int(n)
	}
	// By internal code: the order, its item and its batch part — 3 lines, not o2's.
	if n := count(AuditSearch{Order: "300001"}); n != 3 {
		t.Errorf("order 300001 = %d lines, want 3", n)
	}
	// By store order id, and by the item's code, reach the same order.
	if n := count(AuditSearch{Order: "ETSY-77"}); n != 3 {
		t.Errorf("store order id = %d, want 3", n)
	}
	if n := count(AuditSearch{Order: " 300001 "}); n != 3 {
		t.Errorf("padded code = %d, want 3", n)
	}
	if n := count(AuditSearch{Order: "999999"}); n != 0 {
		t.Errorf("unknown order must be empty, got %d", n)
	}
	// Batch with and without '#': the batch line + both parts' lines (1 here).
	if n := count(AuditSearch{Batch: "#103000"}); n != 2 {
		t.Errorf("#103000 = %d, want 2", n)
	}
	if n := count(AuditSearch{Batch: "103000"}); n != 2 {
		t.Errorf("103000 = %d, want 2", n)
	}
	if n := count(AuditSearch{Batch: "103000", Order: "300002"}); n != 0 {
		// o2's lines: order edit + nothing on its batch part -> intersect with batch = 0
		t.Errorf("order AND batch = %d, want 0", n)
	}
	if n := count(AuditSearch{Order: "300001", Batch: "103000"}); n != 1 {
		t.Errorf("order AND batch (300001) = %d, want 1", n)
	}
	if n := count(AuditSearch{Batch: "#999"}); n != 0 {
		t.Errorf("unknown batch must be empty, got %d", n)
	}
	// Actor: by e-mail part (case-insensitive) and by full name.
	if n := count(AuditSearch{Actor: "LAN@"}); n != 1 {
		t.Errorf("actor email = %d, want 1", n)
	}
	if n := count(AuditSearch{Actor: "lan"}); n != 2 {
		t.Errorf("actor lan = %d, want 2 (email + by id via name)", n)
	}
	if n := count(AuditSearch{Actor: "lan anh"}); n != 1 {
		t.Errorf("actor by name = %d, want 1", n)
	}
	if n := count(AuditSearch{Actor: "%"}); n != 0 {
		t.Errorf("wildcard must be literal, got %d", n)
	}
	// Date range is half-open [from, before).
	from, before := at(2), at(5)
	if n := count(AuditSearch{From: &from, Before: &before}); n != 3 {
		t.Errorf("range = %d, want 3 (2h,3h,4h)", n)
	}
	// Combined with a code, still scoped to it.
	if n := count(AuditSearch{Order: "300001", From: &from}); n != 2 {
		t.Errorf("order + from = %d, want 2", n)
	}
	// Newest first and paged.
	rows, total, _ := svc.Search(AuditSearch{Page: repositories.Page{Page: 1, PageSize: 2}})
	if total != 7 || len(rows) != 2 || rows[0].Action != "BY_ID" {
		t.Errorf("paging: total=%d rows=%d first=%v", total, len(rows), rows)
	}
}

// A soft-deleted order's ORDER_DELETE line must still be findable by its code.
func TestAuditSearch_FindsSoftDeletedOrder(t *testing.T) {
	db, repo := newTimelineDB(t)
	svc := &AuditService{repo: repo}
	o := &models.Order{InternalCode: "300009", StoreOrderID: "X", SellerID: 1}
	db.Create(o)
	addAudit(t, db, "ORDER_DELETE", "order", o.ID, "a@x.test", at(1))
	db.Delete(o)
	_, n, err := svc.Search(AuditSearch{Order: "300009"})
	if err != nil || n != 1 {
		t.Errorf("deleted order lines = %d, err %v", n, err)
	}
}
