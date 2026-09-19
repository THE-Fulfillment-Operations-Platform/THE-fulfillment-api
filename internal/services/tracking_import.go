package services

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/unicode/norm"

	"the-fulfillment/backend/internal/apperr"
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

// MaxTrackingImportRows caps one uploaded file. Same magnitude as the order
// import; a CS day is a few hundred parcels, so the cap only stops runaway files.
const MaxTrackingImportRows = 1000

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
}

// Match actions. UNCHANGED lines are shown but never committed; OVERWRITE lines
// are committed only when the operator explicitly opts in on the client.
const (
	TrackingActionAssign    = "ASSIGN"
	TrackingActionOverwrite = "OVERWRITE"
	TrackingActionUnchanged = "UNCHANGED"
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
}

// TrackingImportIssue is a file line that will NOT be applied, with the reason.
type TrackingImportIssue struct {
	Row            int    `json:"row"`
	OrderKey       string `json:"order_key"`
	TrackingNumber string `json:"tracking_number,omitempty"`
	Code           string `json:"code"`
	Reason         string `json:"reason"`
}

// TrackingImportSummary is the preview's headline numbers. ScopeTotal /
// ScopeMissing describe the other direction of the comparison — orders the
// SYSTEM has (handed over in the requested date range, still without tracking)
// that the file does or does not cover. Both are -1 when no range was sent,
// meaning "not computed", which the client renders differently from 0.
type TrackingImportSummary struct {
	TotalRows    int   `json:"total_rows"`
	Assign       int   `json:"assign"`
	Overwrite    int   `json:"overwrite"`
	Unchanged    int   `json:"unchanged"`
	Issues       int   `json:"issues"`
	ScopeTotal   int64 `json:"scope_total"`
	ScopeMissing int64 `json:"scope_missing"`
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
	OrderID        uint   `json:"order_id" binding:"required"`
	TrackingNumber string `json:"tracking_number" binding:"required"`
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

// Header aliases for the two columns. The order column accepts everything the
// journey screen displays — the shop's order id or our internal code — because
// the carrier file quotes whichever the CS pasted into the manifest.
var (
	trackingOrderHeaderKeys = map[string]bool{
		"orderid": true, "order": true, "ordercode": true, "ordernumber": true,
		"mãđơn": true, "madon": true, "mãđơnhàng": true, "madonhang": true,
		"mãđơnshop": true, "madonshop": true, "storeorderid": true,
		"mãnộibộ": true, "manoibo": true, "internalcode": true,
	}
	trackingNumberHeaderKeys = map[string]bool{
		"tracking": true, "trackingnumber": true, "trackingcode": true, "trackingid": true,
		"mãvậnđơn": true, "mavandon": true, "vậnđơn": true, "vandon": true,
		"mãtracking": true, "matracking": true, "mãvậnchuyển": true, "mavanchuyen": true,
	}
)

// ParseTrackingCSV reads a CSV stream into tracking-import rows.
func ParseTrackingCSV(r io.Reader) ([]TrackingImportRow, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, apperr.BadRequest("Không đọc được file CSV: " + err.Error())
	}
	return trackingRowsFromRecords(records)
}

// ParseTrackingXLSX reads the first worksheet of an .xlsx/.xlsm stream.
func ParseTrackingXLSX(r io.Reader) ([]TrackingImportRow, error) {
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
	return trackingRowsFromRecords(records)
}

func trackingRowsFromRecords(records [][]string) ([]TrackingImportRow, error) {
	if len(records) < 2 {
		return nil, apperr.BadRequest("File phải có dòng tiêu đề và ít nhất một dòng dữ liệu")
	}
	orderCol, trackCol := -1, -1
	for i, h := range records[0] {
		key := normalizeTrackingHeader(h)
		switch {
		case trackingOrderHeaderKeys[key] && orderCol == -1:
			orderCol = i
		case trackingNumberHeaderKeys[key] && trackCol == -1:
			trackCol = i
		}
	}
	if orderCol == -1 || trackCol == -1 {
		return nil, apperr.BadRequest(
			"Không nhận diện được cột OrderID / Mã vận đơn — tải file mẫu để xem đúng định dạng")
	}
	cell := func(rec []string, col int) string {
		if col < len(rec) {
			return strings.TrimSpace(rec[col])
		}
		return ""
	}
	rows := make([]TrackingImportRow, 0, len(records)-1)
	for i, rec := range records[1:] {
		key, number := cell(rec, orderCol), cell(rec, trackCol)
		if key == "" && number == "" {
			continue // fully blank line — Excel files end with plenty of them
		}
		rows = append(rows, TrackingImportRow{Row: i + 1, OrderKey: key, TrackingNumber: number})
	}
	if len(rows) == 0 {
		return nil, apperr.BadRequest("File không có dòng dữ liệu nào")
	}
	return rows, nil
}

// ---------- template ----------

