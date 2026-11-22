package services

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// "Xuất Excel" of the journey board. Two promises are pinned here:
//   1. the file holds exactly the orders the screen's filter matches, with the
//      columns the factory asked for, times in Vietnam time as real Excel dates;
//   2. the same file, with tracking numbers typed into "Mã vận đơn", is accepted
//      back by "Gắn mã từ Excel" without touching a single column.

type journeyFixture struct {
	db     *gorm.DB
	svc    *OrderService
	seller *models.Seller
}

func newJourneyFixture(t *testing.T) *journeyFixture {
	t.Helper()
	db, svc := newTrackingImportFixture(t)
	s := &models.Seller{Code: "XS1", Name: "Xưởng Seller Một"}
	if err := db.Create(s).Error; err != nil {
		t.Fatalf("seller: %v", err)
	}
	return &journeyFixture{db: db, svc: svc, seller: s}
}

type journeySeed struct {
	code, store, tracking string
	status                models.SellerStatus
	trackStatus           models.TrackingStatus
	handedAt              *time.Time
	qty                   []int // live item quantities
	cancelledQty          int   // one cancelled line of this quantity (0 = none)
}

func (fx *journeyFixture) seed(t *testing.T, s journeySeed) *models.Order {
	t.Helper()
	if s.status == "" {
		s.status = models.SellerStatusHandedOff
	}
	if s.trackStatus == "" {
		s.trackStatus = models.TrackingNone
		if s.tracking != "" {
			s.trackStatus = models.TrackingPending
		}
	}
	o := &models.Order{
		InternalCode: s.code, StoreOrderID: s.store, SellerID: fx.seller.ID,
		ReviewStatus: models.ReviewApproved, SellerStatus: s.status,
		TrackingNumber: s.tracking, TrackingStatus: s.trackStatus, HandedOverAt: s.handedAt,
		ShippingName: "Người nhận " + s.code, ShippingCity: "Austin", ShippingProvince: "TX", ShippingCountry: "US",
	}
	if err := fx.db.Create(o).Error; err != nil {
		t.Fatalf("seed order %s: %v", s.code, err)
	}
	line := 0
	add := func(q int, cancel models.CancellationStatus) {
		line++
		it := &models.OrderItem{OrderID: o.ID, InternalCode: fmt.Sprintf("%s_%d", s.code, line),
			SKUCode: "SKU", Quantity: q, LineNo: line, CancellationStatus: cancel}
		if err := fx.db.Create(it).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}
	for _, q := range s.qty {
		add(q, models.CancellationNone)
	}
	if s.cancelledQty > 0 {
		add(s.cancelledQty, models.CancellationSeller)
	}
	return o
}

// journeyFilter is what the web page sends for a tab + search + day range.
func journeyFilter(scope string, from, to *time.Time, search string) repositories.OrderFilter {
	yes := true
	f := repositories.OrderFilter{HandedOver: &yes, HandedOverFrom: from, HandedOverTo: to, Search: search}
	switch scope {
	case "missing":
		no := false
		f.HasTracking = &no
	case "has":
		f.HasTracking = &yes
	}
	return f
}

func readJourneyExport(t *testing.T, data []byte) (*excelize.File, string, [][]string) {
	t.Helper()
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open export: %v", err)
	}
	sheet := f.GetSheetList()[0]
	rows, err := f.GetRows(sheet)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	return f, sheet, rows
}

func colIndex(t *testing.T, header []string, name string) int {
	t.Helper()
	for i, h := range header {
		if h == name {
			return i
		}
	}
	t.Fatalf("column %q missing from %v", name, header)
	return -1
}

