package services

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/unicode/norm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Bulk tracking assignment from the CS Excel ("gắn mã vận đơn hàng loạt").
//
// The carrier hands CS a spreadsheet of (order id, tracking number) pairs; CS
// uploads it, the system matches each line against the orders and PREVIEWS the
// outcome — nothing is written until the operator has seen exactly what will
// happen and confirmed. The preview is deliberately conservative: any line the
// matcher is not certain about (unknown order, ambiguous store order id, two
// numbers for one order, one number on several orders, a number already used
// elsewhere) becomes an issue to resolve by hand instead of a guess.

// MaxTrackingImportRows caps one uploaded file (lines that carry a tracking
// number; blank lines of a journey export are skipped before counting). A CS
// day is a few hundred parcels, so the cap only stops runaway files. It is also
// the journey export's cap (MaxJourneyExportRows): a file the system hands out
// must always be accepted back — raised from 1000 when the export arrived.
const MaxTrackingImportRows = 5000

// MaxTrackingCommitBatch caps one commit call. Each assignment funnels through
// UpdateTracking (audit + provider registration per order), so the cap bounds
// how much third-party work a single request can queue.
const MaxTrackingCommitBatch = 500

// trackingImportScopeCap bounds the "in scope but missing from the file" list
// returned in the preview; the summary still carries the exact count.
const trackingImportScopeCap = 200

// TrackingImportRow is one data line of the uploaded file: an order identifier
// (store order id or our internal code — the matcher tries both) and the
// tracking number to attach. Row is 1-based over data rows, header excluded,
// matching the order-import error tables.
type TrackingImportRow struct {
	Row            int    `json:"row"`
	OrderKey       string `json:"order_key"`
	TrackingNumber string `json:"tracking_number"`
	// LabelURL: link to a label bought outside (https://file.gkelogistics.com/…
	// pdf) for the same order. Optional — a line may carry the tracking number,
	// the label link, or both (see ExternalLabelURL on the order).
	LabelURL string `json:"label_url,omitempty"`
}

// MaxExternalLabelURLLen mirrors orders.external_label_url (size:500).
const MaxExternalLabelURLLen = 500

// Match actions. UNCHANGED lines are shown but never committed; OVERWRITE lines
// are committed only when the operator explicitly opts in on the client.
const (
	TrackingActionAssign    = "ASSIGN"
	TrackingActionOverwrite = "OVERWRITE"
	TrackingActionUnchanged = "UNCHANGED"
	// TrackingActionNone: the line carries only a label link — nothing to do for
	// the tracking number (the label side has its own LabelAction).
	TrackingActionNone = "NONE"
)

// Issue codes. The client groups by these; Reason is the operator-facing text.
const (
	TrackingIssueEmptyOrder     = "EMPTY_ORDER"
	TrackingIssueEmptyTracking  = "EMPTY_TRACKING"
	TrackingIssueNotFound       = "NOT_FOUND"
	TrackingIssueAmbiguous      = "AMBIGUOUS"
	TrackingIssueDuplicate      = "DUPLICATE"
	TrackingIssueConflict       = "CONFLICT"
	TrackingIssueSharedTracking = "SHARED_TRACKING"
	TrackingIssueTakenTracking  = "TAKEN_TRACKING"
	// TrackingIssueExcelNumber: the cell holds a number Excel rewrote in
	// scientific notation ("9.40011189922346E+21"). A 22-digit USPS number typed
	// into a General-format cell becomes that, and the digits past the 15th are
	// gone for good — attaching it would register a parcel that does not exist.
	TrackingIssueExcelNumber = "EXCEL_NUMBER"
	// TrackingIssueBadLabel: the "Link label" cell is not a usable http(s) URL.
	TrackingIssueBadLabel = "BAD_LABEL"
	// TrackingIssueSharedLabel: one label link on several different orders.
	TrackingIssueSharedLabel = "SHARED_LABEL"
)

// TrackingImportMatch is a file line the matcher resolved to exactly one order.
type TrackingImportMatch struct {
	Row             int    `json:"row"`
	OrderID         uint   `json:"order_id"`
	InternalCode    string `json:"internal_code"`
	StoreOrderID    string `json:"store_order_id"`
	ShippingName    string `json:"shipping_name"`
	TrackingNumber  string `json:"tracking_number"`
	CurrentTracking string `json:"current_tracking,omitempty"`
	Action          string `json:"action"`
	// Label side (external label link). LabelAction is empty when the line has no
	// link; otherwise ASSIGN / OVERWRITE / UNCHANGED, like the tracking number.
	LabelURL     string `json:"label_url,omitempty"`
	CurrentLabel string `json:"current_label,omitempty"`
	LabelAction  string `json:"label_action,omitempty"`
}

