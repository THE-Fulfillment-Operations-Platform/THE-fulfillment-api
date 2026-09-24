package services

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// newExcelFixture seeds two flat PENDING batches (2 items and 1 item) through
// the real Create flow and returns them ordered by id.
func newExcelFixture(t *testing.T) (*gorm.DB, *BatchService, []*models.Batch) {
	t.Helper()
	db := newSplitDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 3)
	b1All, _, err := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids[:2]})
	b1 := firstBatch(t, b1All, err)
	if err != nil {
		t.Fatalf("create batch 1: %v", err)
	}
	// The second batch needs its own material: the same (item, material) cannot
	// be double-batched, and a fresh material keeps the fixture obvious.
	mat2 := &models.Material{Code: "WOOD", Name: "Wood"}
	if err := db.Create(mat2).Error; err != nil {
		t.Fatalf("seed material 2: %v", err)
	}
	sku := &models.SKU{}
	if err := db.First(sku, "code = ?", "MICA-01").Error; err != nil {
		t.Fatalf("load sku: %v", err)
	}
	if err := db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mat2.ID, QuantityPerUnit: 1}).Error; err != nil {
		t.Fatalf("seed sku-material: %v", err)
	}
	b2All, _, err := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: mat2.ID, OrderItemIDs: ids[2:]})
	b2 := firstBatch(t, b2All, err)
	if err != nil {
		t.Fatalf("create batch 2: %v", err)
	}
	return db, svc, []*models.Batch{b1, b2}
}

// importRowsFor turns batches into ready-to-commit import rows.
func importRowsFor(batches []*models.Batch, print, cut string) []BatchLinkImportRow {
	rows := make([]BatchLinkImportRow, 0, len(batches))
	for i, b := range batches {
		rows = append(rows, BatchLinkImportRow{
			Row: i + 1, Version: BatchLinkTemplateVersion,
			BatchID: b.ID, BatchCode: b.Code,
			PrintURL: fmt.Sprintf("%s-%d", print, b.ID), CutURL: fmt.Sprintf("%s-%d", cut, b.ID),
		})
	}
	return rows
}

// commitRowsFromPreview converts a clean preview into commit rows, carrying the
// current URLs the operator saw so the commit can detect changes since preview.
func commitRowsFromPreview(t *testing.T, p *BatchLinkImportPreview) []BatchLinkImportCommitRow {
	t.Helper()
	rows := make([]BatchLinkImportCommitRow, 0, len(p.Rows))
	for _, r := range p.Rows {
		if r.Severity == BatchLinkRowError {
			t.Fatalf("fixture preview must be clean, got error row %+v", r)
		}
		rows = append(rows, BatchLinkImportCommitRow{
			BatchID: r.BatchID, BatchCode: r.BatchCode,
			PrintURL: r.NewPrintURL, CutURL: r.NewCutURL,
			ExpectedPrintURL: r.CurrentPrintURL, ExpectedCutURL: r.CurrentCutURL,
		})
	}
	return rows
}