// Every header the importer would read must be exactly the three intended ones;
// any other header colliding with an alias would silently change which column
// the importer takes as the key or the number.
func TestJourneyExport_HeadersOnlyKeyColumnsHitImportAliases(t *testing.T) {
	hits := map[string]string{}
	for _, h := range journeyExportHeaders {
		key := normalizeTrackingHeader(h)
		switch {
		case trackingInternalHeaderKeys[key]:
			hits[h] = "internal"
		case trackingOrderHeaderKeys[key]:
			hits[h] = "order"
		case trackingNumberHeaderKeys[key]:
			hits[h] = "tracking"
		}
	}
	want := map[string]string{journeyColInternal: "internal", journeyColStoreOrder: "order", journeyColTracking: "tracking"}
	if fmt.Sprint(hits) != fmt.Sprint(want) {
		t.Fatalf("headers hitting import aliases = %v, want %v", hits, want)
	}
	if len(journeyExportWidths) != len(journeyExportHeaders) {
		t.Fatalf("%d widths for %d columns", len(journeyExportWidths), len(journeyExportHeaders))
	}
}

func TestJourneyExport_ColumnsValuesAndExcelTypes(t *testing.T) {
	fx := newJourneyFixture(t)
	// 07:05 UTC = 14:05 in Hanoi — the file must say 14:05.
	handed := time.Date(2026, 11, 20, 7, 5, 0, 0, time.UTC)
	trackedAt := time.Date(2026, 11, 21, 23, 30, 0, 0, time.UTC) // 06:30 on the 22nd in Hanoi
	a := fx.seed(t, journeySeed{code: "200001", store: "4141801137", handedAt: &handed, qty: []int{2, 1}, cancelledQty: 5})
	b := fx.seed(t, journeySeed{code: "200002", store: "SO-B", tracking: "9400111899223456789012",
		trackStatus: models.TrackingInTransit, handedAt: &handed, qty: []int{1}})
	if err := fx.db.Model(b).Updates(map[string]interface{}{
		"tracking_detail": "Departed facility", "tracking_location": "LOS ANGELES CA", "tracking_updated_at": trackedAt,
	}).Error; err != nil {
		t.Fatalf("tracking fields: %v", err)
	}
	// Not handed over: never on the journey board, never in its file.
	fx.seed(t, journeySeed{code: "200003", store: "SO-C", status: models.SellerStatusPacked, qty: []int{1}})

	data, filename, err := fx.svc.JourneyExportXLSX(journeyFilter("all", nil, nil, ""))
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.HasPrefix(filename, "hanh-trinh-don-") || !strings.HasSuffix(filename, ".xlsx") {
		t.Errorf("filename %q", filename)
	}
	f, sheet, rows := readJourneyExport(t, data)
	if strings.Join(rows[0], "|") != strings.Join(journeyExportHeaders, "|") {
		t.Fatalf("header row = %v", rows[0])
	}
	if len(rows) != 3 {
		t.Fatalf("want 2 data rows (A, B), got %d: %v", len(rows)-1, rows)
	}
	hdr := rows[0]
	byCode := map[string][]string{}
	for _, r := range rows[1:] {
		for len(r) < len(hdr) {
			r = append(r, "")
		}
		byCode[r[colIndex(t, hdr, journeyColInternal)]] = r
	}
	ra, rb := byCode[a.InternalCode], byCode[b.InternalCode]
	if ra == nil || rb == nil {
		t.Fatalf("rows by internal code: %v", byCode)
	}
	check := func(row []string, col, want string) {
		t.Helper()
		if got := row[colIndex(t, hdr, col)]; got != want {
			t.Errorf("%s / %s = %q, want %q", row[1], col, got, want)
		}
	}
	check(ra, journeyColStoreOrder, "4141801137")
	check(ra, journeyColSeller, "Xưởng Seller Một")
	check(ra, journeyColQty, "3") // 2 + 1; the cancelled line of 5 is not in the parcel
	check(ra, journeyColRecipient, "Người nhận 200001")
	check(ra, journeyColCity, "Austin")
	check(ra, journeyColProvince, "TX")
	check(ra, journeyColCountry, "US")
	check(ra, journeyColHandedOver, "20/11/2026 14:05")
	check(ra, journeyColTracking, "")
	check(ra, journeyColStatus, "Đã bàn giao")
	check(ra, journeyColTrackStatus, "Chưa có")
	check(ra, journeyColTrackUpdated, "")
	check(rb, journeyColTracking, "9400111899223456789012") // all 22 digits, not 9.4E+21
	check(rb, journeyColStatus, "Đang vận chuyển")
	check(rb, journeyColTrackStatus, "Đang vận chuyển")
	check(rb, journeyColTrackDetail, "Departed facility")
	check(rb, journeyColTrackPlace, "LOS ANGELES CA")
	check(rb, journeyColTrackUpdated, "22/11/2026 06:30")

	// The time columns are real date-times (a serial number with a date format),
	// so Excel filters them by date and sorts them chronologically.
	rawRows, err := f.GetRows(sheet, excelize.Options{RawCellValue: true})
	if err != nil {
		t.Fatalf("raw rows: %v", err)
	}
	handedCol := colIndex(t, hdr, journeyColHandedOver)
	for _, r := range rawRows[1:] {
		if r[1] != a.InternalCode {
			continue
		}
		// 2026-11-20 14:05 → Excel serial 46346 + 14:05/24h.
		want := 46346 + (14*60+5)/1440.0
		var got float64
		if _, err := fmt.Sscan(r[handedCol], &got); err != nil || got < want-1e-6 || got > want+1e-6 {
			t.Errorf("handed-over cell raw value %q, want serial %.6f (a real date-time)", r[handedCol], want)
		}
	}
	// Tracking column: Text format on the column itself, so a number typed into
	// ANY row stays text.
	trackColName, _ := excelize.ColumnNumberToName(colIndex(t, hdr, journeyColTracking) + 1)
	styleID, err := f.GetCellStyle(sheet, trackColName+"2")
	if err != nil {
		t.Fatalf("style: %v", err)
	}
	st, err := f.GetStyle(styleID)
	if err != nil || st.NumFmt != 49 {
		t.Errorf("tracking cell number format = %v (err %v), want 49 (Text)", st, err)
	}
	colStyleID, err := f.GetColStyle(sheet, trackColName)
	if err != nil {
		t.Fatalf("col style: %v", err)
	}
	if cs, _ := f.GetStyle(colStyleID); cs == nil || cs.NumFmt != 49 {
		t.Errorf("tracking COLUMN default format = %v, want 49 (Text) for rows typed later", cs)
	}
}