// TrackingImportIssue is a file line that will NOT be applied, with the reason.
type TrackingImportIssue struct {
	Row            int    `json:"row"`
	OrderKey       string `json:"order_key"`
	TrackingNumber string `json:"tracking_number,omitempty"`
	LabelURL       string `json:"label_url,omitempty"`
	Code           string `json:"code"`
	Reason         string `json:"reason"`
}

// TrackingImportSummary is the preview's headline numbers. ScopeTotal /
// ScopeMissing describe the other direction of the comparison — orders the
// SYSTEM has (handed over in the requested date range, still without tracking)
// that the file does or does not cover. Both are -1 when no range was sent,
// meaning "not computed", which the client renders differently from 0.
type TrackingImportSummary struct {
	TotalRows int `json:"total_rows"`
	Assign    int `json:"assign"`
	Overwrite int `json:"overwrite"`
	Unchanged int `json:"unchanged"`
	// Label counters, over the lines that carry a link: ASSIGN = new label,
	// OVERWRITE = the order already has a different one, UNCHANGED = same link.
	LabelAssign    int `json:"label_assign"`
	LabelOverwrite int `json:"label_overwrite"`
	LabelUnchanged int `json:"label_unchanged"`
	// MergedRows: extra lines of an order already covered by an earlier line (a
	// label shared by several product lines) — folded in, not an issue.
	MergedRows   int   `json:"merged_rows"`
	Issues       int   `json:"issues"`
	ScopeTotal   int64 `json:"scope_total"`
	ScopeMissing int64 `json:"scope_missing"`
	// SkippedBlank counts the lines of a journey export (the page's own "Xuất
	// Excel" file) left without a tracking number — the operator filled only
	// some rows. They are not issues and are not in TotalRows. Always 0 for any
	// other file, where a blank number stays an EMPTY_TRACKING issue.
	SkippedBlank int `json:"skipped_blank"`
}

// TrackingImportPreview is the full dry-run result.
type TrackingImportPreview struct {
	Summary      TrackingImportSummary       `json:"summary"`
	Matches      []TrackingImportMatch       `json:"matches"`
	Issues       []TrackingImportIssue       `json:"issues"`
	ScopeMissing []repositories.OrderCodeRef `json:"scope_missing"`
}

// TrackingAssignment is one confirmed (order, number) pair sent back on commit.
type TrackingAssignment struct {
	OrderID uint `json:"order_id" binding:"required"`
	// TrackingNumber and LabelURL are each optional but at least one is needed.
	TrackingNumber string `json:"tracking_number"`
	LabelURL       string `json:"label_url"`
}

// TrackingImportCommitInput carries the assignments the operator confirmed —
// the client sends ASSIGN lines always and OVERWRITE lines only after the
// explicit opt-in, so the server never re-decides that policy.
type TrackingImportCommitInput struct {
	Assignments []TrackingAssignment `json:"assignments" binding:"required,min=1,dive"`
}

// TrackingCommitFailure names an assignment that could not be applied.
type TrackingCommitFailure struct {
	OrderID uint   `json:"order_id"`
	Reason  string `json:"reason"`
}

// TrackingImportCommitResult reports partial success: the batch never rolls
// back wholesale — 199 parcels must not stay unassigned because one order was
// deleted between preview and commit.
type TrackingImportCommitResult struct {
	Updated int                     `json:"updated"`
	Failed  []TrackingCommitFailure `json:"failed"`
}

// ---------- parsing ----------

