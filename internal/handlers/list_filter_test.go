package handlers

import (
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
)

func ctxWithQuery(rawQuery string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/x?"+rawQuery, nil)
	return c
}

// Ô lọc chọn nhiều gửi ?status=A,B hoặc ?status=A&status=B (hai kiểu trộn được);
// client cũ gửi một giá trị — phải ra đúng bộ lọc như trước.
func TestStringListQuery(t *testing.T) {
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"", nil},
		{"status=", nil},
		{"status=PENDING", []string{"PENDING"}},
		{"status=FILES_MISSING,FILES_READY,PRINTED", []string{"FILES_MISSING", "FILES_READY", "PRINTED"}},
		{"status=FILES_MISSING&status=PRINTED", []string{"FILES_MISSING", "PRINTED"}},
		{"status=A,B&status=C&status=A&status=%20,,", []string{"A", "B", "C"}},
		{"status=%20HANDED_OVER%20", []string{"HANDED_OVER"}},
	} {
		if got := stringListQuery(ctxWithQuery(tc.q), "status"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("?%s → %#v, want %#v", tc.q, got, tc.want)
		}
	}
}

func TestUintListQuery(t *testing.T) {
	for _, tc := range []struct {
		q    string
		want []uint
	}{
		{"", nil},
		{"seller_id=", nil},
		{"seller_id=abc", nil}, // như uintQueryPtr cũ: không phải số = không lọc
		{"seller_id=5", []uint{5}},
		{"seller_id=3,7", []uint{3, 7}},
		{"seller_id=3&seller_id=7&seller_id=3", []uint{3, 7}},
		{"seller_id=3,x,-1,7", []uint{3, 7}},
	} {
		if got := uintListQuery(ctxWithQuery(tc.q), "seller_id"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("?%s → %#v, want %#v", tc.q, got, tc.want)
		}
	}
}

func TestBatchFilterFrom_LegacyAndMulti(t *testing.T) {
	// Tham số đơn kiểu cũ (web bản trước, bảng sản xuất, dashboard).
	f := batchFilterFrom(ctxWithQuery("status=PENDING&files=missing&seller_id=5&page=2&page_size=20"))
	if !reflect.DeepEqual(f.Statuses, []string{"PENDING"}) || f.Files != "missing" || f.Handover != "" {
		t.Errorf("legacy status: %+v", f)
	}
	if !reflect.DeepEqual(f.SellerIDs, []uint{5}) || f.SellerID != nil {
		t.Errorf("legacy seller: SellerIDs=%v SellerID=%v", f.SellerIDs, f.SellerID)
	}
	if f.Page.Page != 2 || f.Page.PageSize != 20 {
		t.Errorf("page: %+v", f.Page)
	}
	f = batchFilterFrom(ctxWithQuery("status=QC_PASSED&handover=done"))
	if !reflect.DeepEqual(f.Statuses, []string{"QC_PASSED"}) || f.Handover != "done" {
		t.Errorf("legacy handover: %+v", f)
	}

	// Chọn nhiều.
	f = batchFilterFrom(ctxWithQuery("status=FILES_MISSING,FILES_READY,PRINTED&seller_id=1,2"))
	if !reflect.DeepEqual(f.Statuses, []string{"FILES_MISSING", "FILES_READY", "PRINTED"}) {
		t.Errorf("multi status: %v", f.Statuses)
	}
	if !reflect.DeepEqual(f.SellerIDs, []uint{1, 2}) {
		t.Errorf("multi seller: %v", f.SellerIDs)
	}
	if f.Files != "" || f.Handover != "" {
		t.Errorf("multi must not invent files/handover: %+v", f)
	}
}

func TestItemFilterFrom_LegacyAndMulti(t *testing.T) {
	f := itemFilterFrom(ctxWithQuery("status=PENDING&files=ready&seller_id=4"))
	if !reflect.DeepEqual(f.InternalStatuses, []string{"PENDING"}) || f.Files != "ready" {
		t.Errorf("legacy files: %+v", f.InternalStatuses)
	}
	if !reflect.DeepEqual(f.SellerIDs, []uint{4}) || f.SellerID != nil {
		t.Errorf("legacy seller: %v", f.SellerIDs)
	}
	f = itemFilterFrom(ctxWithQuery("handed_over=true"))
	if !f.HandedOver || len(f.InternalStatuses) != 0 {
		t.Errorf("legacy handed_over: %+v / %v", f.HandedOver, f.InternalStatuses)
	}
	f = itemFilterFrom(ctxWithQuery("packed=1"))
	if !f.Packed {
		t.Errorf("legacy packed lost")
	}

	f = itemFilterFrom(ctxWithQuery("status=FILES_MISSING&status=QC_PASSED&status=PACKED,HANDED_OVER&seller_id=2&seller_id=9"))
	if want := []string{"FILES_MISSING", "QC_PASSED", "PACKED", "HANDED_OVER"}; !reflect.DeepEqual(f.InternalStatuses, want) {
		t.Errorf("multi status: %v", f.InternalStatuses)
	}
	if !reflect.DeepEqual(f.SellerIDs, []uint{2, 9}) {
		t.Errorf("multi seller: %v", f.SellerIDs)
	}
	if f.HandedOver || f.Packed || f.Files != "" {
		t.Errorf("multi must not set legacy flags: %+v", f)
	}
}