// The file is the screen's filter: tab × date range × search box, all rows not
// just one page. Compared against ListOrders — the list endpoint behind the
// screen — for every combination.
func TestJourneyExport_SameSetAsTheList(t *testing.T) {
	fx := newJourneyFixture(t)
	d18 := time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC) // 18/11 01:00 VN
	d19 := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	d20 := time.Date(2026, 11, 19, 17, 30, 0, 0, time.UTC) // 20/11 00:30 VN
	n := 0
	for _, at := range []time.Time{d18, d19, d20} {
		for _, tracking := range []string{"", "TRK"} {
			for _, name := range []string{"alice", "bob"} {
				n++
				code := fmt.Sprintf("3%05d", n)
				tr := ""
				if tracking != "" {
					tr = tracking + code
				}
				at := at
				o := fx.seed(t, journeySeed{code: code, store: "SO-" + name + code, tracking: tr, handedAt: &at, qty: []int{1}})
				if err := fx.db.Model(o).Update("shipping_name", strings.ToUpper(name)+" Smith").Error; err != nil {
					t.Fatalf("name: %v", err)
				}
			}
		}
	}
	fx.seed(t, journeySeed{code: "399999", store: "SO-prod", status: models.SellerStatusProduction, qty: []int{1}})

	vn := AppLocation()
	// 19/11 → 20/11 (the page sends next-day start). UTC instants, like the
	// handler parses them: SQLite compares time strings lexically.
	from := time.Date(2026, 11, 19, 0, 0, 0, 0, vn).UTC()
	to := time.Date(2026, 11, 21, 0, 0, 0, 0, vn).UTC()
	type tc struct {
		scope    string
		from, to *time.Time
		search   string
		want     int
	}
	cases := []tc{
		{"all", nil, nil, "", 12},
		{"missing", nil, nil, "", 6},
		{"has", nil, nil, "", 6},
		{"missing", &from, &to, "", 4},
		{"has", &from, &to, "bob", 2},
		{"all", nil, nil, "alice", 6},
		{"all", nil, nil, "TRK3", 6}, // tracking-number search
	}
	for _, c := range cases {
		name := fmt.Sprintf("%s/%v/%q", c.scope, c.from != nil, c.search)
		f := journeyFilter(c.scope, c.from, c.to, c.search)
		lf := f
		lf.Page = repositories.Page{Page: 1, PageSize: repositories.PageSizeAll}
		listed, total, err := fx.svc.ListOrders(lf)
		if err != nil {
			t.Fatalf("%s list: %v", name, err)
		}
		data, _, err := fx.svc.JourneyExportXLSX(f)
		if err != nil {
			t.Fatalf("%s export: %v", name, err)
		}
		_, _, rows := readJourneyExport(t, data)
		var listCodes, fileCodes []string
		for _, o := range listed {
			listCodes = append(listCodes, o.InternalCode)
		}
		for _, r := range rows[1:] {
			fileCodes = append(fileCodes, r[1])
		}
		if int(total) != c.want || len(fileCodes) != c.want {
			t.Errorf("%s: list total %d, file rows %d, want %d", name, total, len(fileCodes), c.want)
		}
		// Same order as the screen (newest first) — compare unsorted.
		if strings.Join(listCodes, ",") != strings.Join(fileCodes, ",") {
			t.Errorf("%s: file %v != list %v", name, fileCodes, listCodes)
		}
	}
}