// TestExportBatchLinks_ScopeAndRoundTrip: the export holds exactly the batches
// a designer can still work on — PENDING, not closed, item-holding (flat or
// child, never the parent) — one row each with the immutable Batch ID, the
// human code, and the current links; and the file parses back through the
// import parser (same template version).
func TestExportBatchLinks_ScopeAndRoundTrip(t *testing.T) {
	db, svc, batches := newExcelFixture(t)
	// Give batch 1 an existing PRINT link — the export must show it.
	if _, err := svc.SetBatchLink(Actor{ID: 1}, batches[0].ID, SetBatchLinkInput{Kind: "PRINT", URL: "https://files/print-old"}); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	// Out-of-scope rows: a PRINTED batch and a closed batch.
	mat := &models.Material{Code: "SCOPE", Name: "Scope"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	now := time.Now()
	outOfScope := []*models.Batch{
		{Code: "#S-1", MaterialID: mat.ID, Status: models.StatusPrinted},
		{Code: "#S-2", MaterialID: mat.ID, Status: models.StatusPending, ClosedAt: &now},
	}
	for _, b := range outOfScope {
		if err := db.Create(b).Error; err != nil {
			t.Fatalf("seed out-of-scope batch: %v", err)
		}
	}

	data, filename, err := svc.ExportBatchLinksXLSX()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.HasSuffix(filename, ".xlsx") {
		t.Fatalf("filename: %q", filename)
	}

	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open export: %v", err)
	}
	defer f.Close()
	records, err := f.GetRows(f.GetSheetList()[0])
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if len(records) != 3 { // header + the two pending batches
		t.Fatalf("want header + 2 rows, got %d rows", len(records))
	}
	// Every data row carries the template version and both identifiers.
	for _, rec := range records[1:] {
		if rec[0] != BatchLinkTemplateVersion {
			t.Fatalf("row missing template version: %v", rec)
		}
	}
	if records[1][1] != fmt.Sprint(batches[0].ID) || records[1][2] != batches[0].Code {
		t.Fatalf("row 1 identifiers: got %v, want id=%d code=%s", records[1][:3], batches[0].ID, batches[0].Code)
	}
	joined := strings.Join(records[1], "|")
	if !strings.Contains(joined, "https://files/print-old") {
		t.Fatalf("existing PRINT link must be exported: %v", records[1])
	}

	// Round-trip: the exported file is a valid import file.
	rows, err := ParseBatchLinkImportXLSX(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse exported file: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 parsed rows, got %d", len(rows))
	}
	if rows[0].BatchID != batches[0].ID || rows[0].BatchCode != batches[0].Code || rows[0].Version != BatchLinkTemplateVersion {
		t.Fatalf("parsed row 1: %+v", rows[0])
	}
	if rows[0].PrintURL != "https://files/print-old" {
		t.Fatalf("parsed row 1 must keep the current print link, got %q", rows[0].PrintURL)
	}
}

// TestParseBatchLinkImport_CSVAliasesAndBlankLines: the CSV parser accepts the
// documented header aliases, skips fully blank lines, and keeps 1-based data
// row numbers for error messages.
func TestParseBatchLinkImport_CSVAliasesAndBlankLines(t *testing.T) {
	csv := "Template,Batch ID,Batch Code,Print URL,Cut URL\n" +
		BatchLinkTemplateVersion + ",12,#101012,https://f/p,https://f/c\n" +
		",,,,\n" +
		BatchLinkTemplateVersion + ",13,#101013,https://f/p2,https://f/c2\n"
	rows, err := ParseBatchLinkImportCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("blank line must be skipped: got %d rows", len(rows))
	}
	if rows[0].BatchID != 12 || rows[0].BatchCode != "#101012" || rows[1].Row != 3 {
		t.Fatalf("rows: %+v", rows)
	}
}

// TestParseBatchLinkImport_MissingColumnNamed: a file without a required column
// is refused with the missing column named — not silently mis-mapped.
func TestParseBatchLinkImport_MissingColumnNamed(t *testing.T) {
	csv := "Phiên bản mẫu,Batch ID,Mã batch,Link in\n" + // no cut column
		BatchLinkTemplateVersion + ",12,#101012,https://f/p\n"
	_, err := ParseBatchLinkImportCSV(strings.NewReader(csv))
	if err == nil || !strings.Contains(err.Error(), "Link cắt") {
		t.Fatalf("missing column must be named, got %v", err)
	}
}