// normalizeTrackingHeader flattens a header cell for alias lookup: NFC (macOS
// tools emit decomposed Vietnamese), lowercase, and every non-letter/digit
// removed — so "Mã vận đơn", "ma_van_don" and "Tracking Number" all hit the
// alias table regardless of punctuation.
func normalizeTrackingHeader(h string) string {
	h = strings.TrimPrefix(h, string(rune(0xFEFF)))
	h = norm.NFC.String(strings.ToLower(strings.TrimSpace(h)))
	var b strings.Builder
	for _, r := range h {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Header aliases. The order column accepts everything the journey screen
// displays — the shop's order id or our internal code — because the carrier
// file quotes whichever the CS pasted into the manifest. The internal-code
// aliases are a separate set: when a file has BOTH kinds of column (the journey
// export does), the internal code is the key — it is unique by construction,
// while a store order id may answer for several orders.
var (
	trackingOrderHeaderKeys = map[string]bool{
		"orderid": true, "order": true, "ordercode": true, "ordernumber": true,
		"mãđơn": true, "madon": true, "mãđơnhàng": true, "madonhang": true,
		"mãđơnshop": true, "madonshop": true, "storeorderid": true,
	}
	trackingInternalHeaderKeys = map[string]bool{
		"mãnộibộ": true, "manoibo": true, "internalcode": true,
	}
	// The label-link column: where the customer pastes the PDF link of a label
	// bought at an outside carrier.
	trackingLabelHeaderKeys = map[string]bool{
		"linklabel": true, "labelurl": true, "label": true, "labellink": true,
		"urllabel": true, "linkpdf": true, "pdflabel": true, "labelpdf": true,
		"linknhãn": true, "linknhan": true,
	}
	trackingNumberHeaderKeys = map[string]bool{
		"tracking": true, "trackingnumber": true, "trackingcode": true, "trackingid": true,
		"mãvậnđơn": true, "mavandon": true, "vậnđơn": true, "vandon": true,
		"mãtracking": true, "matracking": true, "mãvậnchuyển": true, "mavanchuyen": true,
	}
)

// TrackingImportFile is a parsed upload: the lines to match, plus what the
// parser deliberately left out.
type TrackingImportFile struct {
	Rows []TrackingImportRow
	// JourneyExport: the header carries the journey export's marker column, i.e.
	// this is the page's own "Xuất Excel" file coming back with numbers filled in.
	JourneyExport bool
	// SkippedBlank: journey-export lines whose tracking cell was left empty — the
	// operator filled only some of the exported orders. Not an error.
	SkippedBlank int
}

// ParseTrackingCSV reads a CSV stream into tracking-import rows.
func ParseTrackingCSV(r io.Reader) ([]TrackingImportRow, error) {
	file, err := ParseTrackingCSVFile(r)
	if err != nil {
		return nil, err
	}
	return file.Rows, nil
}

// ParseTrackingXLSX reads the first worksheet of an .xlsx/.xlsm stream.
func ParseTrackingXLSX(r io.Reader) ([]TrackingImportRow, error) {
	file, err := ParseTrackingXLSXFile(r)
	if err != nil {
		return nil, err
	}
	return file.Rows, nil
}

// ParseTrackingCSVFile is ParseTrackingCSV keeping the parse metadata.
func ParseTrackingCSVFile(r io.Reader) (*TrackingImportFile, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, apperr.BadRequest("Không đọc được file CSV: " + err.Error())
	}
	return trackingFileFromRecords(records)
}

// ParseTrackingXLSXFile is ParseTrackingXLSX keeping the parse metadata.
func ParseTrackingXLSXFile(r io.Reader) (*TrackingImportFile, error) {
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
	resolveLabelHyperlinks(f, sheets[0], records)
	return trackingFileFromRecords(records)
}

// resolveLabelHyperlinks: a link pasted into Excel is often shown as a word
// ("Label", "Xem") with the URL hidden behind the cell's hyperlink. GetRows only
// returns the shown text, so for the label column the real target is read back
// from the cell and put in its place.
func resolveLabelHyperlinks(f *excelize.File, sheet string, records [][]string) {
	if len(records) < 2 {
		return
	}
	labelCol := -1
	for i, h := range records[0] {
		if trackingLabelHeaderKeys[normalizeTrackingHeader(h)] {
			labelCol = i
			break
		}
	}
	if labelCol < 0 {
		return
	}
	for r := 1; r < len(records); r++ {
		rec := records[r]
		if labelCol >= len(rec) {
			continue
		}
		shown := strings.TrimSpace(rec[labelCol])
		if shown == "" || strings.HasPrefix(strings.ToLower(shown), "http") {
			continue
		}
		name, err := excelize.CoordinatesToCellName(labelCol+1, r+1)
		if err != nil {
			continue
		}
		if ok, target, err := f.GetCellHyperLink(sheet, name); err == nil && ok && target != "" {
			rec[labelCol] = target
		}
	}
}

func trackingFileFromRecords(records [][]string) (*TrackingImportFile, error) {
	if len(records) < 2 {
		return nil, apperr.BadRequest("File phải có dòng tiêu đề và ít nhất một dòng dữ liệu")
	}
	orderCol, internalCol, trackCol, labelCol := -1, -1, -1, -1
	journeyExport := false
	for i, h := range records[0] {
		key := normalizeTrackingHeader(h)
		if key == journeyExportMarkerKey {
			journeyExport = true
		}
		switch {
		case trackingInternalHeaderKeys[key] && internalCol == -1:
			internalCol = i
		case trackingOrderHeaderKeys[key] && orderCol == -1:
			orderCol = i
		case trackingNumberHeaderKeys[key] && trackCol == -1:
			trackCol = i
		case trackingLabelHeaderKeys[key] && labelCol == -1:
			labelCol = i
		}
	}
	// Key column: the internal code when present, else the generic order column.
	// The other one (if any) is the fallback for a line whose key cell is empty.
	keyCol, fallbackCol := orderCol, -1
	if internalCol != -1 {
		keyCol, fallbackCol = internalCol, orderCol
	}
	// The label link is an alternative payload: a file may carry tracking numbers,
	// label links, or both. At least one of the two columns must exist.
	if keyCol == -1 || (trackCol == -1 && labelCol == -1) {
		return nil, apperr.BadRequest(
			"Không nhận diện được cột OrderID và cột Mã vận đơn / Link label — tải file mẫu để xem đúng định dạng")
	}
	cell := func(rec []string, col int) string {
		if col >= 0 && col < len(rec) {
			return strings.TrimSpace(rec[col])
		}
		return ""
	}
	out := &TrackingImportFile{JourneyExport: journeyExport}
	rows := make([]TrackingImportRow, 0, len(records)-1)
	for i, rec := range records[1:] {
		key, number, label := cell(rec, keyCol), cell(rec, trackCol), cell(rec, labelCol)
		if key == "" {
			key = cell(rec, fallbackCol)
		}
		if key == "" && number == "" && label == "" {
			continue // fully blank line — Excel files end with plenty of them
		}
		if journeyExport && number == "" && label == "" {
			// An exported order the operator did not fill. The bulk path never
			// clears a number anyway, so skipping is exactly what an EMPTY_TRACKING
			// issue would have done — minus a red alert listing hundreds of rows.
			out.SkippedBlank++
			continue
		}
		rows = append(rows, TrackingImportRow{Row: i + 1, OrderKey: key, TrackingNumber: number, LabelURL: label})
	}
	if len(rows) == 0 {
		if out.SkippedBlank > 0 {
			return nil, apperr.BadRequest("Chưa dòng nào được điền mã vận đơn — điền cột \"Mã vận đơn\" rồi tải lên lại")
		}
		return nil, apperr.BadRequest("File không có dòng dữ liệu nào")
	}
	out.Rows = rows
	return out, nil
}

// excelScientificRe matches a number Excel displays in scientific notation —
// what GetRows (and a CSV saved by Excel) yields for a long digit string typed
// into a General-format cell, e.g. "9.40011189922346E+21". Deliberately narrow
// (one leading digit, explicit "E+"): Excel always writes the sign, and no
// carrier's number contains a '+', so a real tracking number cannot match.
var excelScientificRe = regexp.MustCompile(`^[0-9]([.,][0-9]+)?[eE]\+[0-9]+$`)

func looksLikeExcelScientific(v string) bool {
	return excelScientificRe.MatchString(strings.TrimSpace(v))
}

// normalizeExternalLabelURL checks the "Link label" cell: an http(s) URL with a
// host, no spaces or control characters, no credentials, at most 500 bytes (the
// column size). It returns the trimmed URL, or the operator-facing reason. Syntax
// only — the server fetches it later through the SSRF-guarded client, so the
// preview needs no network (and no DNS) to judge a file.
func normalizeExternalLabelURL(v string) (string, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", ""
	}
	if len(v) > MaxExternalLabelURLLen {
		return "", fmt.Sprintf("Link label quá dài (tối đa %d ký tự)", MaxExternalLabelURLLen)
	}
	for _, r := range v {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", "Link label có khoảng trắng hoặc ký tự lạ — dán lại đúng link"
		}
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", "Link label không hợp lệ — phải bắt đầu bằng http:// hoặc https://"
	}
	if u.User != nil {
		return "", "Link label không được chứa tài khoản/mật khẩu"
	}
	return v, ""
}

