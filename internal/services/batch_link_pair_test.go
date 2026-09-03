package services

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// newPairFixture builds a flat PENDING batch with n live items through the real
// Create flow, so fan-out assertions run against genuine batch_items rows.
func newPairFixture(t *testing.T, n int) (*gorm.DB, *BatchService, *models.Batch) {
	t.Helper()
	db := newSplitDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, n)
	batch, _, err := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("create fixture batch: %v", err)
	}
	return db, svc, batch
}

func pairAuditCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&models.AuditLog{}).Where("action = ?", "BATCH_PRODUCTION_PACKAGE_SET").Count(&n).Error; err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

func batchLinkCount(t *testing.T, db *gorm.DB, batchID uint) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&models.BatchLink{}).Where("batch_id = ?", batchID).Count(&n).Error; err != nil {
		t.Fatalf("count links: %v", err)
	}
	return n
}

// TestSetBatchLinkPair_AssignsBothAndFansOut: one call saves BOTH production
// links and stamps both per-item columns, and the audit records the whole
// package once.
func TestSetBatchLinkPair_AssignsBothAndFansOut(t *testing.T) {
	db, svc, batch := newPairFixture(t, 2)

	res, err := svc.SetBatchLinkPair(Actor{ID: 7}, batch.ID, SetBatchLinkPairInput{
		PrintURL: " https://files/print-v1 ", CutURL: "https://files/cut-v1",
	})
	if err != nil {
		t.Fatalf("set pair: %v", err)
	}
	if res.Action != BatchLinkActionAssign {
		t.Fatalf("action: got %q, want ASSIGN", res.Action)
	}
	if len(res.Links) != 2 {
		t.Fatalf("want both links back, got %d", len(res.Links))
	}
	byKind := map[models.BatchLinkKind]string{}
	for _, l := range res.Links {
		byKind[l.Kind] = l.URL
	}
	if byKind[models.BatchLinkPrint] != "https://files/print-v1" || byKind[models.BatchLinkCut] != "https://files/cut-v1" {
		t.Fatalf("URLs not trimmed/saved: %v", byKind)
	}

	var items []models.OrderItem
	if err := db.Find(&items).Error; err != nil {
		t.Fatalf("load items: %v", err)
	}
	for _, it := range items {
		if it.PrintFileURL != "https://files/print-v1" || it.CutFileURL != "https://files/cut-v1" {
			t.Fatalf("item %d not fanned out: print=%q cut=%q", it.ID, it.PrintFileURL, it.CutFileURL)
		}
	}
	if got := pairAuditCount(t, db); got != 1 {
		t.Fatalf("want exactly one package audit, got %d", got)
	}
}

// TestSetBatchLinkPair_ValidatesURLs: both URLs are required and must be
// http(s); nothing is written on rejection.
func TestSetBatchLinkPair_ValidatesURLs(t *testing.T) {
	db, svc, batch := newPairFixture(t, 1)

	cases := []SetBatchLinkPairInput{
		{PrintURL: "", CutURL: "https://files/cut"},
		{PrintURL: "https://files/print", CutURL: "   "},
		{PrintURL: "javascript:alert(1)", CutURL: "https://files/cut"},
		{PrintURL: "https://files/print", CutURL: "ftp://files/cut"},
		{PrintURL: "https://files/print", CutURL: "https://files/" + strings.Repeat("x", 500)},
	}
	for i, in := range cases {
		if _, err := svc.SetBatchLinkPair(Actor{ID: 1}, batch.ID, in); err == nil {
			t.Fatalf("case %d: invalid input must be rejected: %+v", i, in)
		}
	}
	if got := batchLinkCount(t, db, batch.ID); got != 0 {
		t.Fatalf("no link may be written on rejection, got %d", got)
	}
	if got := pairAuditCount(t, db); got != 0 {
		t.Fatalf("no audit on rejection, got %d", got)
	}
}