// TrackingImportTemplateXLSX renders the 2-column template CS downloads. The
// sample rows show both accepted identifiers: a shop order id and an internal
// code.
func (s *OrderService) TrackingImportTemplateXLSX() ([]byte, string, error) {
	grid := [][]string{
		{"OrderID", "Mã vận đơn"},
		{"4141801137", "92612903029511573030094547"},
		{"100111", "YT2521521266065831"},
	}
	data, err := buildTemplateXLSX("Gắn mã vận đơn", grid, []float64{24, 36})
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
			Code: code, Reason: reason,
		})
	}

	for _, row := range rows {
		if row.OrderKey == "" {
			issue(row, TrackingIssueEmptyOrder, "Thiếu mã đơn")
			continue
		}
		if row.TrackingNumber == "" {
			issue(row, TrackingIssueEmptyTracking, "Thiếu mã vận đơn")
			continue
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
		m := TrackingImportMatch{
			Row: row.Row, OrderID: ref.ID, InternalCode: ref.InternalCode,
			StoreOrderID: ref.StoreOrderID, ShippingName: ref.ShippingName,
			TrackingNumber: row.TrackingNumber,
		}
		switch current := strings.TrimSpace(ref.TrackingNumber); {
		case current == "":
			m.Action = TrackingActionAssign
		case strings.EqualFold(current, row.TrackingNumber):
			m.Action = TrackingActionUnchanged
			m.CurrentTracking = current
		default:
			m.Action = TrackingActionOverwrite
			m.CurrentTracking = current
		}
		matches = append(matches, m)
	}

	// Cross-line safety passes. Each drops the affected lines from the matches
	// and files an issue instead — the bulk path never guesses.
	drop := map[int]bool{}

	// (a) One order, several lines. Same number repeated → keep the first, mark
	// the rest as duplicates. Different numbers → nobody knows which is right,
	// drop them all.
	byOrder := map[uint][]int{}
	for i, m := range matches {
		byOrder[m.OrderID] = append(byOrder[m.OrderID], i)
	}
	for _, idxs := range byOrder {
		if len(idxs) < 2 {
			continue
		}
		numbers := map[string]bool{}
		for _, i := range idxs {
			numbers[strings.ToUpper(matches[i].TrackingNumber)] = true
		}
		if len(numbers) > 1 {
			for _, i := range idxs {
				drop[i] = true
				issue(TrackingImportRow{Row: matches[i].Row, OrderKey: matches[i].InternalCode, TrackingNumber: matches[i].TrackingNumber},
					TrackingIssueConflict, "File chứa nhiều mã vận đơn khác nhau cho cùng một đơn — hệ thống không tự chọn")
			}
		} else {
			for _, i := range idxs[1:] {
				drop[i] = true
				issue(TrackingImportRow{Row: matches[i].Row, OrderKey: matches[i].InternalCode, TrackingNumber: matches[i].TrackingNumber},
					TrackingIssueDuplicate, fmt.Sprintf("Trùng với dòng %d — chỉ gắn một lần", matches[idxs[0]].Row))
			}
		}
	}

	// (b) One tracking number on several different orders — in this factory a
	// parcel belongs to exactly one order (one open package per order), so this
	// is almost always a copy-paste slip in the file.
	byNumber := map[string][]int{}
	for i, m := range matches {
		if !drop[i] {
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
			issue(TrackingImportRow{Row: matches[i].Row, OrderKey: matches[i].InternalCode, TrackingNumber: matches[i].TrackingNumber},
				TrackingIssueSharedTracking, fmt.Sprintf("Mã vận đơn này xuất hiện ở %d đơn khác nhau trong file — thường là dán nhầm", len(orderIDs)))
		}
	}

	// (c) A number already attached to a DIFFERENT order in the system: assigning
	// it here would give two orders one parcel. The single-order screen may still
	// do it deliberately; the bulk path flags it.
	remainingNumbers := make([]string, 0, len(matches))
	for i, m := range matches {
		if !drop[i] && m.Action != TrackingActionUnchanged {
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
		if drop[i] || m.Action == TrackingActionUnchanged {
			continue
		}
		if owner, ok := takenBy[strings.ToUpper(m.TrackingNumber)]; ok && owner.ID != m.OrderID {
			drop[i] = true
			issue(TrackingImportRow{Row: m.Row, OrderKey: m.InternalCode, TrackingNumber: m.TrackingNumber},
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
	}
	covered := map[uint]bool{}
	for _, m := range matches {
		covered[m.OrderID] = true
		switch m.Action {
		case TrackingActionAssign:
			summary.Assign++
		case TrackingActionOverwrite:
			summary.Overwrite++
		default:
			summary.Unchanged++
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
		if a.OrderID == 0 || number == "" {
			res.Failed = append(res.Failed, TrackingCommitFailure{OrderID: a.OrderID, Reason: "Thiếu order_id hoặc mã vận đơn"})
			continue
		}
		if seen[a.OrderID] {
			res.Failed = append(res.Failed, TrackingCommitFailure{OrderID: a.OrderID, Reason: "Đơn xuất hiện nhiều lần trong yêu cầu"})
			continue
		}
		seen[a.OrderID] = true
		if _, err := s.UpdateTracking(actor, a.OrderID, UpdateTrackingInput{TrackingNumber: &number}); err != nil {
			reason := "Không cập nhật được"
			if ae, ok := apperr.As(err); ok {
				reason = ae.Message
			}
			res.Failed = append(res.Failed, TrackingCommitFailure{OrderID: a.OrderID, Reason: reason})
			continue
		}
		res.Updated++
	}
	s.audit.Log(actor, "ORDER_TRACKING_BULK_IMPORT", "order", nil,
		fmt.Sprintf("Gắn mã vận đơn từ file: %d cập nhật, %d lỗi", res.Updated, len(res.Failed)), nil)
	return res, nil
}
