package services

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"

	"the-fulfillment/backend/internal/models"
)

// ---------------------------------------------------------------------------
// The 2026-08 seller template. These tests exist because the previous parser
// ignored any header it did not recognise, so a renamed template imported as a
// screen of "ORDER ID is required" errors with nothing pointing at the cause.
// ---------------------------------------------------------------------------

// newTemplateHeader returns the shipped template's header row.
func newTemplateHeader() []string {
	return append([]string(nil), orderImportTemplateHeaders...)
}

// gridWith builds a header+one-row grid from a label→value map, filling every
// other template column with a workable default so a test can vary one thing.
func gridWith(overrides map[string]string) [][]string {
	header := newTemplateHeader()
	defaults := map[string]string{
		"Seller ID": "S1", "Account": "acc-1", "Shop name": "MyStore",
		"DATE": "2026-08-20", "name": "John Doe",
		"Địa chỉ nhận": "12 Main St", "Địa chỉ nhận (Phụ)": "Apt 5",
		"Thành phố": "Austin", "Mã vùng": "TX", "Zipcode": "73301", "Quốc Gia": "US",
		"ORDER ID": "ETSY-1", "MÃ SKU": "TESTSKU", "Mã ảnh (nếu có)": "IMG-1", "SỐ LƯỢNG": "2",
		"DESIGN ORDER": "https://d.example.com/f.png", "designBack": "",
		"Mockup": "https://m.example.com/1.png", "EngraveText (if have)": "Hello",
		"ShippingPhone": "+1900000000", "Note": "rush",
	}
	rowVals := make([]string, len(header))
	for i, h := range header {
		v := defaults[h]
		if o, ok := overrides[h]; ok {
			v = o
		}
		rowVals[i] = v
	}
	return [][]string{header, rowVals}
}

func parseOne(t *testing.T, grid [][]string) (ImportRow, HeaderReport) {
	t.Helper()
	rows, hdr, err := rowsFromRecords("CSV", grid)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	return rows[0], hdr
}

// Every column of the new template must land in the field it names. This is the
// whole point of the change, so it is asserted column by column.
func TestNewTemplate_EveryColumnLandsInItsField(t *testing.T) {
	r, hdr := parseOne(t, gridWith(nil))

	if len(hdr.Missing) != 0 || len(hdr.Unknown) != 0 || len(hdr.Retired) != 0 {
		t.Fatalf("clean template produced a header report: %+v", hdr)
	}
	checks := []struct{ name, got, want string }{
		{"Seller ID", r.SellerRef, "S1"},
		{"Account", r.Account, "acc-1"},
		{"Shop name", r.StoreName, "MyStore"},
		{"DATE", r.OrderDateRaw, "2026-08-20"},
		{"name", r.ShippingName, "John Doe"},
		{"Địa chỉ nhận", r.ShippingAddress1, "12 Main St"},
		{"Địa chỉ nhận (Phụ)", r.ShippingAddress2, "Apt 5"},
		{"Thành phố", r.ShippingCity, "Austin"},
		{"Mã vùng", r.ShippingProvince, "TX"},
		{"Zipcode", r.ShippingZip, "73301"},
		{"Quốc Gia", r.ShippingCountry, "US"},
		{"ORDER ID", r.StoreOrderID, "ETSY-1"},
		{"MÃ SKU", r.SKU, "TESTSKU"},
		{"Mã ảnh (nếu có)", r.ImageCode, "IMG-1"},
		{"DESIGN ORDER", r.FrontDesignValue(), "https://d.example.com/f.png"},
		{"Mockup", r.Mockup, "https://m.example.com/1.png"},
		{"EngraveText (if have)", r.EngraveText, "Hello"},
		{"ShippingPhone", r.ShippingPhone, "+1900000000"},
		{"Note", r.Note, "rush"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("column %q: got %q, want %q", c.name, c.got, c.want)
		}
	}
	if int(r.Quantity) != 2 {
		t.Errorf("SỐ LƯỢNG: got %d, want 2", int(r.Quantity))
	}
}

// The single most dangerous header in the file: "Địa chỉ nhận (Phụ)" differs
// from "Địa chỉ nhận" only by a parenthesis. A parser that strips parentheses to
// be helpful would map both to address line 1 — line 2 would overwrite line 1 and
// the parcel would ship to an apartment number with no street.
func TestNewTemplate_AddressLine2NeverCollapsesOntoLine1(t *testing.T) {
	r, _ := parseOne(t, gridWith(map[string]string{
		"Địa chỉ nhận":       "12 Main St",
		"Địa chỉ nhận (Phụ)": "Apt 5",
	}))
	if r.ShippingAddress1 != "12 Main St" {
		t.Fatalf("address1 was overwritten: %q", r.ShippingAddress1)
	}
	if r.ShippingAddress2 != "Apt 5" {
		t.Fatalf("address2 lost: %q", r.ShippingAddress2)
	}
}