// ---------- template ----------

// TrackingImportTemplateXLSX renders the template CS downloads: order id, tracking
// number (optional when the line only brings a label) and the label link (optional
// too). The sample rows show both accepted identifiers — a shop order id and an
// internal code — and a label shared by two product lines of one order.
func (s *OrderService) TrackingImportTemplateXLSX() ([]byte, string, error) {
	grid := [][]string{
		{"OrderID", "Mã vận đơn", "Link label"},
		{"4141801137", "92612903029511573030094547", "https://file.gkelogistics.com/labels/4141801137.pdf"},
		{"100111", "", "https://file.gkelogistics.com/labels/100111.pdf"},
		{"100111", "", "https://file.gkelogistics.com/labels/100111.pdf"},
	}
	data, err := buildTemplateXLSX("Gắn mã vận đơn và label", grid, []float64{24, 36, 60})
	if err != nil {
		return nil, "", err
	}
	return data, "tracking-import-template.xlsx", nil
}

// ---------- preview ----------

// PreviewTrackingImport matches the file against the orders without writing
// anything. scopeFrom/scopeTo, when given, are the handed-over date range the
// operator had filtered on; the preview then also reports which orders of that
// range the file fails to cover (the "file ít hơn hệ thống" alert).
func (s *OrderService) PreviewTrackingImport(actor Actor, rows []TrackingImportRow, scopeFrom, scopeTo *time.Time) (*TrackingImportPreview, error) {
	if !canEditTracking(actor) {
		return nil, apperr.Forbidden("Bạn không có quyền cập nhật tracking")
	}
	if len(rows) > MaxTrackingImportRows {
		return nil, apperr.BadRequest(fmt.Sprintf("File quá lớn: tối đa %d dòng mỗi lần", MaxTrackingImportRows))
	}

	// Bulk-resolve every identifier in the file: one query per identifier kind.
	keySet := map[string]bool{}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.OrderKey != "" && !keySet[row.OrderKey] {
			keySet[row.OrderKey] = true
			keys = append(keys, row.OrderKey)
		}
	}
	byInternal := map[string]repositories.OrderCodeRef{}
	byStore := map[string][]repositories.OrderCodeRef{}
	refByID := map[uint]repositories.OrderCodeRef{}
	internalRefs, err := s.repo.Order.RefsByInternalCodes(keys)
	if err != nil {
		return nil, apperr.Internal("could not resolve internal codes").Wrap(err)
	}
	for _, ref := range internalRefs {
		byInternal[ref.InternalCode] = ref
		refByID[ref.ID] = ref
	}
	storeRefs, err := s.repo.Order.RefsByStoreOrderIDs(keys)
	if err != nil {
		return nil, apperr.Internal("could not resolve store order ids").Wrap(err)
	}
	for _, ref := range storeRefs {
		byStore[ref.StoreOrderID] = append(byStore[ref.StoreOrderID], ref)
		refByID[ref.ID] = ref
	}

	// Non-nil from the start: a nil slice marshals to JSON `null`, and the client
	// reads these as arrays (`.length`, `.filter`) — a null crashes the preview
	// screen instead of showing "0 dòng lỗi".
	matches := make([]TrackingImportMatch, 0, len(rows))
	issues := make([]TrackingImportIssue, 0)
	issue := func(row TrackingImportRow, code, reason string) {
		issues = append(issues, TrackingImportIssue{
			Row: row.Row, OrderKey: row.OrderKey, TrackingNumber: row.TrackingNumber,
			LabelURL: row.LabelURL, Code: code, Reason: reason,
		})
	}

	for _, row := range rows {
		if row.OrderKey == "" {
			issue(row, TrackingIssueEmptyOrder, "Thiếu mã đơn")
			continue
		}
		if row.TrackingNumber == "" && row.LabelURL == "" {
			issue(row, TrackingIssueEmptyTracking, "Thiếu mã vận đơn (hoặc link label)")
			continue
		}
		if row.TrackingNumber != "" && looksLikeExcelScientific(row.TrackingNumber) {
			issue(row, TrackingIssueExcelNumber,
				"Excel đã đổi mã vận đơn thành số mũ nên mất chữ số — định dạng ô là Text (hoặc gõ dấu ' trước mã) rồi dán lại mã")
			continue
		}
		if row.LabelURL != "" {
			clean, reason := normalizeExternalLabelURL(row.LabelURL)
			if reason != "" {
				issue(row, TrackingIssueBadLabel, reason)
				continue
			}
			row.LabelURL = clean
		}
		// Internal code first — it is unique by construction. A store order id is a
		// repeatable label, so several orders answering to it is a real possibility
		// the matcher refuses to guess through.
		ref, found := byInternal[row.OrderKey]
		if !found {
			candidates := byStore[row.OrderKey]
			switch len(candidates) {
			case 0:
				issue(row, TrackingIssueNotFound, "Không tìm thấy đơn nào khớp mã này trong hệ thống")
				continue
			case 1:
				ref = candidates[0]
			default:
				issue(row, TrackingIssueAmbiguous, fmt.Sprintf(
					"Mã đơn shop này khớp %d đơn khác nhau — dùng mã nội bộ để chỉ đích danh", len(candidates)))
				continue
			}
		}
		// Actions are decided after the one-order-several-lines merge below.
		matches = append(matches, TrackingImportMatch{
			Row: row.Row, OrderID: ref.ID, InternalCode: ref.InternalCode,
			StoreOrderID: ref.StoreOrderID, ShippingName: ref.ShippingName,
			TrackingNumber: row.TrackingNumber, LabelURL: row.LabelURL,
		})
	}

	// Cross-line safety passes. Each drops the affected lines from the matches
	// and files an issue instead — the bulk path never guesses.
	drop := map[int]bool{}
	mergedRows := 0

	// (a) One order, several lines. A label shared by several product lines of
	// one order (or a tracking number repeated on each) is normal: the lines are
	// folded into the first one and what each contributes is kept (line 1 holds
	// the number, line 2 the link → one assignment with both). Two DIFFERENT
	// numbers, or two DIFFERENT links, for one order: nobody knows which is right,
	// all its lines are dropped.
	byOrder := map[uint][]int{}
	for i, m := range matches {
		byOrder[m.OrderID] = append(byOrder[m.OrderID], i)
	}
	for _, idxs := range byOrder {
		if len(idxs) < 2 {
			continue
		}
		numbers, labels := map[string]bool{}, map[string]bool{}
		for _, i := range idxs {
			if n := matches[i].TrackingNumber; n != "" {
				numbers[strings.ToUpper(n)] = true
			}
			if l := matches[i].LabelURL; l != "" {
				labels[l] = true
			}
		}
		if len(numbers) > 1 || len(labels) > 1 {
			reason := "File chứa nhiều mã vận đơn khác nhau cho cùng một đơn — hệ thống không tự chọn"
			if len(numbers) <= 1 {
				reason = "File chứa nhiều link label khác nhau cho cùng một đơn — hệ thống không tự chọn"
			}
			for _, i := range idxs {
				drop[i] = true
				issue(TrackingImportRow{Row: matches[i].Row, OrderKey: matches[i].InternalCode,
					TrackingNumber: matches[i].TrackingNumber, LabelURL: matches[i].LabelURL}, TrackingIssueConflict, reason)
			}
			continue
		}
		first := idxs[0]
		for _, i := range idxs[1:] {
			drop[i] = true
			if matches[first].TrackingNumber == "" {
				matches[first].TrackingNumber = matches[i].TrackingNumber
			}
			if matches[first].LabelURL == "" {
				matches[first].LabelURL = matches[i].LabelURL
			}
			if matches[i].LabelURL != "" || matches[i].TrackingNumber == "" {
				mergedRows++ // shared label line: folded in silently
				continue
			}
			issue(TrackingImportRow{Row: matches[i].Row, OrderKey: matches[i].InternalCode, TrackingNumber: matches[i].TrackingNumber, LabelURL: matches[i].LabelURL},
				TrackingIssueDuplicate, fmt.Sprintf("Trùng với dòng %d — chỉ gắn một lần", matches[first].Row))
		}
	}

	// Actions, per side, against what the order holds now.
	for i := range matches {
		if drop[i] {
			continue
		}
		m := &matches[i]
		ref := refByID[m.OrderID]
		switch current := strings.TrimSpace(ref.TrackingNumber); {
		case m.TrackingNumber == "":
			m.Action = TrackingActionNone
		case current == "":
			m.Action = TrackingActionAssign
		case strings.EqualFold(current, m.TrackingNumber):
			m.Action = TrackingActionUnchanged
			m.CurrentTracking = current
		default:
			m.Action = TrackingActionOverwrite
			m.CurrentTracking = current
		}
		switch current := strings.TrimSpace(ref.ExternalLabelURL); {
		case m.LabelURL == "":
		case current == "":
			m.LabelAction = TrackingActionAssign
		case current == m.LabelURL:
			m.LabelAction = TrackingActionUnchanged
			m.CurrentLabel = current
		default:
			m.LabelAction = TrackingActionOverwrite
			m.CurrentLabel = current
		}
	}

	// (b) One tracking number on several different orders — in this factory a
	// parcel belongs to exactly one order (one open package per order), so this
	// is almost always a copy-paste slip in the file.
	byNumber := map[string][]int{}
	for i, m := range matches {
		if !drop[i] && m.TrackingNumber != "" {
			byNumber[strings.ToUpper(m.TrackingNumber)] = append(byNumber[strings.ToUpper(m.TrackingNumber)], i)
		}
	}
	for _, idxs := range byNumber {
		orderIDs := map[uint]bool{}
		for _, i := range idxs {
			orderIDs[matches[i].OrderID] = true
		}
		if len(orderIDs) < 2 {
			continue
		}
		for _, i := range idxs {
			drop[i] = true
			issue(TrackingImportRow{Row: matches[i].Row, OrderKey: matches[i].InternalCode, TrackingNumber: matches[i].TrackingNumber, LabelURL: matches[i].LabelURL},
				TrackingIssueSharedTracking, fmt.Sprintf("Mã vận đơn này xuất hiện ở %d đơn khác nhau trong file — thường là dán nhầm", len(orderIDs)))
		}
	}

	// (b2) One label link on several different orders: a label is one parcel, so
	// the same PDF on two orders means a row was pasted wrong.
	byLabel := map[string][]int{}
	for i, m := range matches {
		if !drop[i] && m.LabelURL != "" {
			byLabel[m.LabelURL] = append(byLabel[m.LabelURL], i)
		}
	}
	for _, idxs := range byLabel {
		orderIDs := map[uint]bool{}
		for _, i := range idxs {
			orderIDs[matches[i].OrderID] = true
		}
		if len(orderIDs) < 2 {
			continue
		}
		for _, i := range idxs {
			m := &matches[i]
			issue(TrackingImportRow{Row: m.Row, OrderKey: m.InternalCode, TrackingNumber: m.TrackingNumber, LabelURL: m.LabelURL},
				TrackingIssueSharedLabel, fmt.Sprintf("Link label này xuất hiện ở %d đơn khác nhau trong file — thường là dán nhầm", len(orderIDs)))
			if m.TrackingNumber == "" {
				drop[i] = true
			} else {
				// The tracking number on the line is still good; only the link is.
				m.LabelURL, m.LabelAction, m.CurrentLabel = "", "", ""
			}
		}
	}

	// (c) A number already attached to a DIFFERENT order in the system: assigning
	// it here would give two orders one parcel. The single-order screen may still
	// do it deliberately; the bulk path flags it.
	remainingNumbers := make([]string, 0, len(matches))
	for i, m := range matches {
		if !drop[i] && m.TrackingNumber != "" && m.Action != TrackingActionUnchanged {
			remainingNumbers = append(remainingNumbers, m.TrackingNumber)
		}
	}
	takenRefs, err := s.repo.Order.RefsByTrackingNumbers(remainingNumbers)
	if err != nil {
		return nil, apperr.Internal("could not check tracking numbers").Wrap(err)
	}
	takenBy := map[string]repositories.OrderCodeRef{}
	for _, ref := range takenRefs {
		takenBy[strings.ToUpper(ref.TrackingNumber)] = ref
	}
	for i, m := range matches {
		if drop[i] || m.TrackingNumber == "" || m.Action == TrackingActionUnchanged {
			continue
		}
		if owner, ok := takenBy[strings.ToUpper(m.TrackingNumber)]; ok && owner.ID != m.OrderID {
			drop[i] = true
			issue(TrackingImportRow{Row: m.Row, OrderKey: m.InternalCode, TrackingNumber: m.TrackingNumber, LabelURL: m.LabelURL},
				TrackingIssueTakenTracking, fmt.Sprintf("Mã vận đơn này đã gắn cho đơn khác (mã nội bộ %s)", owner.InternalCode))
		}
	}

	kept := matches[:0]
	for i, m := range matches {
		if !drop[i] {
			kept = append(kept, m)
		}
	}
	matches = kept
	sort.Slice(issues, func(i, j int) bool { return issues[i].Row < issues[j].Row })

	summary := TrackingImportSummary{
		TotalRows: len(rows), Issues: len(issues), ScopeTotal: -1, ScopeMissing: -1,
		MergedRows: mergedRows,
	}
	covered := map[uint]bool{}
	for _, m := range matches {
		covered[m.OrderID] = true
		switch m.Action {
		case TrackingActionAssign:
			summary.Assign++
		case TrackingActionOverwrite:
			summary.Overwrite++
		case TrackingActionUnchanged:
			summary.Unchanged++
		}
		switch m.LabelAction {
		case TrackingActionAssign:
			summary.LabelAssign++
		case TrackingActionOverwrite:
			summary.LabelOverwrite++
		case TrackingActionUnchanged:
			summary.LabelUnchanged++
		}
	}

	// The other direction: which handed-over-in-range orders (still without a
	// number) does the file NOT cover? The visible list is capped; the count is
	// exact anyway — coverage is counted over the matches themselves (only an
	// ASSIGN match can sit in scope, since scope means "no tracking yet"), so
	// the math stays right even when the fetched list is truncated.
	// ScopeMissing stays an empty array (never null) when no date range was sent:
	// "not compared" is carried by summary.scope_total = -1, not by a missing list.
	preview := &TrackingImportPreview{
		Matches: matches, Issues: issues, ScopeMissing: []repositories.OrderCodeRef{},
	}
	if scopeFrom != nil || scopeTo != nil {
		refs, total, err := s.repo.Order.HandedOverWithoutTracking(scopeFrom, scopeTo, trackingImportScopeCap)
		if err != nil {
			return nil, apperr.Internal("could not list handed-over orders").Wrap(err)
		}
		missing := make([]repositories.OrderCodeRef, 0, len(refs))
		for _, ref := range refs {
			if !covered[ref.ID] {
				missing = append(missing, ref)
			}
		}
		coveredInScope := int64(0)
		for _, m := range matches {
			if m.Action == TrackingActionAssign && refInScope(refByID[m.OrderID], scopeFrom, scopeTo) {
				coveredInScope++
			}
		}
		summary.ScopeTotal = total
		summary.ScopeMissing = total - coveredInScope
		preview.ScopeMissing = missing
	}
	preview.Summary = summary
	return preview, nil
}