// TestPreviewBatchLinkImport_MatchesByIdNotRowOrder: rows arrive shuffled and
// still land on the right batches — matching is by immutable Batch ID, never by
// position.
func TestPreviewBatchLinkImport_MatchesByIdNotRowOrder(t *testing.T) {
	_, svc, batches := newExcelFixture(t)
	rows := importRowsFor([]*models.Batch{batches[1], batches[0]}, "https://f/p", "https://f/c")

	p, err := svc.PreviewBatchLinkImport(Actor{ID: 1}, rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p.Summary.Errors != 0 || p.Summary.Assign != 2 {
		t.Fatalf("summary: %+v", p.Summary)
	}
	if p.Rows[0].BatchID != batches[1].ID || p.Rows[1].BatchID != batches[0].ID {
		t.Fatalf("rows must keep file order but match by id: %+v", p.Rows)
	}
	if !p.CanCommit {
		t.Fatalf("clean preview must be committable")
	}
}

// TestPreviewBatchLinkImport_Blockers: every tampered or invalid row is a
// blocking error, and one blocker pins CanCommit to false for the WHOLE file —
// no silent partial import.
func TestPreviewBatchLinkImport_Blockers(t *testing.T) {
	db, svc, batches := newExcelFixture(t)
	good := importRowsFor(batches[:1], "https://f/p", "https://f/c")[0]

	mat := &models.Material{Code: "BLK", Name: "Blk"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	now := time.Now()
	printed := &models.Batch{Code: "#K-1", MaterialID: mat.ID, Status: models.StatusPrinted}
	closed := &models.Batch{Code: "#K-2", MaterialID: mat.ID, Status: models.StatusPending, ClosedAt: &now}
	for _, b := range []*models.Batch{printed, closed} {
		if err := db.Create(b).Error; err != nil {
			t.Fatalf("seed batch: %v", err)
		}
	}

	mk := func(mut func(*BatchLinkImportRow)) BatchLinkImportRow {
		r := good
		mut(&r)
		return r
	}
	cases := []struct {
		name string
		row  BatchLinkImportRow
	}{
		{"wrong template version", mk(func(r *BatchLinkImportRow) { r.Version = "batch-links-v0" })},
		{"unknown batch id", mk(func(r *BatchLinkImportRow) { r.BatchID = 999999 })},
		{"id/code mismatch", mk(func(r *BatchLinkImportRow) { r.BatchID = batches[1].ID })}, // code still batches[0]'s
		{"missing print", mk(func(r *BatchLinkImportRow) { r.PrintURL = "" })},
		{"missing cut", mk(func(r *BatchLinkImportRow) { r.CutURL = "  " })},
		{"invalid url", mk(func(r *BatchLinkImportRow) { r.CutURL = "javascript:alert(1)" })},
		{"printed batch", mk(func(r *BatchLinkImportRow) { r.BatchID, r.BatchCode = printed.ID, printed.Code })},
		{"closed batch", mk(func(r *BatchLinkImportRow) { r.BatchID, r.BatchCode = closed.ID, closed.Code })},
	}
	for _, tc := range cases {
		p, err := svc.PreviewBatchLinkImport(Actor{ID: 1}, []BatchLinkImportRow{tc.row})
		if err != nil {
			t.Fatalf("%s: preview should report per-row, got %v", tc.name, err)
		}
		if p.Summary.Errors != 1 || p.Rows[0].Severity != BatchLinkRowError {
			t.Fatalf("%s: want a blocking error, got %+v", tc.name, p.Rows[0])
		}
		if p.CanCommit {
			t.Fatalf("%s: a blocker must pin CanCommit=false", tc.name)
		}
	}

	// Duplicate batch in one file blocks BOTH rows.
	p, err := svc.PreviewBatchLinkImport(Actor{ID: 1}, []BatchLinkImportRow{good, good})
	if err != nil {
		t.Fatalf("duplicate preview: %v", err)
	}
	if p.Summary.Errors != 2 || p.CanCommit {
		t.Fatalf("duplicate batch rows must both block: %+v", p.Summary)
	}
}

// TestPreviewBatchLinkImport_ActionsAndNoWrites: assigning, replacing (a
// warning that needs opt-in + reason at commit) and unchanged rows are told
// apart — and preview writes NOTHING.
func TestPreviewBatchLinkImport_ActionsAndNoWrites(t *testing.T) {
	db, svc, batches := newExcelFixture(t)
	if _, err := svc.SetBatchLinkPair(Actor{ID: 1}, batches[0].ID, SetBatchLinkPairInput{
		PrintURL: "https://f/print-cur", CutURL: "https://f/cut-cur",
	}); err != nil {
		t.Fatalf("seed pair: %v", err)
	}
	var auditsBefore, linksBefore int64
	db.Model(&models.AuditLog{}).Count(&auditsBefore)
	db.Model(&models.BatchLink{}).Count(&linksBefore)

	rows := []BatchLinkImportRow{
		{Row: 1, Version: BatchLinkTemplateVersion, BatchID: batches[0].ID, BatchCode: batches[0].Code,
			PrintURL: "https://f/print-cur", CutURL: "https://f/cut-cur"}, // UNCHANGED
		{Row: 2, Version: BatchLinkTemplateVersion, BatchID: batches[1].ID, BatchCode: batches[1].Code,
			PrintURL: "https://f/p2", CutURL: "https://f/c2"}, // ASSIGN
	}
	p, err := svc.PreviewBatchLinkImport(Actor{ID: 1}, rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p.Summary.Unchanged != 1 || p.Summary.Assign != 1 || p.Summary.Errors != 0 {
		t.Fatalf("summary: %+v", p.Summary)
	}
	if p.Rows[0].Action != BatchLinkActionUnchanged || p.Rows[1].Action != BatchLinkActionAssign {
		t.Fatalf("actions: %+v", p.Rows)
	}

	// REPLACE shows the current pair next to the incoming one.
	p2, err := svc.PreviewBatchLinkImport(Actor{ID: 1}, []BatchLinkImportRow{
		{Row: 1, Version: BatchLinkTemplateVersion, BatchID: batches[0].ID, BatchCode: batches[0].Code,
			PrintURL: "https://f/print-NEW", CutURL: "https://f/cut-cur"},
	})
	if err != nil {
		t.Fatalf("replace preview: %v", err)
	}
	r := p2.Rows[0]
	if r.Action != BatchLinkActionReplace || r.Severity != BatchLinkRowWarning {
		t.Fatalf("replace row: %+v", r)
	}
	if r.CurrentPrintURL != "https://f/print-cur" || r.NewPrintURL != "https://f/print-NEW" {
		t.Fatalf("replace row must show current vs incoming: %+v", r)
	}

	var auditsAfter, linksAfter int64
	db.Model(&models.AuditLog{}).Count(&auditsAfter)
	db.Model(&models.BatchLink{}).Count(&linksAfter)
	if auditsAfter != auditsBefore || linksAfter != linksBefore {
		t.Fatalf("preview must write nothing (audits %d→%d, links %d→%d)", auditsBefore, auditsAfter, linksBefore, linksAfter)
	}
}

// TestCommitBatchLinkImport_AppliesAtomically: a clean commit updates every
// batch's pair, fans out to the items, and audits both per batch and once for
// the whole import.
func TestCommitBatchLinkImport_AppliesAtomically(t *testing.T) {
	db, svc, batches := newExcelFixture(t)
	rows := importRowsFor(batches, "https://f/p", "https://f/c")
	p, err := svc.PreviewBatchLinkImport(Actor{ID: 1}, rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	res, err := svc.CommitBatchLinkImport(Actor{ID: 9, Email: "ops@x"}, BatchLinkImportCommitInput{
		SourceFilename: "batch-links.xlsx",
		Rows:           commitRowsFromPreview(t, p),
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Updated != 2 || res.Unchanged != 0 {
		t.Fatalf("result: %+v", res)
	}
	if len(res.UpdatedBatchCodes) != 2 {
		t.Fatalf("updated codes: %+v", res.UpdatedBatchCodes)
	}
	for _, b := range batches {
		var links []models.BatchLink
		if err := db.Where("batch_id = ?", b.ID).Find(&links).Error; err != nil || len(links) != 2 {
			t.Fatalf("batch %s: want both links, got %d (%v)", b.Code, len(links), err)
		}
	}
	// Fan-out reached the items of both batches.
	var items []models.OrderItem
	if err := db.Find(&items).Error; err != nil {
		t.Fatalf("load items: %v", err)
	}
	for _, it := range items {
		if it.PrintFileURL == "" || it.CutFileURL == "" {
			t.Fatalf("item %d missing fan-out: %q/%q", it.ID, it.PrintFileURL, it.CutFileURL)
		}
	}
	if got := pairAuditCount(t, db); got != 2 {
		t.Fatalf("want one package audit per batch, got %d", got)
	}
	var summary int64
	db.Model(&models.AuditLog{}).Where("action = ?", "BATCH_LINK_IMPORT_COMMIT").Count(&summary)
	if summary != 1 {
		t.Fatalf("want one import summary audit, got %d", summary)
	}
}

// TestCommitBatchLinkImport_StateChangeAbortsWholeImport: a batch that left
// PENDING between preview and commit invalidates the WHOLE import — the other
// rows roll back too, per the all-or-nothing contract.
func TestCommitBatchLinkImport_StateChangeAbortsWholeImport(t *testing.T) {
	db, svc, batches := newExcelFixture(t)
	rows := importRowsFor(batches, "https://f/p", "https://f/c")
	p, err := svc.PreviewBatchLinkImport(Actor{ID: 1}, rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	commitRows := commitRowsFromPreview(t, p)

	// Batch 2 starts production after the preview.
	seedBatchLinks(t, db, batches[1].ID)
	if _, err := svc.UpdateStatus(Actor{ID: 1, Role: models.RoleProduction}, batches[1].ID,
		UpdateStatusInput{Status: string(models.StatusPrinted)}); err != nil {
		t.Fatalf("advance batch 2: %v", err)
	}

	_, err = svc.CommitBatchLinkImport(Actor{ID: 9}, BatchLinkImportCommitInput{
		SourceFilename: "batch-links.xlsx", Rows: commitRows,
	})
	if err == nil {
		t.Fatalf("commit must abort when a batch changed after preview")
	}
	// Batch 1 (still valid on its own) must NOT have been updated.
	if got := batchLinkCount(t, db, batches[0].ID); got != 0 {
		t.Fatalf("all-or-nothing: batch 1 must roll back too, found %d links", got)
	}
	if got := pairAuditCount(t, db); got != 0 {
		t.Fatalf("aborted import must leave no package audit, got %d", got)
	}
}

// TestCommitBatchLinkImport_LinkChangeSincePreviewAborts: someone attached a
// link by hand after the preview was taken — the stale import must not
// silently overwrite it.
func TestCommitBatchLinkImport_LinkChangeSincePreviewAborts(t *testing.T) {
	db, svc, batches := newExcelFixture(t)
	rows := importRowsFor(batches[:1], "https://f/p", "https://f/c")
	p, err := svc.PreviewBatchLinkImport(Actor{ID: 1}, rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	commitRows := commitRowsFromPreview(t, p)

	// A manual PRINT link lands after the preview.
	if _, err := svc.SetBatchLink(Actor{ID: 2}, batches[0].ID, SetBatchLinkInput{Kind: "PRINT", URL: "https://f/manual"}); err != nil {
		t.Fatalf("manual link: %v", err)
	}

	_, err = svc.CommitBatchLinkImport(Actor{ID: 9}, BatchLinkImportCommitInput{
		SourceFilename: "batch-links.xlsx", Rows: commitRows,
	})
	if err == nil {
		t.Fatalf("commit must abort instead of overwriting a link added after preview")
	}
	var link models.BatchLink
	if err := db.Where("batch_id = ? AND kind = ?", batches[0].ID, models.BatchLinkPrint).First(&link).Error; err != nil {
		t.Fatalf("manual link vanished: %v", err)
	}
	if link.URL != "https://f/manual" {
		t.Fatalf("manual link must survive, got %q", link.URL)
	}
}

// TestCommitBatchLinkImport_ReplaceNeedsReason: an import that replaces an
// existing pair carries a reason, exactly like the manual path.
func TestCommitBatchLinkImport_ReplaceNeedsReason(t *testing.T) {
	_, svc, batches := newExcelFixture(t)
	if _, err := svc.SetBatchLinkPair(Actor{ID: 1}, batches[0].ID, SetBatchLinkPairInput{
		PrintURL: "https://f/print-cur", CutURL: "https://f/cut-cur",
	}); err != nil {
		t.Fatalf("seed pair: %v", err)
	}
	rows := []BatchLinkImportRow{{
		Row: 1, Version: BatchLinkTemplateVersion, BatchID: batches[0].ID, BatchCode: batches[0].Code,
		PrintURL: "https://f/print-NEW", CutURL: "https://f/cut-cur",
	}}
	p, err := svc.PreviewBatchLinkImport(Actor{ID: 1}, rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	commitRows := commitRowsFromPreview(t, p)

	if _, err := svc.CommitBatchLinkImport(Actor{ID: 9}, BatchLinkImportCommitInput{
		SourceFilename: "batch-links.xlsx", Rows: commitRows,
	}); err == nil {
		t.Fatalf("replacing via import without a reason must be refused")
	}
	res, err := svc.CommitBatchLinkImport(Actor{ID: 9}, BatchLinkImportCommitInput{
		SourceFilename: "batch-links.xlsx", Rows: commitRows, Reason: "file cũ sai khổ in",
	})
	if err != nil {
		t.Fatalf("commit with reason: %v", err)
	}
	if res.Updated != 1 {
		t.Fatalf("result: %+v", res)
	}
}

// TestCommitBatchLinkImport_ReuploadSameDataIsNoOp: re-uploading the exported
// file with unchanged data updates nothing and fabricates no history.
func TestCommitBatchLinkImport_ReuploadSameDataIsNoOp(t *testing.T) {
	db, svc, batches := newExcelFixture(t)
	rows := importRowsFor(batches, "https://f/p", "https://f/c")
	p, err := svc.PreviewBatchLinkImport(Actor{ID: 1}, rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := svc.CommitBatchLinkImport(Actor{ID: 9}, BatchLinkImportCommitInput{
		SourceFilename: "batch-links.xlsx", Rows: commitRowsFromPreview(t, p),
	}); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	auditsAfterFirst := pairAuditCount(t, db)

	// Same data again — preview shows everything unchanged, commit is a no-op.
	p2, err := svc.PreviewBatchLinkImport(Actor{ID: 1}, rows)
	if err != nil {
		t.Fatalf("second preview: %v", err)
	}
	if p2.Summary.Unchanged != 2 || p2.Summary.Errors != 0 {
		t.Fatalf("second preview: %+v", p2.Summary)
	}
	res, err := svc.CommitBatchLinkImport(Actor{ID: 9}, BatchLinkImportCommitInput{
		SourceFilename: "batch-links.xlsx", Rows: commitRowsFromPreview(t, p2),
	})
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if res.Updated != 0 || res.Unchanged != 2 {
		t.Fatalf("no-op result: %+v", res)
	}
	if got := pairAuditCount(t, db); got != auditsAfterFirst {
		t.Fatalf("no-op must not add package audits: %d → %d", auditsAfterFirst, got)
	}
}

// TestCommitBatchLinkImport_DuplicateRowsRejected: two rows for one batch in a
// commit request are refused outright.
func TestCommitBatchLinkImport_DuplicateRowsRejected(t *testing.T) {
	_, svc, batches := newExcelFixture(t)
	row := BatchLinkImportCommitRow{
		BatchID: batches[0].ID, BatchCode: batches[0].Code,
		PrintURL: "https://f/p", CutURL: "https://f/c",
	}
	if _, err := svc.CommitBatchLinkImport(Actor{ID: 9}, BatchLinkImportCommitInput{
		SourceFilename: "x.xlsx", Rows: []BatchLinkImportCommitRow{row, row},
	}); err == nil {
		t.Fatalf("duplicate rows for one batch must be rejected")
	}
}

// TestCommitBatchLinkImport_CodeMismatchRejected: a commit whose code does not
// belong to the Batch ID (tampered file / stale identifiers) is refused and
// nothing lands.
func TestCommitBatchLinkImport_CodeMismatchRejected(t *testing.T) {
	db, svc, batches := newExcelFixture(t)
	if _, err := svc.CommitBatchLinkImport(Actor{ID: 9}, BatchLinkImportCommitInput{
		SourceFilename: "x.xlsx",
		Rows: []BatchLinkImportCommitRow{{
			BatchID: batches[0].ID, BatchCode: batches[1].Code,
			PrintURL: "https://f/p", CutURL: "https://f/c",
		}},
	}); err == nil {
		t.Fatalf("id/code mismatch must be rejected at commit too")
	}
	if got := batchLinkCount(t, db, batches[0].ID); got != 0 {
		t.Fatalf("nothing may land on mismatch, got %d links", got)
	}
}
