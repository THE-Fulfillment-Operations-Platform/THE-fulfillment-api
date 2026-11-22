package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/services"
)

// Ngày trơn trong ?date_from / ?date_to / ?handed_over_* là ngày THEO GIỜ XƯỞNG
// (Asia/Ho_Chi_Minh), không phải 00:00 UTC (= 07:00 giờ VN, lỗi lệch 7 tiếng cũ).
func TestParseTimeBound(t *testing.T) {
	loc := services.AppLocation()
	eq := func(name string, got *time.Time, want time.Time) {
		t.Helper()
		if got == nil || !got.Equal(want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	eq("ngày trơn", parseTimeBound("2026-12-31"), time.Date(2026, 12, 31, 0, 0, 0, 0, loc))
	if loc.String() == "Asia/Ho_Chi_Minh" {
		eq("ngày trơn (UTC)", parseTimeBound("2026-12-31"), time.Date(2026, 12, 30, 17, 0, 0, 0, time.UTC))
		eq("đầu tháng", parseTimeBound("2026-12-01"), time.Date(2026, 11, 30, 17, 0, 0, 0, time.UTC))
	}
	eq("RFC3339 Z", parseTimeBound("2026-12-30T17:00:00Z"), time.Date(2026, 12, 30, 17, 0, 0, 0, time.UTC))
	eq("RFC3339 +07:00", parseTimeBound("2027-01-01T00:00:00+07:00"), time.Date(2026, 12, 31, 17, 0, 0, 0, time.UTC))
	eq("RFC3339 phần giây", parseTimeBound("2026-12-31T03:30:59.5Z"), time.Date(2026, 12, 31, 3, 30, 59, 500000000, time.UTC))
	for _, bad := range []string{"", "31/12/2026", "2026-02-30", "abc"} {
		if got := parseTimeBound(bad); got != nil {
			t.Errorf("%q = %v, want nil (không lọc)", bad, got)
		}
	}
}

// ?date_to của /api/items, /api/orders, /api/batches là cận trên LOẠI TRỪ
// (DateBefore, lọc <) — web gửi đầu ngày / đầu phút kế tiếp. Web cũ gửi ngày trơn
// của NGÀY KẾ TIẾP: giờ đọc theo giờ VN nên ra đúng [00:00 ngày đầu, 00:00 ngày kế).
func TestListFilters_DateToIsExclusive(t *testing.T) {
	loc := services.AppLocation()
	from := time.Date(2026, 12, 30, 17, 0, 0, 0, time.UTC)
	before := time.Date(2026, 12, 31, 17, 0, 0, 0, time.UTC)
	q := "date_from=2026-12-30T17:00:00Z&date_to=2026-12-31T17:00:00Z"

	it := itemFilterFrom(ctxWithQuery(q))
	if it.DateFrom == nil || !it.DateFrom.Equal(from) || it.DateBefore == nil || !it.DateBefore.Equal(before) {
		t.Errorf("items: from=%v before=%v", it.DateFrom, it.DateBefore)
	}
	b := batchFilterFrom(ctxWithQuery(q))
	if b.DateFrom == nil || !b.DateFrom.Equal(from) || b.DateBefore == nil || !b.DateBefore.Equal(before) {
		t.Errorf("batches: from=%v before=%v", b.DateFrom, b.DateBefore)
	}
	o := orderFilterFrom(ctxWithQuery(q))
	if o.DateFrom == nil || !o.DateFrom.Equal(from) || o.DateBefore == nil || !o.DateBefore.Equal(before) || o.DateTo != nil {
		t.Errorf("orders: from=%v before=%v to(<=)=%v", o.DateFrom, o.DateBefore, o.DateTo)
	}

	// Web cũ (trước bản sửa): ngày trơn, date_to là ngày KẾ TIẾP.
	old := itemFilterFrom(ctxWithQuery("date_from=2026-12-31&date_to=2027-01-01"))
	if old.DateFrom == nil || !old.DateFrom.Equal(time.Date(2026, 12, 31, 0, 0, 0, 0, loc)) ||
		old.DateBefore == nil || !old.DateBefore.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, loc)) {
		t.Errorf("web cũ ngày trơn: from=%v before=%v", old.DateFrom, old.DateBefore)
	}

	// Không gửi = không lọc.
	none := batchFilterFrom(ctxWithQuery("status=PENDING"))
	if none.DateFrom != nil || none.DateBefore != nil {
		t.Errorf("không gửi ngày mà vẫn lọc: %v %v", none.DateFrom, none.DateBefore)
	}
}

// formTimePtr (form multipart/urlencoded của "Gắn mã từ Excel") đọc y như timeQueryPtr.
func TestFormTimePtr_SameAsQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/x", strings.NewReader("handed_over_from=2026-12-31&handed_over_to=2026-12-31T17%3A00%3A00Z"))
	c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got, want := formTimePtr(c, "handed_over_from"), time.Date(2026, 12, 31, 0, 0, 0, 0, services.AppLocation()); got == nil || !got.Equal(want) {
		t.Errorf("handed_over_from = %v, want %v", got, want)
	}
	if got, want := formTimePtr(c, "handed_over_to"), time.Date(2026, 12, 31, 17, 0, 0, 0, time.UTC); got == nil || !got.Equal(want) {
		t.Errorf("handed_over_to = %v, want %v", got, want)
	}
}