// TestSetBatchLinkPair_LockedBatches: parent batches never carry production
// files; closed batches and batches that already started production (PRINTED /
// CUT / QC_PASSED) refuse a new package — mistakes go through scrap/rework, not
// through rewriting history.
func TestSetBatchLinkPair_LockedBatches(t *testing.T) {
	db, svc, _ := newPairFixture(t, 1)
	mat := &models.Material{Code: "LOCK", Name: "Lock"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}

	mk := func(code string, mut func(*models.Batch)) *models.Batch {
		b := &models.Batch{Code: code, MaterialID: mat.ID, Status: models.StatusPending}
		mut(b)
		if err := db.Create(b).Error; err != nil {
			t.Fatalf("seed batch %s: %v", code, err)
		}
		return b
	}
	now := time.Now()
	parent := mk("#L-P", func(b *models.Batch) { b.IsParent = true })
	closed := mk("#L-C", func(b *models.Batch) { b.ClosedAt = &now })
	printed := mk("#L-1", func(b *models.Batch) { b.Status = models.StatusPrinted })
	cut := mk("#L-2", func(b *models.Batch) { b.Status = models.StatusCut })
	qced := mk("#L-3", func(b *models.Batch) { b.Status = models.StatusQCPassed })

	in := SetBatchLinkPairInput{PrintURL: "https://files/p", CutURL: "https://files/c"}
	for _, b := range []*models.Batch{parent, closed, printed, cut, qced} {
		if _, err := svc.SetBatchLinkPair(Actor{ID: 1}, b.ID, in); err == nil {
			t.Fatalf("batch %s must refuse the package", b.Code)
		}
	}
	// Production-started batches get the explicit lock message.
	_, err := svc.SetBatchLinkPair(Actor{ID: 1}, printed.ID, in)
	if err == nil || !strings.Contains(err.Error(), "khóa") {
		t.Fatalf("lock message should say the package is locked, got %v", err)
	}
}

// TestSetBatchLinkPair_NoLiveItems: a batch whose every part was scrapped has
// nothing left to produce — attaching a fresh package to it is a mistake.
func TestSetBatchLinkPair_NoLiveItems(t *testing.T) {
	db, svc, batch := newPairFixture(t, 1)
	now := time.Now()
	if err := db.Model(&models.BatchItem{}).Where("batch_id = ?", batch.ID).
		Updates(map[string]any{"scrapped_at": now, "scrap_reason": "vỡ"}).Error; err != nil {
		t.Fatalf("scrap parts: %v", err)
	}

	if _, err := svc.SetBatchLinkPair(Actor{ID: 1}, batch.ID, SetBatchLinkPairInput{
		PrintURL: "https://files/p", CutURL: "https://files/c",
	}); err == nil {
		t.Fatalf("batch without live items must be rejected")
	}
	if got := batchLinkCount(t, db, batch.ID); got != 0 {
		t.Fatalf("no link may be written, got %d", got)
	}
}

