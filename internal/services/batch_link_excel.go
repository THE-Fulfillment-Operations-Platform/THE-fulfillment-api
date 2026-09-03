package services

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Excel/Sheet là bàn làm việc trung gian của designer: hệ thống xuất một file
// trong đó MỖI DÒNG là MỘT BATCH đã tồn tại (kèm Batch ID bất biến + mã batch
// dễ đọc + link hiện tại), designer điền link in/cắt vào đúng dòng rồi upload
// lại. Đối chiếu khi nhập CHỈ theo Batch ID (kiểm tra thêm mã batch) — không
// bao giờ theo vị trí dòng, nên designer sắp xếp lại file thoải mái.

// BatchLinkTemplateVersion stamps every data row of the exported file. A file
// built from an older template carries the wrong value (or none) and is
// refused row-by-row, so a stale sheet can never slip through a re-upload.
const BatchLinkTemplateVersion = "batch-links-v1"

// MaxBatchLinkImportRows caps one uploaded file, mirroring the tracking import.
const MaxBatchLinkImportRows = 1000

// Preview row severities.
const (
	BatchLinkRowOK      = "OK"
	BatchLinkRowWarning = "WARNING"
	BatchLinkRowError   = "ERROR"
)

// Preview issue codes (stable identifiers the client groups by).
const (
	BatchLinkIssueBadTemplate    = "BAD_TEMPLATE"
	BatchLinkIssueMissingBatchID = "MISSING_BATCH_ID"
	BatchLinkIssueMissingCode    = "MISSING_CODE"
	BatchLinkIssueNotFound       = "NOT_FOUND"
	BatchLinkIssueCodeMismatch   = "CODE_MISMATCH"
	BatchLinkIssueParentBatch    = "PARENT_BATCH"
	BatchLinkIssueClosed         = "CLOSED"
	BatchLinkIssueStarted        = "ALREADY_STARTED"
	BatchLinkIssueNoItems        = "NO_ITEMS"
	BatchLinkIssueDuplicate      = "DUPLICATE_BATCH"
	BatchLinkIssueMissingPrint   = "MISSING_PRINT"
	BatchLinkIssueMissingCut     = "MISSING_CUT"
	BatchLinkIssueInvalidURL     = "INVALID_URL"
	BatchLinkIssueReplace        = "REPLACE"
)

// batchLinkExportHeaders is the exact header row of the exported workbook. The
// import parser recognises these (plus a few aliases), so export → fill →
// import round-trips without anyone touching the header.
var batchLinkExportHeaders = []string{
	"Phiên bản mẫu", "Batch ID", "Mã batch", "Chất liệu",
	"Số item", "Số sản phẩm", "Link design", "Link in", "Link cắt",
}

// normalizeBatchCode trims a human-entered batch code and restores the leading
// "#" every generated code carries — Excel loves eating that character. This is
// a display normalisation only; identity always comes from Batch ID.
func normalizeBatchCode(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && !strings.HasPrefix(s, "#") {
		s = "#" + s
	}
	return s
}

// ---------- export ----------

// ExportBatchLinksXLSX renders the designer worksheet: one row per batch that
// can still take a production package (PENDING, open, item-holding — parents
// are excluded, children appear individually). Current links are included so a
// re-upload of an untouched file is a clean no-op.
func (s *BatchService) ExportBatchLinksXLSX() ([]byte, string, error) {
	batches, err := s.repo.Batch.PendingLinkTargets()
	if err != nil {
		return nil, "", apperr.Internal("could not list pending batches").Wrap(err)
	}
	ids := make([]uint, 0, len(batches))
	for i := range batches {
		ids = append(ids, batches[i].ID)
	}
	counts, err := s.repo.Batch.LiveItemProductCounts(ids)
	if err != nil {
		return nil, "", apperr.Internal("could not count batch items").Wrap(err)
	}

	grid := [][]string{batchLinkExportHeaders}
	for i := range batches {
		b := &batches[i]
		printURL, cutURL := batchProductionLinks(b)
		materialName := b.Material.Name
		if materialName == "" {
			materialName = b.Material.Code
		}
		c := counts[b.ID]
		grid = append(grid, []string{
			BatchLinkTemplateVersion,
			strconv.FormatUint(uint64(b.ID), 10),
			b.Code,
			materialName,
			strconv.Itoa(c.Items),
			strconv.Itoa(c.Products),
			s.batchDetailURL(b.ID),
			printURL,
			cutURL,
		})
	}
	data, err := buildTemplateXLSX("Batch links", grid, []float64{14, 10, 12, 18, 8, 10, 44, 44, 44})
	if err != nil {
		return nil, "", err
	}
	return data, "batch-production-links.xlsx", nil
}

