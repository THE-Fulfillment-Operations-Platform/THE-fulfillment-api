package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/services"
)

// "Xuất Excel" of Hành trình đơn hàng over real HTTP, the way the web calls it:
// the export takes the SAME query string as the list (handed_over + tab + day
// range + search box), returns every matching order, and the file — with
// numbers typed into "Mã vận đơn" — goes straight back through "Gắn mã từ Excel"
// (preview, then commit).

func (h *multiHarness) tokenFor(role models.Role) string {
	h.t.Helper()
	if tok, ok := h.tokens[role]; ok {
		return tok
	}
	u := &models.User{Email: "jx-" + strings.ToLower(string(role)) + "@t", PasswordHash: "x", FullName: string(role), Role: role, IsActive: true}
	if err := h.db.Create(u).Error; err != nil {
		h.t.Fatalf("seed %s: %v", role, err)
	}
	// Same secret as newMultiHarness: any manager with it issues valid tokens.
	tok, _, err := auth.NewManager("test-secret-test-secret-test-secret", time.Hour).Issue(u)
	if err != nil {
		h.t.Fatalf("issue %s: %v", role, err)
	}
	h.tokens[role] = tok
	return tok
}

func (h *multiHarness) do(req *http.Request, role models.Role) *httptest.ResponseRecorder {
	h.t.Helper()
	req.Header.Set("Authorization", "Bearer "+h.tokenFor(role))
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func TestJourneyExport_HTTP_SameFilterAsListAndRoundTrip(t *testing.T) {
	h := newMultiHarness(t)
	db := h.db
	seller := &models.Seller{Code: "JX1", Name: "Seller JX"}
	if err := db.Create(seller).Error; err != nil {
		t.Fatalf("seller: %v", err)
	}
	vn := services.AppLocation()
	// Stored as UTC instants (what Postgres timestamptz hands back): SQLite in
	// tests compares time strings lexically, so mixed zones would lie.
	day := func(d, hh int) *time.Time { v := time.Date(2026, 11, d, hh, 0, 0, 0, vn).UTC(); return &v }
	n := 0
	seed := func(status models.SellerStatus, handed *time.Time, tracking, name string) *models.Order {
		n++
		code := fmt.Sprintf("91%04d", n)
		o := &models.Order{InternalCode: code, StoreOrderID: "SO-" + code, SellerID: seller.ID,
			ReviewStatus: models.ReviewApproved, SellerStatus: status, HandedOverAt: handed,
			TrackingNumber: tracking, TrackingStatus: models.TrackingNone, ShippingName: name, ShippingCountry: "US"}
		if tracking != "" {
			o.TrackingStatus = models.TrackingInTransit
		}
		if err := db.Create(o).Error; err != nil {
			t.Fatalf("order: %v", err)
		}
		it := &models.OrderItem{OrderID: o.ID, InternalCode: code + "_1/1", SKUCode: "SKU", Quantity: 2, LineNo: 1}
		if err := db.Create(it).Error; err != nil {
			t.Fatalf("item: %v", err)
		}
		return o
	}
	handed := models.SellerStatusHandedOff
	// 20/11: 3 without tracking, 1 with. 21/11 23:00: 1 without (still in a
	// 20–21 range — last day counts whole). 19/11 and 22/11: outside.
	seed(handed, day(20, 9), "", "Alice")
	seed(handed, day(20, 10), "", "Bob")
	seed(handed, day(20, 23), "", "Carol")
	seed(models.SellerStatusShipped, day(20, 11), "TRK-OLD", "Dan")
	seed(handed, day(21, 23), "", "Eve")
	seed(handed, day(19, 23), "", "Out Before")
	seed(handed, day(22, 0), "", "Out After")
	seed(models.SellerStatusPacked, nil, "", "Not handed over")

	// The exact query the page sends for tab "Chưa có mã vận đơn" + 20/11–21/11.
	from := time.Date(2026, 11, 20, 0, 0, 0, 0, vn).UTC().Format(time.RFC3339)
	to := time.Date(2026, 11, 22, 0, 0, 0, 0, vn).UTC().Format(time.RFC3339)
	q := url.Values{}
	q.Set("handed_over", "true")
	q.Set("has_tracking", "false")
	q.Set("handed_over_from", from)
	q.Set("handed_over_to", to)

	list := h.list("/api/orders?" + q.Encode() + "&page=1&page_size=2") // one page of 2
	if list.Meta.Total != 4 || len(list.Data) != 2 {
		t.Fatalf("list: total %d rows %d, want 4 / 2", list.Meta.Total, len(list.Data))
	}

	w := h.get("/api/orders/tracking/export.xlsx?"+q.Encode(), models.RoleOps)
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "spreadsheetml") {
		t.Errorf("content-type %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".xlsx") {
		t.Errorf("content-disposition %q", cd)
	}
	f, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sheet := f.GetSheetList()[0]
	rows, err := f.GetRows(sheet)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	// Whole result (4), not the page of 2.
	if got := len(rows) - 1; int64(got) != list.Meta.Total {
		t.Fatalf("file has %d orders, list total %d — must be equal: %v", got, list.Meta.Total, rows)
	}
	wantHeader := "Mã đơn shop|Mã nội bộ|Seller|Số sản phẩm|Tên người nhận|Thành phố|Tỉnh/Bang|Quốc gia|" +
		"Xuất xưởng (giờ VN)|Mã vận đơn|Trạng thái|Trạng thái tracking|Chi tiết tracking|Vị trí tracking|Cập nhật tracking (giờ VN)"
	if got := strings.Join(rows[0], "|"); got != wantHeader {
		t.Fatalf("header:\n got %s\nwant %s", got, wantHeader)
	}
	names := []string{}
	for _, r := range rows[1:] {
		names = append(names, r[4])
		if r[2] != "Seller JX" || r[3] != "2" {
			t.Errorf("seller/qty wrong: %v", r)
		}
	}
	if strings.Join(names, ",") != "Eve,Carol,Bob,Alice" { // newest first, like the list
		t.Errorf("orders in file: %v", names)
	}
	// Carol left at 23:00 on 20/11 Vietnam time — the cell must say so.
	for _, r := range rows[1:] {
		if r[4] == "Carol" && r[8] != "20/11/2026 23:00" {
			t.Errorf("Carol handed over %q, want 20/11/2026 23:00 (giờ VN)", r[8])
		}
	}

	// Search box narrows the file exactly like the list.
	q2 := url.Values{}
	for k, v := range q {
		q2[k] = v
	}
	q2.Set("search", "bob")
	if w := h.get("/api/orders/tracking/export.xlsx?"+q2.Encode(), models.RoleOps); w.Code != http.StatusOK {
		t.Fatalf("export search: %d %s", w.Code, w.Body.String())
	} else {
		g, _ := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
		rr, _ := g.GetRows(g.GetSheetList()[0])
		if len(rr) != 2 || rr[1][4] != "Bob" {
			t.Errorf("search=bob exported %v", rr)
		}
	}

	// ---- Round trip: fill 2 of the 4, upload the same file. ----
	fill := map[string]string{"Alice": "9400111899223456789012", "Carol": "YT2521521266065831"}
	for i, r := range rows[1:] {
		if num, ok := fill[r[4]]; ok {
			if err := f.SetCellStr(sheet, fmt.Sprintf("J%d", i+2), num); err != nil {
				t.Fatalf("fill: %v", err)
			}
		}
	}
	filled, err := f.WriteToBuffer()
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "hanh-trinh-don.xlsx")
	_, _ = fw.Write(filled.Bytes())
	// The dialog sends the page's range too — the 2 unfilled orders must show up
	// as "file thiếu", the honest answer.
	_ = mw.WriteField("handed_over_from", from)
	_ = mw.WriteField("handed_over_to", to)
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/api/orders/tracking/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w = h.do(req, models.RoleCS)
	if w.Code != http.StatusOK {
		t.Fatalf("preview of the filled export: %d %s", w.Code, w.Body.String())
	}
	var preview struct {
		Data services.TrackingImportPreview `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatalf("preview body: %v", err)
	}
	s := preview.Data.Summary
	if s.TotalRows != 2 || s.Assign != 2 || s.Issues != 0 || s.SkippedBlank != 2 || s.ScopeTotal != 4 || s.ScopeMissing != 2 {
		t.Fatalf("preview summary %+v issues %+v", s, preview.Data.Issues)
	}

	assign := []map[string]interface{}{}
	for _, m := range preview.Data.Matches {
		assign = append(assign, map[string]interface{}{"order_id": m.OrderID, "tracking_number": m.TrackingNumber})
	}
	blob, _ := json.Marshal(map[string]interface{}{"assignments": assign})
	req = httptest.NewRequest("POST", "/api/orders/tracking/import/commit", bytes.NewReader(blob))
	req.Header.Set("Content-Type", "application/json")
	w = h.do(req, models.RoleCS)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"updated":2`) {
		t.Fatalf("commit: %d %s", w.Code, w.Body.String())
	}
	var got []models.Order
	db.Where("shipping_name IN ?", []string{"Alice", "Carol", "Bob"}).Find(&got)
	for _, o := range got {
		want := fill[o.ShippingName]
		if o.TrackingNumber != want {
			t.Errorf("%s: tracking %q, want %q", o.ShippingName, o.TrackingNumber, want)
		}
	}
	// The "Chưa có mã vận đơn" tab now holds the 2 not filled.
	if r := h.list("/api/orders?" + q.Encode()); r.Meta.Total != 2 {
		t.Errorf("after import, missing tab total %d, want 2", r.Meta.Total)
	}
}

// Reading the journey board is enough to export it; a role without the board
// gets the standard 403.
func TestJourneyExport_HTTP_Permission(t *testing.T) {
	h := newMultiHarness(t)
	req := httptest.NewRequest("GET", "/api/orders/tracking/export.xlsx?handed_over=true", nil)
	if w := h.do(req, models.RoleProduction); w.Code != http.StatusForbidden {
		t.Errorf("PRODUCTION (no journeys tick): %d, want 403", w.Code)
	}
	// CS has the board: allowed (empty result → 400 with a message, not 403).
	req = httptest.NewRequest("GET", "/api/orders/tracking/export.xlsx?handed_over=true", nil)
	if w := h.do(req, models.RoleCS); w.Code == http.StatusForbidden || w.Code == http.StatusOK {
		t.Errorf("CS on empty data: %d %s, want 400 'không có đơn'", w.Code, w.Body.String())
	} else if !strings.Contains(w.Body.String(), "Không có đơn") {
		t.Errorf("CS empty export message: %s", w.Body.String())
	}
}