// TestSetBatchLinkPair_ReplaceNeedsReason: replacing an existing package is a
// deliberate act — without a reason it is refused; with one it lands and the
// audit keeps both the old and the new URLs plus the reason.
func TestSetBatchLinkPair_ReplaceNeedsReason(t *testing.T) {
	db, svc, batch := newPairFixture(t, 1)
	actor := Actor{ID: 7}

	if _, err := svc.SetBatchLinkPair(actor, batch.ID, SetBatchLinkPairInput{
		PrintURL: "https://files/print-v1", CutURL: "https://files/cut-v1",
	}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// One URL differs → REPLACE → reason required.
	if _, err := svc.SetBatchLinkPair(actor, batch.ID, SetBatchLinkPairInput{
		PrintURL: "https://files/print-v2", CutURL: "https://files/cut-v1",
	}); err == nil {
		t.Fatalf("replace without a reason must be refused")
	}

	res, err := svc.SetBatchLinkPair(actor, batch.ID, SetBatchLinkPairInput{
		PrintURL: "https://files/print-v2", CutURL: "https://files/cut-v1",
		Reason: "designer nộp nhầm file khổ cũ",
	})
	if err != nil {
		t.Fatalf("replace with reason: %v", err)
	}
	if res.Action != BatchLinkActionReplace {
		t.Fatalf("action: got %q, want REPLACE", res.Action)
	}

	var log models.AuditLog
	if err := db.Where("action = ?", "BATCH_PRODUCTION_PACKAGE_SET").Order("id desc").First(&log).Error; err != nil {
		t.Fatalf("load audit: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(log.Metadata, &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta["prev_print_url"] != "https://files/print-v1" || meta["new_print_url"] != "https://files/print-v2" {
		t.Fatalf("audit must keep old and new print URLs, got %v", meta)
	}
	if meta["reason"] != "designer nộp nhầm file khổ cũ" {
		t.Fatalf("audit must keep the replacement reason, got %v", meta["reason"])
	}
	if meta["action"] != "REPLACE" {
		t.Fatalf("audit action: got %v, want REPLACE", meta["action"])
	}
}

// TestSetBatchLinkPair_UnchangedIsNoOp: resubmitting the identical pair changes
// nothing and must not fabricate an audit trail of a change that never happened.
func TestSetBatchLinkPair_UnchangedIsNoOp(t *testing.T) {
	db, svc, batch := newPairFixture(t, 1)
	actor := Actor{ID: 7}
	in := SetBatchLinkPairInput{PrintURL: "https://files/print-v1", CutURL: "https://files/cut-v1"}

	if _, err := svc.SetBatchLinkPair(actor, batch.ID, in); err != nil {
		t.Fatalf("assign: %v", err)
	}
	var before []models.BatchLink
	if err := db.Where("batch_id = ?", batch.ID).Order("kind").Find(&before).Error; err != nil {
		t.Fatalf("load links: %v", err)
	}

	res, err := svc.SetBatchLinkPair(actor, batch.ID, in)
	if err != nil {
		t.Fatalf("no-op resubmit: %v", err)
	}
	if res.Action != BatchLinkActionUnchanged {
		t.Fatalf("action: got %q, want UNCHANGED", res.Action)
	}
	var after []models.BatchLink
	if err := db.Where("batch_id = ?", batch.ID).Order("kind").Find(&after).Error; err != nil {
		t.Fatalf("reload links: %v", err)
	}
	for i := range before {
		if !before[i].LinkUpdatedAt.Equal(after[i].LinkUpdatedAt) {
			t.Fatalf("no-op must not restamp link_updated_at (%s)", before[i].Kind)
		}
	}
	if got := pairAuditCount(t, db); got != 1 {
		t.Fatalf("no-op must not add an audit row, got %d", got)
	}
}

// TestSetBatchLinkPair_RollsBackWhenCutSaveFails: if saving the CUT link fails,
// the already-saved PRINT link must roll back too — a batch never holds half a
// package.
func TestSetBatchLinkPair_RollsBackWhenCutSaveFails(t *testing.T) {
	db, svc, batch := newPairFixture(t, 2)
	if err := db.Exec(`CREATE TRIGGER fail_cut_link BEFORE INSERT ON batch_links
		WHEN NEW.kind = 'CUT' BEGIN SELECT RAISE(ABORT, 'cut insert blocked'); END`).Error; err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, err := svc.SetBatchLinkPair(Actor{ID: 1}, batch.ID, SetBatchLinkPairInput{
		PrintURL: "https://files/print", CutURL: "https://files/cut",
	}); err == nil {
		t.Fatalf("blocked CUT insert must surface as an error")
	}
	if got := batchLinkCount(t, db, batch.ID); got != 0 {
		t.Fatalf("PRINT must roll back with CUT, found %d link rows", got)
	}
	var items []models.OrderItem
	if err := db.Find(&items).Error; err != nil {
		t.Fatalf("load items: %v", err)
	}
	for _, it := range items {
		if it.PrintFileURL != "" || it.CutFileURL != "" {
			t.Fatalf("no fan-out may survive the rollback: item %d print=%q cut=%q", it.ID, it.PrintFileURL, it.CutFileURL)
		}
	}
	if got := pairAuditCount(t, db); got != 0 {
		t.Fatalf("no audit after a rollback, got %d", got)
	}
}

// TestSetBatchLinkPair_RollsBackWhenFanoutFails: both BatchLink rows must roll
// back when the fan-out onto the items fails — the link table and the item
// columns never disagree.
func TestSetBatchLinkPair_RollsBackWhenFanoutFails(t *testing.T) {
	db, svc, batch := newPairFixture(t, 2)
	if err := db.Exec(`CREATE TRIGGER fail_cut_fanout BEFORE UPDATE OF cut_file_url ON order_items
		BEGIN SELECT RAISE(ABORT, 'fanout blocked'); END`).Error; err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, err := svc.SetBatchLinkPair(Actor{ID: 1}, batch.ID, SetBatchLinkPairInput{
		PrintURL: "https://files/print", CutURL: "https://files/cut",
	}); err == nil {
		t.Fatalf("blocked fan-out must surface as an error")
	}
	if got := batchLinkCount(t, db, batch.ID); got != 0 {
		t.Fatalf("both BatchLink rows must roll back, found %d", got)
	}
	var items []models.OrderItem
	if err := db.Find(&items).Error; err != nil {
		t.Fatalf("load items: %v", err)
	}
	for _, it := range items {
		if it.PrintFileURL != "" {
			t.Fatalf("print fan-out must roll back too: item %d print=%q", it.ID, it.PrintFileURL)
		}
	}
}

// TestSetBatchLinkPair_SkipsScrappedAndCancelled: the fan-out only stamps live
// parts — a scrapped part waits for its re-make in a NEW batch, and a cancelled
// line is never produced.
func TestSetBatchLinkPair_SkipsScrappedAndCancelled(t *testing.T) {
	db, svc, batch := newPairFixture(t, 3)
	var parts []models.BatchItem
	if err := db.Where("batch_id = ?", batch.ID).Order("id").Find(&parts).Error; err != nil {
		t.Fatalf("load parts: %v", err)
	}
	now := time.Now()
	if err := db.Model(&parts[1]).Updates(map[string]any{"scrapped_at": now, "scrap_reason": "vỡ"}).Error; err != nil {
		t.Fatalf("scrap part: %v", err)
	}
	if err := db.Model(&models.OrderItem{}).Where("id = ?", parts[2].OrderItemID).
		Update("cancellation_status", models.CancellationApproved).Error; err != nil {
		t.Fatalf("cancel item: %v", err)
	}

	if _, err := svc.SetBatchLinkPair(Actor{ID: 1}, batch.ID, SetBatchLinkPairInput{
		PrintURL: "https://files/print", CutURL: "https://files/cut",
	}); err != nil {
		t.Fatalf("set pair: %v", err)
	}

	assertURLs := func(orderItemID uint, wantPrint, wantCut string) {
		t.Helper()
		var it models.OrderItem
		if err := db.First(&it, orderItemID).Error; err != nil {
			t.Fatalf("load item %d: %v", orderItemID, err)
		}
		if it.PrintFileURL != wantPrint || it.CutFileURL != wantCut {
			t.Fatalf("item %d: got print=%q cut=%q, want %q/%q", orderItemID, it.PrintFileURL, it.CutFileURL, wantPrint, wantCut)
		}
	}
	assertURLs(parts[0].OrderItemID, "https://files/print", "https://files/cut")
	assertURLs(parts[1].OrderItemID, "", "")
	assertURLs(parts[2].OrderItemID, "", "")
}

// TestSetBatchLink_LockedAfterProductionStart: the legacy single-link endpoint
// obeys the same lock — once a batch left PENDING its package is history, not an
// editable field.
func TestSetBatchLink_LockedAfterProductionStart(t *testing.T) {
	db, svc, batch := newPairFixture(t, 1)
	seedBatchLinks(t, db, batch.ID)
	if _, err := svc.UpdateStatus(Actor{ID: 1, Role: models.RoleProduction}, batch.ID,
		UpdateStatusInput{Status: string(models.StatusPrinted)}); err != nil {
		t.Fatalf("advance to PRINTED: %v", err)
	}

	_, err := svc.SetBatchLink(Actor{ID: 1}, batch.ID, SetBatchLinkInput{
		Kind: "PRINT", URL: "https://files/print-v2", Reason: "đổi file",
	})
	if err == nil || !strings.Contains(err.Error(), "khóa") {
		t.Fatalf("single-link edit must be locked after production starts, got %v", err)
	}
}

// TestSetBatchLink_ReplaceRequiresReason: replacing a single link now needs a
// reason too, and the audit keeps the old URL, the new URL and that reason.
func TestSetBatchLink_ReplaceRequiresReason(t *testing.T) {
	db, svc, batch := newPairFixture(t, 1)
	actor := Actor{ID: 7}

	if _, err := svc.SetBatchLink(actor, batch.ID, SetBatchLinkInput{Kind: "PRINT", URL: "https://files/print-v1"}); err != nil {
		t.Fatalf("add link: %v", err)
	}
	if _, err := svc.SetBatchLink(actor, batch.ID, SetBatchLinkInput{Kind: "PRINT", URL: "https://files/print-v2"}); err == nil {
		t.Fatalf("replacing with a different URL without a reason must be refused")
	}
	if _, err := svc.SetBatchLink(actor, batch.ID, SetBatchLinkInput{
		Kind: "PRINT", URL: "https://files/print-v2", Reason: "file cũ sai khổ",
	}); err != nil {
		t.Fatalf("replace with reason: %v", err)
	}

	var log models.AuditLog
	if err := db.Where("action = ?", "BATCH_LINK_UPDATE").Order("id desc").First(&log).Error; err != nil {
		t.Fatalf("load audit: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(log.Metadata, &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta["old_url"] != "https://files/print-v1" || meta["url"] != "https://files/print-v2" {
		t.Fatalf("audit must keep old and new URLs, got %v", meta)
	}
	if meta["reason"] != "file cũ sai khổ" {
		t.Fatalf("audit must keep the reason, got %v", meta["reason"])
	}
}