// batchDetailURL points the "Link design" column at the batch's detail page,
// where the existing design-ZIP download lives — the sheet reuses that flow
// instead of inventing a second way to hand out design files.
func (s *BatchService) batchDetailURL(id uint) string {
	base := strings.TrimRight(s.appBaseURL, "/")
	return fmt.Sprintf("%s/batches/%d", base, id)
}

// ---------- parsing ----------

// BatchLinkImportRow is one data line of the uploaded sheet.
type BatchLinkImportRow struct {
	Row        int    `json:"row"`
	Version    string `json:"version"`
	BatchID    uint   `json:"batch_id"`
	BatchIDRaw string `json:"batch_id_raw,omitempty"`
	BatchCode  string `json:"batch_code"`
	PrintURL   string `json:"print_url"`
	CutURL     string `json:"cut_url"`
}

// Header aliases, matched after normalizeTrackingHeader (NFC, lowercase,
// letters+digits only) — the same treatment the tracking import applies.
var (
	batchLinkVersionHeaderKeys = map[string]bool{
		"phiênbảnmẫu": true, "phienbanmau": true, "phiênbản": true, "phienban": true,
		"template": true, "templateversion": true, "version": true,
	}
	batchLinkIDHeaderKeys = map[string]bool{
		"batchid": true,
	}
	batchLinkCodeHeaderKeys = map[string]bool{
		"mãbatch": true, "mabatch": true, "batchcode": true, "batch": true,
	}
	batchLinkPrintHeaderKeys = map[string]bool{
		"linkin": true, "printurl": true, "printlink": true, "print": true,
	}
	batchLinkCutHeaderKeys = map[string]bool{
		"linkcắt": true, "linkcat": true, "cuturl": true, "cutlink": true, "cut": true,
	}
)

// ParseBatchLinkImportCSV reads a CSV stream into import rows.
func ParseBatchLinkImportCSV(r io.Reader) ([]BatchLinkImportRow, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, apperr.BadRequest("Không đọc được file CSV: " + err.Error())
	}
	return batchLinkRowsFromRecords(records)
}

// ParseBatchLinkImportXLSX reads the first worksheet of an .xlsx/.xlsm stream.
func ParseBatchLinkImportXLSX(r io.Reader) ([]BatchLinkImportRow, error) {
	f, err := excelize.OpenReader(r)
	if err != nil {
		return nil, apperr.BadRequest("Không đọc được file Excel: " + err.Error())
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, apperr.BadRequest("File Excel không có sheet nào")
	}
	records, err := f.GetRows(sheets[0])
	if err != nil {
		return nil, apperr.BadRequest("Không đọc được dữ liệu Excel: " + err.Error())
	}
	return batchLinkRowsFromRecords(records)
}