// The round trip the factory asked for: export the "Chưa có mã vận đơn" tab,
// type numbers into some rows of the SAME file, upload it — no column edited.
func TestJourneyExport_FillAndReimportRoundTrip(t *testing.T) {
	fx := newJourneyFixture(t)
	at := time.Now().Add(-2 * time.Hour)
	var orders []*models.Order
	for i := 0; i < 5; i++ {
		orders = append(orders, fx.seed(t, journeySeed{code: fmt.Sprintf("40000%d", i), store: fmt.Sprintf("SO-%d", i), handedAt: &at}))
	}
	// Two orders sharing ONE store order id: by store id they are ambiguous, but
	// the export's key column is the internal code, so both still match.
	dupA := fx.seed(t, journeySeed{code: "400010", store: "SO-DUP", handedAt: &at})
	dupB := fx.seed(t, journeySeed{code: "400011", store: "SO-DUP", handedAt: &at})
	tracked := fx.seed(t, journeySeed{code: "400020", store: "SO-T", tracking: "HAS-OLD", handedAt: &at})

	data, _, err := fx.svc.JourneyExportXLSX(journeyFilter("all", nil, nil, ""))
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	f, sheet, rows := readJourneyExport(t, data)
	hdr := rows[0]
	trackCol, _ := excelize.ColumnNumberToName(colIndex(t, hdr, journeyColTracking) + 1)
	fill := map[string]string{
		orders[0].InternalCode: "9400111899223456789012", // 22-digit USPS, written as text
		orders[2].InternalCode: "YT2521521266065831",
		dupA.InternalCode:      "DUP-A-1",
		dupB.InternalCode:      "DUP-B-1",
	}
	for i, r := range rows[1:] {
		if num, ok := fill[r[1]]; ok {
			if err := f.SetCellStr(sheet, fmt.Sprintf("%s%d", trackCol, i+2), num); err != nil {
				t.Fatalf("fill: %v", err)
			}
		}
	}
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatalf("save filled: %v", err)
	}

	parsed, err := ParseTrackingXLSXFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("filled export rejected by the importer: %v", err)
	}
	if !parsed.JourneyExport {
		t.Error("parser did not recognise its own export")
	}
	// 8 exported: 4 filled, 1 already tracked (kept its number → UNCHANGED), 3 blank.
	if len(parsed.Rows) != 5 || parsed.SkippedBlank != 3 {
		t.Fatalf("parsed %d rows / %d skipped, want 5 / 3: %+v", len(parsed.Rows), parsed.SkippedBlank, parsed.Rows)
	}
	for _, r := range parsed.Rows {
		if !strings.HasPrefix(r.OrderKey, "4000") {
			t.Errorf("key column must be the internal code, got %q", r.OrderKey)
		}
	}

	actor := Actor{ID: 1, Role: models.RoleCS}
	preview, err := fx.svc.PreviewTrackingImport(actor, parsed.Rows, nil, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.Summary.Assign != 4 || preview.Summary.Unchanged != 1 || preview.Summary.Issues != 0 || preview.Summary.Overwrite != 0 {
		t.Fatalf("preview summary %+v, issues %+v", preview.Summary, preview.Issues)
	}
	var assignments []TrackingAssignment
	for _, m := range preview.Matches {
		if m.Action == TrackingActionAssign {
			assignments = append(assignments, TrackingAssignment{OrderID: m.OrderID, TrackingNumber: m.TrackingNumber})
		}
	}
	res, err := fx.svc.CommitTrackingImport(actor, TrackingImportCommitInput{Assignments: assignments})
	if err != nil || res.Updated != 4 || len(res.Failed) != 0 {
		t.Fatalf("commit: %+v %v", res, err)
	}
	got := map[string]string{}
	var all []models.Order
	fx.db.Find(&all)
	for _, o := range all {
		got[o.InternalCode] = o.TrackingNumber
	}
	for code, num := range fill {
		if got[code] != num {
			t.Errorf("order %s: tracking %q, want %q", code, got[code], num)
		}
	}
	if got[tracked.InternalCode] != "HAS-OLD" || got[orders[1].InternalCode] != "" {
		t.Errorf("untouched rows changed: tracked=%q blank=%q", got[tracked.InternalCode], got[orders[1].InternalCode])
	}
}