// refInScope reports whether an order belongs to the comparison scope: handed
// over inside [from, to) and still without a tracking number.
func refInScope(ref repositories.OrderCodeRef, from, to *time.Time) bool {
	if !ref.SellerStatus.HandedOver() || strings.TrimSpace(ref.TrackingNumber) != "" || ref.HandedOverAt == nil {
		return false
	}
	if from != nil && ref.HandedOverAt.Before(*from) {
		return false
	}
	if to != nil && !ref.HandedOverAt.Before(*to) {
		return false
	}
	return true
}

// ---------- commit ----------

// CommitTrackingImport applies the confirmed assignments. Every one funnels
// through UpdateTracking so the bulk path has exactly the single-edit
// semantics: per-order audit entry, stale carrier state reset, old parcel
// released on the provider, new parcel registered. Failures are collected, not
// fatal — the rest of the batch still lands.
func (s *OrderService) CommitTrackingImport(actor Actor, in TrackingImportCommitInput) (*TrackingImportCommitResult, error) {
	if !canEditTracking(actor) {
		return nil, apperr.Forbidden("Bạn không có quyền cập nhật tracking")
	}
	if len(in.Assignments) > MaxTrackingCommitBatch {
		return nil, apperr.BadRequest(fmt.Sprintf("Tối đa %d đơn mỗi lần gắn — chia nhỏ file và thử lại", MaxTrackingCommitBatch))
	}

	res := &TrackingImportCommitResult{}
	seen := map[uint]bool{}
	for _, a := range in.Assignments {
		number := strings.TrimSpace(a.TrackingNumber)
		label, labelReason := normalizeExternalLabelURL(a.LabelURL)
		if a.OrderID == 0 || (number == "" && label == "" && labelReason == "") {
			res.Failed = append(res.Failed, TrackingCommitFailure{OrderID: a.OrderID, Reason: "Thiếu order_id hoặc mã vận đơn / link label"})
			continue
		}
		if labelReason != "" {
			res.Failed = append(res.Failed, TrackingCommitFailure{OrderID: a.OrderID, Reason: labelReason})
			continue
		}
		if seen[a.OrderID] {
			res.Failed = append(res.Failed, TrackingCommitFailure{OrderID: a.OrderID, Reason: "Đơn xuất hiện nhiều lần trong yêu cầu"})
			continue
		}
		seen[a.OrderID] = true
		failure := func(err error) {
			reason := "Không cập nhật được"
			if ae, ok := apperr.As(err); ok {
				reason = ae.Message
			}
			res.Failed = append(res.Failed, TrackingCommitFailure{OrderID: a.OrderID, Reason: reason})
		}
		if number != "" {
			if _, err := s.UpdateTracking(actor, a.OrderID, UpdateTrackingInput{TrackingNumber: &number}); err != nil {
				failure(err)
				continue
			}
		}
		if label != "" {
			if err := s.setExternalLabel(actor, a.OrderID, label); err != nil {
				failure(err)
				continue
			}
		}
		res.Updated++
	}
	s.audit.Log(actor, "ORDER_TRACKING_BULK_IMPORT", "order", nil,
		fmt.Sprintf("Gắn mã vận đơn / label từ file: %d cập nhật, %d lỗi", res.Updated, len(res.Failed)), nil)
	return res, nil
}

// setExternalLabel stores the outside label link on the order, with an audit
// entry holding the old and new link. It touches nothing else — no tracking
// state, no provider call: the link is only a print source for the warehouse.
func (s *OrderService) setExternalLabel(actor Actor, orderID uint, label string) error {
	order, err := s.GetOrder(orderID)
	if err != nil {
		return err
	}
	if order.ExternalLabelURL == label {
		return nil
	}
	if err := s.repo.Order.UpdateTracking(order.ID, map[string]interface{}{"external_label_url": label}); err != nil {
		return apperr.Internal("could not save external label").Wrap(err)
	}
	s.audit.Log(actor, "ORDER_EXTERNAL_LABEL_SET", "order", &order.ID,
		"Gắn label ngoài cho "+order.InternalCode, models.JSONMap{"external_label_url": []string{order.ExternalLabelURL, label}})
	return nil
}