func batchLinkRowsFromRecords(records [][]string) ([]BatchLinkImportRow, error) {
	if len(records) < 2 {
		return nil, apperr.BadRequest("File phải có dòng tiêu đề và ít nhất một dòng dữ liệu")
	}
	verCol, idCol, codeCol, printCol, cutCol := -1, -1, -1, -1, -1
	for i, h := range records[0] {
		key := normalizeTrackingHeader(h)
		switch {
		case batchLinkVersionHeaderKeys[key] && verCol == -1:
			verCol = i
		case batchLinkIDHeaderKeys[key] && idCol == -1:
			idCol = i
		case batchLinkCodeHeaderKeys[key] && codeCol == -1:
			codeCol = i
		case batchLinkPrintHeaderKeys[key] && printCol == -1:
			printCol = i
		case batchLinkCutHeaderKeys[key] && cutCol == -1:
			cutCol = i
		}
	}
	var missing []string
	for _, col := range []struct {
		idx  int
		name string
	}{
		{verCol, "Phiên bản mẫu"}, {idCol, "Batch ID"}, {codeCol, "Mã batch"},
		{printCol, "Link in"}, {cutCol, "Link cắt"},
	} {
		if col.idx == -1 {
			missing = append(missing, col.name)
		}
	}
	if len(missing) > 0 {
		return nil, apperr.BadRequest(
			"File thiếu cột bắt buộc: " + strings.Join(missing, ", ") + " — tải lại file từ hệ thống để có đúng mẫu")
	}
	cell := func(rec []string, col int) string {
		if col < len(rec) {
			return strings.TrimSpace(rec[col])
		}
		return ""
	}
	rows := make([]BatchLinkImportRow, 0, len(records)-1)
	for i, rec := range records[1:] {
		version, rawID := cell(rec, verCol), cell(rec, idCol)
		code, printURL, cutURL := cell(rec, codeCol), cell(rec, printCol), cell(rec, cutCol)
		if version == "" && rawID == "" && code == "" && printURL == "" && cutURL == "" {
			continue // fully blank line — Excel files end with plenty of them
		}
		row := BatchLinkImportRow{
			Row: i + 1, Version: version, BatchIDRaw: rawID, BatchCode: code,
			PrintURL: printURL, CutURL: cutURL,
		}
		if id, err := strconv.ParseUint(rawID, 10, 64); err == nil {
			row.BatchID = uint(id)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, apperr.BadRequest("File không có dòng dữ liệu nào")
	}
	if len(rows) > MaxBatchLinkImportRows {
		return nil, apperr.BadRequest(fmt.Sprintf("File quá lớn: tối đa %d dòng mỗi lần", MaxBatchLinkImportRows))
	}
	return rows, nil
}

// ---------- preview ----------

// BatchLinkImportPreviewRow shows the operator one file line next to the batch
// it resolves to: what the batch has now, what the file wants to write, and
// whether the line may be applied.
type BatchLinkImportPreviewRow struct {
	Row             int                   `json:"row"`
	BatchID         uint                  `json:"batch_id"`
	BatchCode       string                `json:"batch_code"`
	Material        string                `json:"material"`
	Status          models.InternalStatus `json:"status,omitempty"`
	ItemCount       int                   `json:"item_count"`
	CurrentPrintURL string                `json:"current_print_url"`
	CurrentCutURL   string                `json:"current_cut_url"`
	NewPrintURL     string                `json:"new_print_url"`
	NewCutURL       string                `json:"new_cut_url"`
	Action          string                `json:"action,omitempty"`
	Severity        string                `json:"severity"`
	Code            string                `json:"code,omitempty"`
	Reason          string                `json:"reason,omitempty"`
}

// BatchLinkImportSummary is the preview's headline numbers.
type BatchLinkImportSummary struct {
	TotalRows int `json:"total_rows"`
	OK        int `json:"ok"`
	Warnings  int `json:"warnings"`
	Errors    int `json:"errors"`
	Assign    int `json:"assign"`
	Replace   int `json:"replace"`
	Unchanged int `json:"unchanged"`
}

// BatchLinkImportPreview is the full dry-run result. CanCommit is false the
// moment ANY row is a blocking error: the file is fixed and re-uploaded as a
// whole, never silently half-applied.
type BatchLinkImportPreview struct {
	Summary   BatchLinkImportSummary      `json:"summary"`
	Rows      []BatchLinkImportPreviewRow `json:"rows"`
	CanCommit bool                        `json:"can_commit"`
}

// PreviewBatchLinkImport resolves every file line against the batches WITHOUT
// writing anything. Lines match by immutable Batch ID (the human code is only
// cross-checked), so reordering rows in Excel changes nothing.
func (s *BatchService) PreviewBatchLinkImport(actor Actor, rows []BatchLinkImportRow) (*BatchLinkImportPreview, error) {
	if len(rows) > MaxBatchLinkImportRows {
		return nil, apperr.BadRequest(fmt.Sprintf("File quá lớn: tối đa %d dòng mỗi lần", MaxBatchLinkImportRows))
	}
	ids := make([]uint, 0, len(rows))
	seen := map[uint]int{}
	for _, r := range rows {
		if r.BatchID != 0 {
			ids = append(ids, r.BatchID)
			seen[r.BatchID]++
		}
	}
	refs, err := s.repo.Batch.LinkImportRefs(ids)
	if err != nil {
		return nil, apperr.Internal("could not resolve batches").Wrap(err)
	}
	counts, err := s.repo.Batch.LiveItemProductCounts(ids)
	if err != nil {
		return nil, apperr.Internal("could not count batch items").Wrap(err)
	}

	preview := &BatchLinkImportPreview{Rows: make([]BatchLinkImportPreviewRow, 0, len(rows))}
	preview.Summary.TotalRows = len(rows)
	for _, r := range rows {
		out := BatchLinkImportPreviewRow{
			Row: r.Row, BatchID: r.BatchID, BatchCode: normalizeBatchCode(r.BatchCode),
			NewPrintURL: strings.TrimSpace(r.PrintURL), NewCutURL: strings.TrimSpace(r.CutURL),
			Severity: BatchLinkRowOK,
		}
		fail := func(code, reason string) {
			out.Severity, out.Code, out.Reason = BatchLinkRowError, code, reason
		}
		switch {
		case r.Version != BatchLinkTemplateVersion:
			fail(BatchLinkIssueBadTemplate, "Dòng không đúng mẫu hiện hành ("+BatchLinkTemplateVersion+") — tải lại file từ hệ thống")
		case r.BatchID == 0:
			fail(BatchLinkIssueMissingBatchID, "Thiếu hoặc sai Batch ID — không tự đoán batch theo tên hay vị trí dòng")
		case out.BatchCode == "":
			fail(BatchLinkIssueMissingCode, "Thiếu mã batch để đối chiếu")
		case seen[r.BatchID] > 1:
			fail(BatchLinkIssueDuplicate, "Batch này xuất hiện ở nhiều dòng trong file — không biết dòng nào đúng")
		default:
			ref := refs[r.BatchID]
			if ref == nil {
				fail(BatchLinkIssueNotFound, "Không tìm thấy batch với Batch ID này")
				break
			}
			out.BatchCode = ref.Code
			out.Material = ref.Material.Name
			if out.Material == "" {
				out.Material = ref.Material.Code
			}
			out.Status = ref.Status
			out.ItemCount = counts[ref.ID].Items
			curPrint, curCut := batchProductionLinks(ref)
			out.CurrentPrintURL, out.CurrentCutURL = curPrint, curCut
			switch {
			case normalizeBatchCode(r.BatchCode) != ref.Code:
				fail(BatchLinkIssueCodeMismatch, fmt.Sprintf("Mã batch %q không khớp với Batch ID %d (%s) — dòng có thể đã bị sửa hoặc tráo", r.BatchCode, ref.ID, ref.Code))
			case ref.IsParent:
				fail(BatchLinkIssueParentBatch, "Đây là batch mẹ — link sản xuất gắn trên từng batch con")
			case ref.ClosedAt != nil:
				fail(BatchLinkIssueClosed, "Batch đã đóng/huỷ — không gắn file sản xuất nữa")
			case ref.Status != models.StatusPending:
				fail(BatchLinkIssueStarted, "Batch đã bắt đầu sản xuất — bộ file đã bị khóa")
			case counts[ref.ID].Items == 0:
				fail(BatchLinkIssueNoItems, "Batch không còn sản phẩm hợp lệ")
			case out.NewPrintURL == "":
				fail(BatchLinkIssueMissingPrint, "Thiếu Link in — phải điền đủ cả hai link")
			case out.NewCutURL == "":
				fail(BatchLinkIssueMissingCut, "Thiếu Link cắt — phải điền đủ cả hai link")
			default:
				if _, err := normalizeProductionURL("Link in", out.NewPrintURL); err != nil {
					fail(BatchLinkIssueInvalidURL, "Link in không hợp lệ (phải là http/https, tối đa 500 ký tự)")
					break
				}
				if _, err := normalizeProductionURL("Link cắt", out.NewCutURL); err != nil {
					fail(BatchLinkIssueInvalidURL, "Link cắt không hợp lệ (phải là http/https, tối đa 500 ký tự)")
					break
				}
				out.Action = classifyLinkPairAction(curPrint, curCut, out.NewPrintURL, out.NewCutURL)
				switch out.Action {
				case BatchLinkActionReplace:
					out.Severity = BatchLinkRowWarning
					out.Code = BatchLinkIssueReplace
					out.Reason = "Sẽ THAY link hiện có của batch — cần lý do khi xác nhận"
				case BatchLinkActionUnchanged:
					out.Reason = "Trùng dữ liệu hiện tại — sẽ bỏ qua, không tạo lịch sử"
				}
			}
		}
		switch out.Severity {
		case BatchLinkRowError:
			preview.Summary.Errors++
		case BatchLinkRowWarning:
			preview.Summary.Warnings++
		default:
			preview.Summary.OK++
		}
		switch out.Action {
		case BatchLinkActionAssign:
			preview.Summary.Assign++
		case BatchLinkActionReplace:
			preview.Summary.Replace++
		case BatchLinkActionUnchanged:
			preview.Summary.Unchanged++
		}
		preview.Rows = append(preview.Rows, out)
	}
	preview.CanCommit = preview.Summary.Errors == 0 && preview.Summary.TotalRows > 0
	return preview, nil
}

// ---------- commit ----------

// BatchLinkImportCommitRow is one confirmed line sent back after the preview.
// Expected* carry the current links the operator SAW — the commit re-reads the
// batch under lock and refuses to overwrite links that changed since.
type BatchLinkImportCommitRow struct {
	BatchID          uint   `json:"batch_id" binding:"required"`
	BatchCode        string `json:"batch_code" binding:"required"`
	PrintURL         string `json:"print_url" binding:"required"`
	CutURL           string `json:"cut_url" binding:"required"`
	ExpectedPrintURL string `json:"expected_print_url"`
	ExpectedCutURL   string `json:"expected_cut_url"`
}

// BatchLinkImportCommitInput confirms a previewed import. Reason is required
// when any row replaces an existing link.
type BatchLinkImportCommitInput struct {
	SourceFilename string                     `json:"source_filename"`
	Reason         string                     `json:"reason"`
	Rows           []BatchLinkImportCommitRow `json:"rows" binding:"required,min=1,dive"`
}

// BatchLinkImportCommitResult reports one committed import.
type BatchLinkImportCommitResult struct {
	Updated           int      `json:"updated"`
	Unchanged         int      `json:"unchanged"`
	UpdatedBatchCodes []string `json:"updated_batch_codes"`
}

// CommitBatchLinkImport applies a confirmed import in ONE transaction: every
// referenced batch is locked (in id order), re-verified against the same rules
// as the preview, compared against the links the operator saw, and updated
// through the shared pair core. Any failure — a batch that started production,
// a link someone attached meanwhile, a tampered identifier — aborts the WHOLE
// import: the operator re-previews instead of guessing which half applied.
func (s *BatchService) CommitBatchLinkImport(actor Actor, in BatchLinkImportCommitInput) (*BatchLinkImportCommitResult, error) {
	if len(in.Rows) > MaxBatchLinkImportRows {
		return nil, apperr.BadRequest(fmt.Sprintf("Tối đa %d dòng mỗi lần", MaxBatchLinkImportRows))
	}
	reason := strings.TrimSpace(in.Reason)
	if len(reason) > maxLinkReasonLen {
		return nil, apperr.BadRequest(fmt.Sprintf("Lý do quá dài (tối đa %d ký tự)", maxLinkReasonLen))
	}

	type checkedRow struct {
		batchID  uint
		code     string
		printURL string
		cutURL   string
		expPrint string
		expCut   string
	}
	rows := make([]checkedRow, 0, len(in.Rows))
	ids := make([]uint, 0, len(in.Rows))
	dupes := map[uint]bool{}
	for _, r := range in.Rows {
		if r.BatchID == 0 {
			return nil, apperr.BadRequest("Thiếu Batch ID trong yêu cầu xác nhận")
		}
		if dupes[r.BatchID] {
			return nil, apperr.BadRequest(fmt.Sprintf("Batch %s xuất hiện nhiều lần trong yêu cầu — mỗi batch chỉ một dòng", normalizeBatchCode(r.BatchCode)))
		}
		dupes[r.BatchID] = true
		printURL, err := normalizeProductionURL("Link in (batch "+normalizeBatchCode(r.BatchCode)+")", r.PrintURL)
		if err != nil {
			return nil, err
		}
		cutURL, err := normalizeProductionURL("Link cắt (batch "+normalizeBatchCode(r.BatchCode)+")", r.CutURL)
		if err != nil {
			return nil, err
		}
		rows = append(rows, checkedRow{
			batchID: r.BatchID, code: normalizeBatchCode(r.BatchCode),
			printURL: printURL, cutURL: cutURL,
			expPrint: strings.TrimSpace(r.ExpectedPrintURL), expCut: strings.TrimSpace(r.ExpectedCutURL),
		})
		ids = append(ids, r.BatchID)
	}

	type auditEntry struct {
		batchID uint
		code    string
		applied linkPairApplied
		print   string
		cut     string
	}
	res := &BatchLinkImportCommitResult{UpdatedBatchCodes: []string{}}
	audits := make([]auditEntry, 0, len(rows))
	now := time.Now()
	err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		// One lock pass over every batch, in id order — the same discipline every
		// production flow uses, so imports never deadlock against the QC/scrap paths.
		locked, err := txRepo.Batch.FindLiteManyForUpdate(ids)
		if err != nil {
			return apperr.Internal("could not lock batches").Wrap(err)
		}
		byID := map[uint]*models.Batch{}
		for i := range locked {
			byID[locked[i].ID] = &locked[i]
		}
		for _, r := range rows {
			batch := byID[r.batchID]
			if batch == nil {
				return apperr.Unprocessable(fmt.Sprintf("Batch ID %d không còn tồn tại — tải lại file và xem trước lại", r.batchID))
			}
			if r.code != batch.Code {
				return apperr.Unprocessable(fmt.Sprintf("Mã batch %q không khớp với Batch ID %d (%s) — file có thể đã bị sửa; xem trước lại", r.code, batch.ID, batch.Code))
			}
			// The links the operator saw at preview must still be the links on the
			// batch — otherwise the confirmation was given for a different state.
			curPrint, curCut := "", ""
			for _, kind := range []models.BatchLinkKind{models.BatchLinkPrint, models.BatchLinkCut} {
				link, err := txRepo.Batch.FindLink(batch.ID, kind)
				if err != nil {
					if !errors.Is(err, gorm.ErrRecordNotFound) {
						return apperr.Internal("could not look up batch link").Wrap(err)
					}
					continue
				}
				if kind == models.BatchLinkPrint {
					curPrint = link.URL
				} else {
					curCut = link.URL
				}
			}
			if curPrint != r.expPrint || curCut != r.expCut {
				return apperr.Unprocessable("Batch " + batch.Code + " đã thay đổi sau khi xem trước (link đã được cập nhật ở nơi khác) — toàn bộ lần nhập bị huỷ, hãy xem trước lại")
			}
			applied, err := applyBatchLinkPairTx(txRepo, actor, batch, r.printURL, r.cutURL, reason, now)
			if err != nil {
				return err
			}
			if applied.Action == BatchLinkActionUnchanged {
				res.Unchanged++
				continue
			}
			res.Updated++
			res.UpdatedBatchCodes = append(res.UpdatedBatchCodes, batch.Code)
			audits = append(audits, auditEntry{batchID: batch.ID, code: batch.Code, applied: *applied, print: r.printURL, cut: r.cutURL})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Audit only after the transaction landed — an aborted import never leaves
	// a trail of changes that did not happen. One entry per changed batch (the
	// same action the manual pair endpoint writes) plus one for the import.
	for _, a := range audits {
		id := a.batchID
		s.audit.Log(actor, "BATCH_PRODUCTION_PACKAGE_SET", "batch", &id,
			fmt.Sprintf("Gắn bộ file sản xuất cho batch %s từ file %s (%s)", a.code, in.SourceFilename, a.applied.Action),
			models.JSONMap{
				"batch_code":       a.code,
				"prev_print_url":   a.applied.PrevPrint,
				"prev_cut_url":     a.applied.PrevCut,
				"new_print_url":    a.print,
				"new_cut_url":      a.cut,
				"action":           a.applied.Action,
				"reason":           reason,
				"source":           "excel-import",
				"source_filename":  in.SourceFilename,
				"template_version": BatchLinkTemplateVersion,
			})
	}
	if res.Updated > 0 {
		s.audit.Log(actor, "BATCH_LINK_IMPORT_COMMIT", "batch", nil,
			fmt.Sprintf("Import link sản xuất từ %q: %d batch cập nhật, %d không đổi", in.SourceFilename, res.Updated, res.Unchanged),
			models.JSONMap{
				"source_filename":     in.SourceFilename,
				"template_version":    BatchLinkTemplateVersion,
				"updated_batch_codes": res.UpdatedBatchCodes,
				"updated":             res.Updated,
				"unchanged":           res.Unchanged,
				"reason":              reason,
			})
	}
	return res, nil
}