// Same file saved as CSV from Excel keeps working (header marker included).
func TestJourneyExport_ReimportAsCSV(t *testing.T) {
	csv := strings.Join(journeyExportHeaders, ",") + "\n" +
		"SO-1,500001,S,1,A,B,C,US,20/11/2026 14:05,TRK-1,,,,,\n" +
		"SO-2,500002,S,1,A,B,C,US,20/11/2026 14:05,,,,,,\n"
	file, err := ParseTrackingCSVFile(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("csv: %v", err)
	}
	if !file.JourneyExport || file.SkippedBlank != 1 || len(file.Rows) != 1 ||
		file.Rows[0].OrderKey != "500001" || file.Rows[0].TrackingNumber != "TRK-1" || file.Rows[0].Row != 1 {
		t.Fatalf("csv parse: %+v", file)
	}
}

// Parser rules the export relies on, checked on plain files too.
func TestTrackingImport_ParserKeyAndBlankRules(t *testing.T) {
	// Internal code is the key even when the shop-id column comes first.
	rows, err := ParseTrackingCSV(strings.NewReader("Mã đơn shop,Mã nội bộ,Mã vận đơn\nSO-DUP,600001,T1\nSO-X,,T2\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 2 || rows[0].OrderKey != "600001" {
		t.Fatalf("internal code must win as key: %+v", rows)
	}
	// …and an empty internal-code cell falls back to the shop id on that line.
	if rows[1].OrderKey != "SO-X" {
		t.Errorf("fallback to shop id on empty internal cell: %+v", rows[1])
	}

	// A plain (non-export) file keeps the old rule: a blank number is a line the
	// preview reports, not one the parser hides.
	rows, err = ParseTrackingCSV(strings.NewReader("OrderID,Mã vận đơn\nA1,\nA2,T\n"))
	if err != nil || len(rows) != 2 || rows[0].TrackingNumber != "" {
		t.Fatalf("plain file must keep blank-number lines: %+v %v", rows, err)
	}

	// An export with nothing filled says so, instead of "no data".
	_, err = ParseTrackingCSVFile(strings.NewReader(strings.Join(journeyExportHeaders, ",") + "\nSO-1,1,,,,,,,,,,,,,\n"))
	if err == nil || !strings.Contains(err.Error(), "Chưa dòng nào được điền") {
		t.Errorf("empty export: %v", err)
	}
}

// A 22-digit number typed into a General-format cell comes back as
// "9.40011189922346E+21" — digits lost. The preview must refuse it, never attach it.
func TestTrackingImport_RejectsExcelScientificNumbers(t *testing.T) {
	db, svc := newTrackingImportFixture(t)
	seedImportOrder(t, db, "700001", "SO-1", "", nil)
	seedImportOrder(t, db, "700002", "SO-2", "", nil)
	preview, err := svc.PreviewTrackingImport(Actor{ID: 1, Role: models.RoleCS}, []TrackingImportRow{
		{Row: 1, OrderKey: "700001", TrackingNumber: "9.40011189922346E+21"},
		{Row: 2, OrderKey: "700002", TrackingNumber: "9400111899223456789012"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.Summary.Assign != 1 || len(preview.Issues) != 1 || preview.Issues[0].Code != TrackingIssueExcelNumber {
		t.Fatalf("summary %+v issues %+v", preview.Summary, preview.Issues)
	}
	for _, ok := range []string{"YT2521521266065831", "1Z999AA10123456784", "LP00123456789012", "92612903029511573030094547", "4PX3001234567CN", "E12345"} {
		if looksLikeExcelScientific(ok) {
			t.Errorf("real tracking number %q flagged as Excel scientific", ok)
		}
	}
	for _, bad := range []string{"9.40011189922346E+21", "1E+15", "9,26129030295116E+25"} {
		if !looksLikeExcelScientific(bad) {
			t.Errorf("%q not flagged", bad)
		}
	}
}

// Over the cap: refuse with the matching count instead of a truncated file.
func TestJourneyExport_CapRefusesWithCount(t *testing.T) {
	fx := newJourneyFixture(t)
	at := time.Now().Add(-time.Hour)
	batch := make([]models.Order, 0, MaxJourneyExportRows+1)
	for i := 0; i <= MaxJourneyExportRows; i++ {
		batch = append(batch, models.Order{
			InternalCode: fmt.Sprintf("8%06d", i), StoreOrderID: "SO", SellerID: fx.seller.ID,
			ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusHandedOff,
			TrackingStatus: models.TrackingNone, HandedOverAt: &at,
		})
	}
	quiet := fx.db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	if err := quiet.CreateInBatches(batch, 500).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, _, err := fx.svc.JourneyExportXLSX(journeyFilter("all", nil, nil, ""))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d đơn", MaxJourneyExportRows+1)) {
		t.Fatalf("over-cap export: %v", err)
	}
	// Narrowing the filter under the cap exports normally.
	data, _, err := fx.svc.JourneyExportXLSX(journeyFilter("all", nil, nil, "8000001"))
	if err != nil {
		t.Fatalf("narrowed export: %v", err)
	}
	_, _, rows := readJourneyExport(t, data)
	codes := []string{}
	for _, r := range rows[1:] {
		codes = append(codes, r[1])
	}
	sort.Strings(codes)
	if len(codes) != 1 || codes[0] != "8000001" {
		t.Errorf("narrowed export rows %v", codes)
	}
	if _, _, err := fx.svc.JourneyExportXLSX(journeyFilter("all", nil, nil, "khong-co")); err == nil {
		t.Error("an empty result must say so, not hand out a header-only file")
	}
}