// A note in brackets is decoration, not identity: "Mã ảnh (nếu có)" is the same
// column as "Mã ảnh". Only KNOWN filler notes may be dropped (see the test above).
func TestHeaderKey_DropsOnlyKnownParentheticalNotes(t *testing.T) {
	cases := map[string]string{
		"Mã ảnh (nếu có)":       "mãảnh",
		"EngraveText (if have)": "engravetext",
		"Note (optional)":       "note",
		"Địa chỉ nhận (Phụ)":    "địachỉnhận(phụ)", // preserved — it is a different column
	}
	for in, want := range cases {
		if got := headerKey(in); got != want {
			t.Errorf("headerKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// Files come out of Excel, Google Sheets and half a dozen exporters. Diacritics
// may be stripped, Unicode may arrive decomposed (macOS), and "CSV UTF-8" from
// Excel prefixes a BOM. All of them must still map.
func TestNewTemplate_ToleratesExporterMangling(t *testing.T) {
	header := []string{
		"\ufeff" + "SELLER ID", "ORDER ID", "MA SKU", "SO LUONG", "name",
		"Dia chi nhan", "Quoc Gia", norm.NFD.String("Mã ảnh (nếu có)"), "Thanh pho",
	}
	data := []string{"S1", "ETSY-9", "TESTSKU", "3", "Jane", "1 Main St", "US", "IMG-9", "Austin"}
	rows, hdr, err := rowsFromRecords("CSV", [][]string{header, data})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(hdr.Unknown) != 0 {
		t.Fatalf("exporter-mangled headers were not recognised: %+v", hdr.Unknown)
	}
	r := rows[0]
	if r.SellerRef != "S1" || r.StoreOrderID != "ETSY-9" || r.SKU != "TESTSKU" ||
		int(r.Quantity) != 3 || r.ShippingAddress1 != "1 Main St" || r.ImageCode != "IMG-9" ||
		r.ShippingCity != "Austin" {
		t.Fatalf("mangled headers mapped wrong: %+v", r)
	}
}

// The previous template must keep importing. Sellers do not all move on the same
// day, and telling one of them "redo your file" is a support ticket per seller.
func TestOldEnglishTemplate_StillImports(t *testing.T) {
	header := []string{
		"StoreOrderID", "Account", "StoreName", "Quantity", "SKU", "Mã ảnh",
		"Front Design", "Back Design", "Mockup", "EngraveText",
		"ShippingName", "ShippingAddress1", "ShippingAddress2", "ShippingCity",
		"ShippingZip", "ShippingProvince", "ShippingCountry", "ShippingPhone", "Note",
	}
	data := []string{
		"Etsy-1", "acc-01", "MyStore", "3", "WOOD-01", "IMG-77",
		"https://d.example.com/f.png", "", "https://m.example.com/1.png", "Love",
		"John Doe", "1 Main St", "", "Austin", "73301", "TX", "US", "+1900000000", "rush",
	}
	rows, hdr, err := rowsFromRecords("CSV", [][]string{header, data})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(hdr.Missing) != 0 {
		t.Fatalf("old template reported missing columns: %+v", hdr.Missing)
	}
	r := rows[0]
	if r.StoreOrderID != "Etsy-1" || r.SKU != "WOOD-01" || r.ShippingName != "John Doe" ||
		r.ShippingAddress1 != "1 Main St" || r.ShippingCountry != "US" {
		t.Fatalf("old template mapped wrong: %+v", r)
	}
}

// Columns the system deliberately stopped importing are told apart from typos:
// one is "we removed this", the other is "you misspelled something".
func TestHeaderReport_SeparatesRetiredFromUnknown(t *testing.T) {
	header := append(newTemplateHeader(), "IOSS", "ProductName", "Mau sac", "ShippingEmail")
	data := make([]string, len(header))
	for i, h := range header {
		switch h {
		case "ORDER ID":
			data[i] = "E-1"
		case "MÃ SKU":
			data[i] = "TESTSKU"
		case "SỐ LƯỢNG":
			data[i] = "1"
		case "name":
			data[i] = "Jane"
		case "Địa chỉ nhận":
			data[i] = "1 Main St"
		case "Quốc Gia":
			data[i] = "US"
		default:
			data[i] = "x"
		}
	}
	_, hdr, err := rowsFromRecords("CSV", [][]string{header, data})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(hdr.Missing) != 0 {
		t.Fatalf("nothing should be missing: %+v", hdr.Missing)
	}
	wantRetired := map[string]bool{"IOSS": true, "ProductName": true, "ShippingEmail": true}
	for _, r := range hdr.Retired {
		if !wantRetired[r] {
			t.Errorf("unexpected retired column %q", r)
		}
		delete(wantRetired, r)
	}
	if len(wantRetired) != 0 {
		t.Errorf("retired columns not reported: %v", wantRetired)
	}
	if len(hdr.Unknown) != 1 || hdr.Unknown[0] != "Mau sac" {
		t.Errorf("unknown columns = %+v, want [Mau sac]", hdr.Unknown)
	}
}

// A file whose required columns are absent is a wrong-template problem, and the
// report has to name the columns in the words the template uses.
func TestHeaderReport_NamesMissingRequiredColumns(t *testing.T) {
	header := []string{"Account", "Shop name", "DATE", "Mockup"}
	_, hdr, err := rowsFromRecords("CSV", [][]string{header, {"a", "b", "2026-08-20", "m"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"ORDER ID", "MÃ SKU", "SỐ LƯỢNG", "name (tên người nhận)", "Địa chỉ nhận", "Quốc Gia"}
	got := strings.Join(hdr.Missing, "|")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing report %q does not mention %q", got, w)
		}
	}
}

// Excel leaves a tail of formatted-but-empty rows behind. Importing them turns
// into a screen of "ORDER ID is required" for rows nobody typed into.
func TestParser_SkipsTrailingBlankRows(t *testing.T) {
	grid := gridWith(nil)
	blank := make([]string, len(grid[0]))
	grid = append(grid, blank, blank, blank)
	rows, _, err := rowsFromRecords("CSV", grid)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("blank rows were imported: got %d rows", len(rows))
	}
}

// The template puts "Phone" in the address block, so that is the column people
// fill in. Dropping it outright would ship parcels with no contact number.
func TestPhoneFallback_PrefersShippingPhoneButKeepsPhone(t *testing.T) {
	header := []string{"ORDER ID", "MÃ SKU", "SỐ LƯỢNG", "name", "Địa chỉ nhận", "Quốc Gia", "Phone", "ShippingPhone"}
	cases := []struct{ phone, shipping, want string }{
		{"0900111222", "", "0900111222"},           // only the address-block column filled
		{"", "0900333444", "0900333444"},           // only the trailing column filled
		{"0900111222", "0900333444", "0900333444"}, // both: the explicit shipping phone wins
		{"", "", ""},
	}
	for _, c := range cases {
		rows, _, err := rowsFromRecords("CSV", [][]string{header,
			{"E-1", "TESTSKU", "1", "Jane", "1 Main St", "US", c.phone, c.shipping}})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got := rows[0].RecipientPhone(); got != c.want {
			t.Errorf("Phone=%q ShippingPhone=%q → %q, want %q", c.phone, c.shipping, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// DATE parsing
// ---------------------------------------------------------------------------

func TestParseOrderDate(t *testing.T) {
	cases := []struct {
		in        string
		want      string
		ambiguous bool
		wantErr   bool
	}{
		{in: "2026-08-20", want: "2026-08-20"},
		{in: "2026/08/20", want: "2026-08-20"},
		{in: "20/08/2026", want: "2026-08-20"},                  // day-first, unambiguous (20 > 12)
		{in: "8/20/2026", want: "2026-08-20"},                   // month-first, unambiguous (20 > 12)
		{in: "20-8-26", want: "2026-08-20"},                     // two-digit year
		{in: "20260820", want: "2026-08-20"},                    // compact
		{in: "2026-08-20 00:00:00", want: "2026-08-20"},         // Excel datetime string
		{in: "2026-08-20T09:30:00Z", want: "2026-08-20"},        // ISO timestamp
		{in: "05/06/2026", want: "2026-06-05", ambiguous: true}, // could be either; day-first + flag
		{in: "  2026-08-20  ", want: "2026-08-20"},
		{in: "", want: ""},
		{in: "31/02/2026", wantErr: true}, // not a real day
		{in: "2026-13-01", wantErr: true},
		{in: "hôm qua", wantErr: true},
		{in: "N/A", wantErr: true},
	}
	for _, c := range cases {
		got, amb, err := ParseOrderDate(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseOrderDate(%q) = %q, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseOrderDate(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want || amb != c.ambiguous {
			t.Errorf("ParseOrderDate(%q) = (%q, %v), want (%q, %v)", c.in, got, amb, c.want, c.ambiguous)
		}
	}
}

// An Excel cell left as a plain number is a serial day count, not a year.
func TestParseOrderDate_ExcelSerial(t *testing.T) {
	// 45000 = 2023-03-15 in the 1900 serial system Excel uses.
	got, _, err := ParseOrderDate("45000")
	if err != nil {
		t.Fatalf("serial date errored: %v", err)
	}
	if got != "2023-03-15" {
		t.Fatalf("serial 45000 = %q, want 2023-03-15", got)
	}
}

// ---------------------------------------------------------------------------
// Preview: seller cross-check, header gating, date advisories
// ---------------------------------------------------------------------------

// templateRow is a valid row of the new template, for service-level tests.
func templateRow(orderID, date, sellerRef string) ImportRow {
	return ImportRow{
		SellerRef: sellerRef, OrderDateRaw: date,
		StoreOrderID: orderID, SKU: "TESTSKU", Quantity: 1, ImageCode: "IMG-" + orderID,
		Mockup:       "https://example.com/" + orderID + ".png",
		ShippingName: "Jane Doe", ShippingAddress1: "1 Main St", ShippingCountry: "US",
		ShippingPhone: "+1900000000",
	}
}

func fullHeader() HeaderReport {
	return HeaderReport{Present: []string{fStoreOrd, fSKU, fQuantity, fShipName, fShipAddr1, fShipCntry, fOrderDate, fSellerRef, "ShippingPhone"}}
}

// Committing a file onto the wrong seller creates real orders under an account
// that never sold them, and production may pick them up before anyone notices.
// So a "Seller ID" that disagrees with the selected seller BLOCKS, never warns.
func TestPreview_SellerIDMismatchBlocks(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	// The fixture seller is id=1, code "S1", name "Seller One". Its database id
	// ("1") is NOT a way to name it: the column holds seller codes, and an id
	// match is how a "6" Excel made out of "006" would pass for seller #6.
	accepted := []string{"", "S1", "s1", " S1 ", "Seller One"}
	for _, ref := range accepted {
		prev, err := svc.Preview(actor, 1, "XLSX", "f.xlsx",
			[]ImportRow{templateRow("OK-1", "2026-08-20", ref)}, fullHeader())
		if err != nil {
			t.Fatalf("preview(%q): %v", ref, err)
		}
		if prev.ErrorRows != 0 {
			t.Errorf("Seller ID %q should be accepted, got %+v", ref, prev.Errors)
		}
	}

	for _, ref := range []string{"S2", "1"} {
		prev, err := svc.Preview(actor, 1, "XLSX", "f.xlsx",
			[]ImportRow{templateRow("BAD-1", "2026-08-20", ref)}, fullHeader())
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		if prev.ErrorRows != 1 || len(prev.Errors) != 1 || prev.Errors[0].ErrorCode != "SELLER_MISMATCH" {
			t.Fatalf("Seller ID %q must block the row, got %+v", ref, prev.Errors)
		}
		if prev.ValidRows != 0 {
			t.Fatalf("a mismatched row must not be committable, valid=%d", prev.ValidRows)
		}
	}
}

// A file missing required columns is rejected as one clear whole-file error, not
// as one identical error per row.
func TestPreview_RejectsFileWithMissingRequiredColumns(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)

	_, err := svc.Preview(Actor{ID: 1}, 1, "XLSX", "wrong.xlsx",
		[]ImportRow{templateRow("E-1", "2026-08-20", "")},
		HeaderReport{Missing: []string{"ORDER ID", "MÃ SKU"}, Unknown: []string{"Mau sac"}})
	if err == nil {
		t.Fatal("a file missing required columns must be rejected outright")
	}
	msg := err.Error()
	for _, want := range []string{"ORDER ID", "MÃ SKU", "Mau sac"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection message %q does not mention %q", msg, want)
		}
	}
}

// Ignored columns must be announced once, at file level — not per row.
func TestPreview_AnnouncesIgnoredColumnsOnce(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)

	hdr := fullHeader()
	hdr.Retired = []string{"IOSS", "ProductName"}
	hdr.Unknown = []string{"Mau sac"}
	rows := []ImportRow{
		templateRow("E-1", "2026-08-20", ""),
		templateRow("E-2", "2026-08-20", ""),
		templateRow("E-3", "2026-08-20", ""),
	}
	prev, err := svc.Preview(Actor{ID: 1}, 1, "XLSX", "f.xlsx", rows, hdr)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	counts := map[string]int{}
	for _, w := range prev.Warnings {
		counts[w.ErrorCode]++
	}
	if counts["COL_RETIRED"] != 1 || counts["COL_UNKNOWN"] != 1 {
		t.Fatalf("column notices should appear exactly once each, got %+v", counts)
	}
}

// The DATE advisories: a genuinely ambiguous value, a future date, and two rows
// of the same order disagreeing about which day it is.
func TestPreview_DateAdvisories(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)

	rows := []ImportRow{
		templateRow("AMB-1", "05/06/2026", ""),
		templateRow("FUT-1", "2099-01-01", ""),
		templateRow("CONF-1", "2026-08-20", ""),
		templateRow("CONF-1", "2026-08-21", ""),
	}
	prev, err := svc.Preview(Actor{ID: 1}, 1, "XLSX", "f.xlsx", rows, fullHeader())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	seen := map[string]bool{}
	for _, w := range prev.Warnings {
		seen[w.ErrorCode] = true
	}
	for _, code := range []string{"DATE_AMBIGUOUS", "DATE_FUTURE", "DATE_CONFLICT"} {
		if !seen[code] {
			t.Errorf("missing %s warning; got %+v", code, prev.Warnings)
		}
	}
	if prev.ErrorRows != 0 {
		t.Fatalf("date advisories must not block: %+v", prev.Errors)
	}
}

// An unreadable DATE is a blocking row error: guessing would silently file the
// order under the wrong business day, which is also its "STT trong ngày".
func TestPreview_UnreadableDateBlocksTheRow(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)

	prev, err := svc.Preview(Actor{ID: 1}, 1, "XLSX", "f.xlsx",
		[]ImportRow{templateRow("E-1", "hôm qua", "")}, fullHeader())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.ErrorRows != 1 || prev.Errors[0].ErrorCode != "DATE_INVALID" {
		t.Fatalf("want DATE_INVALID, got %+v", prev.Errors)
	}
}

// A file with no DATE column at all is one fact about the file, reported once.
func TestPreview_MissingDateColumnWarnsOnceNotPerRow(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)

	hdr := HeaderReport{Present: []string{fStoreOrd, fSKU, fQuantity, fShipName, fShipAddr1, fShipCntry, "ShippingPhone"}}
	rows := []ImportRow{
		templateRow("E-1", "", ""), templateRow("E-2", "", ""), templateRow("E-3", "", ""),
	}
	prev, err := svc.Preview(Actor{ID: 1}, 1, "XLSX", "f.xlsx", rows, hdr)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	counts := map[string]int{}
	for _, w := range prev.Warnings {
		counts[w.ErrorCode]++
	}
	if counts["DATE_COLUMN_MISSING"] != 1 {
		t.Fatalf("want one DATE_COLUMN_MISSING, got %+v", counts)
	}
	if counts["DATE_EMPTY"] != 0 {
		t.Fatalf("per-row DATE_EMPTY must not fire when the column is absent: %+v", counts)
	}
}

// ---------------------------------------------------------------------------
// Commit: the business day comes from the file
// ---------------------------------------------------------------------------

// Orders land on the day the seller placed them, and "STT trong ngày" restarts
// per day — a file spanning three days allocates three independent sequences.
func TestCommit_OrderDateComesFromFileAndSeqIsPerDay(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	rows := []ImportRow{
		templateRow("A-1", "20/08/2026", ""),
		templateRow("B-1", "19/08/2026", ""),
		templateRow("A-2", "20/08/2026", ""),
		templateRow("C-1", "18/08/2026", ""),
	}
	prev, err := svc.Preview(actor, 1, "XLSX", "f.xlsx", rows, fullHeader())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.ErrorRows != 0 {
		t.Fatalf("unexpected errors: %+v", prev.Errors)
	}
	if _, err := svc.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	orders := loadOrders(t, db, 1)
	if len(orders) != 4 {
		t.Fatalf("want 4 orders, got %d", len(orders))
	}
	want := map[string]struct {
		date string
		seq  int
	}{
		"A-1": {"2026-08-20", 1},
		"A-2": {"2026-08-20", 2}, // same day → next in that day's sequence
		"B-1": {"2026-08-19", 1}, // different day → its own sequence, restarting at 1
		"C-1": {"2026-08-18", 1},
	}
	for _, o := range orders {
		w, ok := want[o.StoreOrderID]
		if !ok {
			t.Fatalf("unexpected order %s", o.StoreOrderID)
		}
		if o.OrderDate != w.date || o.DailySeq != w.seq {
			t.Errorf("%s: order_date=%s seq=%d, want %s / %d", o.StoreOrderID, o.OrderDate, o.DailySeq, w.date, w.seq)
		}
	}
}

// A second file for a day that already has orders continues that day's sequence
// instead of restarting it — otherwise two orders would share an "STT".
func TestCommit_SecondFileContinuesThatDaysSequence(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	first, err := svc.Preview(actor, 1, "XLSX", "1.xlsx",
		[]ImportRow{templateRow("A-1", "2026-08-20", ""), templateRow("A-2", "2026-08-20", "")}, fullHeader())
	if err != nil {
		t.Fatalf("preview 1: %v", err)
	}
	if _, err := svc.Commit(actor, first.ImportJobID); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	second, err := svc.Preview(actor, 1, "XLSX", "2.xlsx",
		[]ImportRow{templateRow("A-3", "2026-08-20", "")}, fullHeader())
	if err != nil {
		t.Fatalf("preview 2: %v", err)
	}
	if _, err := svc.Commit(actor, second.ImportJobID); err != nil {
		t.Fatalf("commit 2: %v", err)
	}

	seqs := map[string]int{}
	for _, o := range loadOrders(t, db, 1) {
		seqs[o.StoreOrderID] = o.DailySeq
	}
	if seqs["A-1"] != 1 || seqs["A-2"] != 2 || seqs["A-3"] != 3 {
		t.Fatalf("per-day sequence did not continue across files: %+v", seqs)
	}
}

// Rows of one order that disagree about DATE still produce ONE order — the first
// row decides, and preview said so. An order cannot sit on two days.
func TestCommit_ConflictingDatesStillProduceOneOrder(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	prev, err := svc.Preview(actor, 1, "XLSX", "f.xlsx", []ImportRow{
		templateRow("X-1", "2026-08-20", ""),
		templateRow("X-1", "2026-08-21", ""),
	}, fullHeader())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := svc.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	orders := loadOrders(t, db, 1)
	if len(orders) != 1 {
		t.Fatalf("want 1 order, got %d", len(orders))
	}
	if orders[0].OrderDate != "2026-08-20" {
		t.Fatalf("order_date = %s, want the first row's 2026-08-20", orders[0].OrderDate)
	}
	if len(orders[0].Items) != 2 {
		t.Fatalf("want 2 items on the order, got %d", len(orders[0].Items))
	}
}

// An empty DATE falls back to the import day rather than failing a commit the
// operator already confirmed.
func TestCommit_EmptyDateFallsBackToImportDay(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	hdr := fullHeader()
	prev, err := svc.Preview(actor, 1, "XLSX", "f.xlsx", []ImportRow{templateRow("E-1", "", "")}, hdr)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := svc.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	orders := loadOrders(t, db, 1)
	if got, want := orders[0].OrderDate, AppDateString(time.Now()); got != want {
		t.Fatalf("order_date = %s, want the import day %s", got, want)
	}
}

// The phone the file actually filled in reaches the order, whichever column it
// was in.
func TestCommit_PhoneFallbackReachesTheOrder(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	r := templateRow("P-1", "2026-08-20", "")
	r.ShippingPhone = ""
	r.PhoneAlt = "0900111222"
	prev, err := svc.Preview(actor, 1, "XLSX", "f.xlsx", []ImportRow{r}, fullHeader())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := svc.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	orders := loadOrders(t, db, 1)
	if orders[0].ShippingPhone != "0900111222" {
		t.Fatalf("shipping_phone = %q, want the Phone column's value", orders[0].ShippingPhone)
	}
}

// ---------------------------------------------------------------------------
// Product name now comes from master data, not from the file
// ---------------------------------------------------------------------------

// The template no longer carries a product name, so the name shown on an order
// line must come from the SKU in master data — and must not be persisted onto
// the line, where it would drift out of sync with the catalogue.
func TestItemProductName_ComesFromSKUMasterData(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	prev, err := svc.Preview(actor, 1, "XLSX", "f.xlsx",
		[]ImportRow{templateRow("PN-1", "2026-08-20", "")}, fullHeader())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := svc.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Read straight from the table, with no SKU preload: the name is not stored,
	// so it must come back empty.
	var bare []models.OrderItem
	if err := db.Find(&bare).Error; err != nil {
		t.Fatalf("load items: %v", err)
	}
	if len(bare) != 1 || bare[0].ProductName != "" {
		t.Fatalf("product name should not be persisted on the item, got %+v", bare)
	}

	// Reading the order back through the service resolves it from the SKU.
	orderSvc := &OrderService{repo: svc.repo, audit: &AuditService{repo: svc.repo}}
	orders := loadOrders(t, db, 1)
	got, err := orderSvc.GetOrder(orders[0].ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].ProductName != "Test SKU" {
		t.Fatalf("product name should come from master data, got %+v", got.Items)
	}
}

// ---------------------------------------------------------------------------
// End to end: download the template, fill it in, upload it back
// ---------------------------------------------------------------------------

// The template is only useful if the round-trip works: what we hand the seller
// must parse cleanly when they hand it back, with no missing/unknown columns and
// no blocking validation errors.
func TestTemplateRoundTrip_DownloadFillUploadCommits(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	// Build the workbook exactly as the download endpoint does, but with the
	// fixture's own seller code and SKU so it validates end to end.
	grid := [][]string{orderImportTemplateHeaders}
	for _, sample := range orderImportTemplateSample {
		filled := append([]string(nil), sample...)
		for i, h := range orderImportTemplateHeaders {
			switch h {
			case "Seller ID":
				filled[i] = "S1"
			case "MÃ SKU":
				filled[i] = "TESTSKU"
			}
		}
		grid = append(grid, filled)
	}
	data, err := buildTemplateXLSX("Đơn hàng", grid, orderImportTemplateWidths)
	if err != nil {
		t.Fatalf("build workbook: %v", err)
	}

	rows, hdr, err := ParseXLSX(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse uploaded workbook: %v", err)
	}
	if len(hdr.Missing) != 0 || len(hdr.Unknown) != 0 || len(hdr.Retired) != 0 {
		t.Fatalf("round-tripped template is not clean: %+v", hdr)
	}
	prev, err := svc.Preview(actor, 1, "XLSX", "order-import-template.xlsx", rows, hdr)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.ErrorRows != 0 {
		t.Fatalf("the shipped template must validate cleanly, got %+v", prev.Errors)
	}
	if prev.OrderCount != 1 || prev.ValidRows != 2 {
		t.Fatalf("template sample should be 1 order with 2 items, got %d order(s) / %d rows",
			prev.OrderCount, prev.ValidRows)
	}
	job, err := svc.Commit(actor, prev.ImportJobID)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if job.Status != models.ImportCommitted {
		t.Fatalf("job status = %s, want COMMITTED", job.Status)
	}
	orders := loadOrders(t, db, 1)
	if len(orders) != 1 || len(orders[0].Items) != 2 {
		t.Fatalf("want 1 order with 2 items, got %+v", orders)
	}
	if orders[0].OrderDate != "2026-08-20" {
		t.Fatalf("order_date = %s, want the template's DATE 2026-08-20", orders[0].OrderDate)
	}
	if orders[0].ShippingAddress1 != "12 Main St" || orders[0].ShippingCountry != "US" {
		t.Fatalf("recipient block did not survive the round trip: %+v", orders[0])
	}
}

// Preview stores the valid rows as JSON on the import job; Commit reads them back
// through ImportRow.UnmarshalJSON, which resolves keys through headerToField. So
// every json tag on ImportRow MUST normalize to a known header key — otherwise
// the field survives preview, disappears at commit, and nothing anywhere errors.
// This caught SellerRef and PhoneAlt silently emptying themselves between the two.
func TestImportRow_JSONTagsSurviveThePreviewCommitRoundTrip(t *testing.T) {
	rt := reflect.TypeOf(ImportRow{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if _, ok := headerToField[headerKey(name)]; !ok {
			t.Errorf("ImportRow.%s has json tag %q, which no header maps to: its value "+
				"would be dropped between preview and commit", rt.Field(i).Name, name)
		}
	}
}

// The end-to-end version of the same guarantee, through the real code path.
func TestPreviewToCommit_CarriesEveryFieldThroughStoredRows(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	r := templateRow("RT-1", "19/08/2026", "S1")
	r.ShippingPhone = ""
	r.PhoneAlt = "0900111222"
	r.ShippingAddress2 = "Apt 5"
	r.StoreName = "MyStore"
	r.Account = "acc-1"
	r.ShippingCity = "Austin"
	r.ShippingProvince = "TX"
	r.ShippingZip = "73301"
	r.Note = "rush"
	r.BackDesign = "https://d.example.com/b.png"
	r.FrontDesign = "https://d.example.com/f.png"
	r.EngraveText = "Hello"

	prev, err := svc.Preview(actor, 1, "XLSX", "f.xlsx", []ImportRow{r}, fullHeader())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := svc.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	o := loadOrders(t, db, 1)[0]
	checks := []struct{ name, got, want string }{
		{"order_date", o.OrderDate, "2026-08-19"},
		{"store_name", o.StoreName, "MyStore"},
		{"account", o.Account, "acc-1"},
		{"shipping_name", o.ShippingName, "Jane Doe"},
		{"shipping_address1", o.ShippingAddress1, "1 Main St"},
		{"shipping_address2", o.ShippingAddress2, "Apt 5"},
		{"shipping_city", o.ShippingCity, "Austin"},
		{"shipping_province", o.ShippingProvince, "TX"},
		{"shipping_zip", o.ShippingZip, "73301"},
		{"shipping_country", o.ShippingCountry, "US"},
		{"shipping_phone", o.ShippingPhone, "0900111222"},
		{"note", o.Note, "rush"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if len(o.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(o.Items))
	}
	it := o.Items[0]
	if it.DesignURL != "https://d.example.com/f.png" || it.BackDesignURL != "https://d.example.com/b.png" ||
		it.EngraveText != "Hello" || it.ImageCode != "IMG-RT-1" {
		t.Errorf("item lost fields through the stored rows: %+v", it)
	}
}

// The file the customer actually sent — all 23 columns from their sheet, in
// their order, including the two the system deliberately dropped (Phone folds
// into ShippingPhone, ShippingEmail is gone). Nothing here may be a surprise on
// the day they upload it.
func TestCustomerFileAsSent_ImportsWithNoBlockingErrors(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	header := []string{
		"Seller ID", "Account", "Shop name", "DATE", "name",
		"Địa chỉ nhận", "Địa chỉ nhận (Phụ)", "Phone", "Thành phố", "Mã vùng", "Zipcode", "Quốc Gia",
		"ORDER ID", "MÃ SKU", "Mã ảnh (nếu có)", "SỐ LƯỢNG",
		"DESIGN ORDER", "designBack", "Mockup", "EngraveText (if have)",
		"ShippingPhone", "ShippingEmail", "Note",
	}
	// Two products on one order, exactly how the sheet repeats the address block.
	rowA := []string{
		"S1", "acc-001", "Etsy-Demo", "20/08/2026", "John Doe",
		"12 Main St", "Apt 5", "0900111222", "Austin", "TX", "73301", "US",
		"ETSY-77", "TESTSKU", "IMG-1", "1",
		"https://d.example.com/1-front.png", "", "https://m.example.com/1.png", "Happy Birthday",
		"", "john@example.com", "Gói quà",
	}
	rowB := []string{
		"S1", "acc-001", "Etsy-Demo", "20/08/2026", "John Doe",
		"12 Main St", "Apt 5", "0900111222", "Austin", "TX", "73301", "US",
		"ETSY-77", "TESTSKU", "IMG-2", "2",
		"https://d.example.com/2-front.png", "https://d.example.com/2-back.png",
		"https://m.example.com/2.png", "",
		"", "john@example.com", "",
	}

	rows, hdr, err := rowsFromRecords("XLSX", [][]string{header, rowA, rowB})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Nothing required is missing, and the two dropped columns are reported as
	// retired (not as unknown, and not silently swallowed).
	if len(hdr.Missing) != 0 {
		t.Fatalf("customer file reported missing columns: %+v", hdr.Missing)
	}
	if len(hdr.Unknown) != 0 {
		t.Fatalf("customer file has unrecognised columns: %+v", hdr.Unknown)
	}
	if len(hdr.Retired) != 1 || hdr.Retired[0] != "ShippingEmail" {
		t.Fatalf("retired report = %+v, want just [ShippingEmail]", hdr.Retired)
	}

	prev, err := svc.Preview(actor, 1, "XLSX", "khach.xlsx", rows, hdr)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.ErrorRows != 0 {
		t.Fatalf("customer file must import without blocking errors: %+v", prev.Errors)
	}
	if prev.OrderCount != 1 || prev.ValidRows != 2 {
		t.Fatalf("want 1 order / 2 rows, got %d / %d", prev.OrderCount, prev.ValidRows)
	}
	// The retired column is announced once so nobody wonders where email went.
	var retiredNotice bool
	for _, w := range prev.Warnings {
		if w.ErrorCode == "COL_RETIRED" {
			retiredNotice = true
		}
	}
	if !retiredNotice {
		t.Fatalf("no notice that ShippingEmail was ignored: %+v", prev.Warnings)
	}

	if _, err := svc.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	orders := loadOrders(t, db, 1)
	if len(orders) != 1 {
		t.Fatalf("want 1 order, got %d", len(orders))
	}
	o := orders[0]
	if o.OrderDate != "2026-08-20" || o.DailySeq != 1 {
		t.Errorf("order date/seq = %s / %d, want 2026-08-20 / 1", o.OrderDate, o.DailySeq)
	}
	// The phone was only in the "Phone" column; it must still reach the parcel.
	if o.ShippingPhone != "0900111222" {
		t.Errorf("shipping_phone = %q, want the Phone column's 0900111222", o.ShippingPhone)
	}
	if o.ShippingAddress1 != "12 Main St" || o.ShippingAddress2 != "Apt 5" {
		t.Errorf("address lines crossed: %q / %q", o.ShippingAddress1, o.ShippingAddress2)
	}
	if o.ShippingProvince != "TX" || o.ShippingZip != "73301" {
		t.Errorf("Mã vùng/Zipcode landed wrong: province=%q zip=%q", o.ShippingProvince, o.ShippingZip)
	}
	if len(o.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(o.Items))
	}
	var qtys []int
	for _, it := range o.Items {
		qtys = append(qtys, it.Quantity)
	}
	if qtys[0] != 1 || qtys[1] != 2 {
		t.Errorf("quantities = %v, want [1 2]", qtys)
	}
	if o.Items[1].BackDesignURL != "https://d.example.com/2-back.png" {
		t.Errorf("designBack lost: %q", o.Items[1].BackDesignURL)
	}
}
