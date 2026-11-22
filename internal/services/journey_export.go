package services

import (
	"fmt"
	"time"

	"github.com/xuri/excelize/v2"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Journey export ("Xuất Excel" on the Hành trình đơn hàng screen).
//
// The file is every order matching the screen's filter (tab + search box +
// handed-over date range), not just the visible page, so the factory can line
// up hand-over times and parcel states in a spreadsheet.
//
// It is ALSO a valid upload for "Gắn mã từ Excel": the operator types tracking
// numbers into the "Mã vận đơn" column of this very file and uploads it back.
// That works because
//   - "Mã nội bộ" and "Mã vận đơn" are headers the tracking parser recognises
//     (trackingInternalHeaderKeys / trackingNumberHeaderKeys), and the parser
//     prefers the internal code as the key — unique, unlike a store order id;
//   - every other header is outside the alias sets, so the parser ignores it;
//   - the "Xuất xưởng (giờ VN)" header is the marker that tells the parser this
//     is our export, so rows left without a number are skipped silently instead
//     of each becoming a "Thiếu mã vận đơn" issue;
//   - the "Mã vận đơn" column is formatted as Text, so a 22-digit USPS number
//     typed into it is not turned into 9.40011E+21 by Excel.
// journey_export_test.go pins all of this with an export → fill → re-import test.

// MaxJourneyExportRows caps one export. Tied to the import cap on purpose: a
// file the system hands out must always be accepted back by "Gắn mã từ Excel".
const MaxJourneyExportRows = MaxTrackingImportRows

// Column headers, in file order. Changing a header can break the round trip —
// run the journey export tests.
const (
	journeyColStoreOrder   = "Mã đơn shop"
	journeyColInternal     = "Mã nội bộ"
	journeyColSeller       = "Seller"
	journeyColQty          = "Số sản phẩm"
	journeyColRecipient    = "Tên người nhận"
	journeyColCity         = "Thành phố"
	journeyColProvince     = "Tỉnh/Bang"
	journeyColCountry      = "Quốc gia"
	journeyColHandedOver   = "Xuất xưởng (giờ VN)"
	journeyColTracking     = "Mã vận đơn"
	journeyColStatus       = "Trạng thái"
	journeyColTrackStatus  = "Trạng thái tracking"
	journeyColTrackDetail  = "Chi tiết tracking"
	journeyColTrackPlace   = "Vị trí tracking"
	journeyColTrackUpdated = "Cập nhật tracking (giờ VN)"
)

var journeyExportHeaders = []string{
	journeyColStoreOrder, journeyColInternal, journeyColSeller, journeyColQty,
	journeyColRecipient, journeyColCity, journeyColProvince, journeyColCountry,
	journeyColHandedOver, journeyColTracking, journeyColStatus,
	journeyColTrackStatus, journeyColTrackDetail, journeyColTrackPlace, journeyColTrackUpdated,
}

var journeyExportWidths = []float64{18, 14, 20, 11, 24, 16, 12, 10, 18, 30, 20, 20, 36, 22, 20}

// journeyExportMarkerKey is the normalised header that identifies the journey
// export to the tracking parser (see trackingFileFromRecords).
var journeyExportMarkerKey = normalizeTrackingHeader(journeyColHandedOver)

// journeyExcelDateTimeFmt is a real Excel date format: the cell holds a serial
// date-time, so the column filters by date and sorts chronologically.
const journeyExcelDateTimeFmt = "dd/mm/yyyy hh:mm"

// Status wording, copied from the web's utils/enums.ts (SELLER_STATUS,
// REVIEW_STATUS, TRACKING_STATUS) so the file reads exactly like the screen.
// Change one, change both.
var (
	journeySellerStatusLabel = map[models.SellerStatus]string{
		models.SellerStatusProduction: "Đang sản xuất",
		models.SellerStatusPacked:     "Đã đóng gói",
		models.SellerStatusHandedOff:  "Đã bàn giao",
		models.SellerStatusShipped:    "Đã gửi đi",
		models.SellerStatusDelivered:  "Đã giao",
	}
	journeyReviewStatusLabel = map[models.ReviewStatus]string{
		models.ReviewPending:   "Chờ duyệt",
		models.ReviewNeedsFix:  "Cần chỉnh sửa",
		models.ReviewApproved:  "Đã duyệt",
		models.ReviewRejected:  "Từ chối",
		models.ReviewCancelled: "Đã huỷ",
	}
	journeyTrackingStatusLabel = map[models.TrackingStatus]string{
		models.TrackingNone:           "Chưa có",
		models.TrackingPending:        "Chờ lấy hàng",
		models.TrackingPreTransit:     "Chuẩn bị vận chuyển",
		models.TrackingInTransit:      "Đang vận chuyển",
		models.TrackingOutForDelivery: "Đang giao",
		models.TrackingPickUp:         "Chờ nhận tại bưu cục",
		models.TrackingDelivered:      "Đã giao",
		models.TrackingUndelivered:    "Giao không thành công",
		models.TrackingException:      "Sự cố",
		models.TrackingExpired:        "Hết hạn theo dõi",
		models.TrackingCancelled:      "Đã huỷ vận chuyển",
	}
)

func labelOr[K ~string](m map[K]string, k K) string {
	if l, ok := m[k]; ok {
		return l
	}
	return string(k)
}

// journeyStatusLabel is the screen's "Trạng thái" column — the Go twin of
// orderStatusBadge in the web's utils/enums.ts. Every exported order is handed
// over, so the production-stage branch of that function never applies here.
func journeyStatusLabel(o *models.Order) string {
	if o.CancellationStatus == models.CancellationSeller || o.ReviewStatus == models.ReviewCancelled {
		return journeyReviewStatusLabel[models.ReviewCancelled]
	}
	if o.ReviewStatus != "" && o.ReviewStatus != models.ReviewApproved {
		return labelOr(journeyReviewStatusLabel, o.ReviewStatus)
	}
	if o.SellerStatus.HandedOver() && o.TrackingStatus != "" && o.TrackingStatus != models.TrackingNone {
		return labelOr(journeyTrackingStatusLabel, o.TrackingStatus)
	}
	return labelOr(journeySellerStatusLabel, o.SellerStatus)
}

// journeyItemQty is the screen's "N sp": the quantities of the order's live
// lines. ListOrders strips cancelled lines before the screen sees them; the
// export loads raw rows, so it filters the same way here.
func journeyItemQty(items []models.OrderItem) int {
	n := 0
	for _, it := range activeOrderItems(items) {
		n += it.Quantity
	}
	return n
}

// excelWallClock re-expresses t's wall clock in the business timezone as a UTC
// time. excelize turns a time.Time into a serial from its absolute instant
// (i.e. in UTC); without this, 14:05 in Hanoi would land in the cell as 07:05.
func excelWallClock(t time.Time) time.Time {
	v := t.In(AppLocation())
	return time.Date(v.Year(), v.Month(), v.Day(), v.Hour(), v.Minute(), v.Second(), 0, time.UTC)
}

// JourneyExportXLSX renders every handed-over order matching f (the journey
// screen's filter) into the re-importable spreadsheet described above. f.Page
// is ignored; HandedOver is forced — the journey screen lists nothing else.
func (s *OrderService) JourneyExportXLSX(f repositories.OrderFilter) ([]byte, string, error) {
	handedOver := true
	f.HandedOver = &handedOver
	f.Page = repositories.Page{}

	orders, total, err := s.repo.Order.ListAllCapped(f, MaxJourneyExportRows)
	if err != nil {
		return nil, "", apperr.Internal("could not load orders for export").Wrap(err)
	}
	if total > MaxJourneyExportRows || len(orders) > MaxJourneyExportRows {
		return nil, "", apperr.BadRequest(fmt.Sprintf(
			"Bộ lọc đang khớp %d đơn — mỗi lần xuất tối đa %d đơn. Thu hẹp khoảng ngày Xuất xưởng rồi xuất lại.",
			total, MaxJourneyExportRows))
	}
	if len(orders) == 0 {
		return nil, "", apperr.BadRequest("Không có đơn nào khớp bộ lọc để xuất")
	}

	data, err := buildJourneyExportXLSX(orders)
	if err != nil {
		return nil, "", err
	}
	return data, "hanh-trinh-don-" + time.Now().In(AppLocation()).Format("2006-01-02-1504") + ".xlsx", nil
}

func buildJourneyExportXLSX(orders []models.Order) ([]byte, error) {
	const sheet = "Hành trình đơn hàng"
	fail := func(err error) ([]byte, error) {
		return nil, apperr.Internal("could not build journey export").Wrap(err)
	}
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	if err := f.SetSheetName("Sheet1", sheet); err != nil {
		return fail(err)
	}

	colName := func(header string) string {
		for i, h := range journeyExportHeaders {
			if h == header {
				name, _ := excelize.ColumnNumberToName(i + 1)
				return name
			}
		}
		panic("journey export: unknown column " + header)
	}
	lastCol, _ := excelize.ColumnNumberToName(len(journeyExportHeaders))
	lastRow := len(orders) + 1

	// Text format on the WHOLE tracking column (not just the data cells), so a
	// number typed into any row stays a string. Set before the values: a column
	// style is the default for cells written afterwards.
	textStyle, err := f.NewStyle(&excelize.Style{NumFmt: 49}) // 49 = "@" (Text)
	if err != nil {
		return fail(err)
	}
	trackCol := colName(journeyColTracking)
	if err := f.SetColStyle(sheet, trackCol, textStyle); err != nil {
		return fail(err)
	}
	for i, w := range journeyExportWidths {
		name, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, name, name, w)
	}

	header := make([]interface{}, len(journeyExportHeaders))
	for i, h := range journeyExportHeaders {
		header[i] = h
	}
	if err := f.SetSheetRow(sheet, "A1", &header); err != nil {
		return fail(err)
	}

	dateCell := func(t *time.Time) interface{} {
		if t == nil || t.IsZero() {
			return nil
		}
		return excelWallClock(*t)
	}
	for i := range orders {
		o := &orders[i]
		seller := o.Seller.Name
		if seller == "" {
			seller = o.Seller.Code
		}
		values := []interface{}{
			o.StoreOrderID,
			o.InternalCode,
			seller,
			journeyItemQty(o.Items),
			o.ShippingName,
			o.ShippingCity,
			o.ShippingProvince,
			o.ShippingCountry,
			dateCell(o.HandedOverAt),
			o.TrackingNumber,
			journeyStatusLabel(o),
			labelOr(journeyTrackingStatusLabel, o.TrackingStatus),
			o.TrackingDetail,
			o.TrackingLocation,
			dateCell(o.TrackingUpdatedAt),
		}
		if o.TrackingStatus == "" {
			values[11] = journeyTrackingStatusLabel[models.TrackingNone]
		}
		cell, _ := excelize.CoordinatesToCellName(1, i+2)
		if err := f.SetSheetRow(sheet, cell, &values); err != nil {
			return fail(err)
		}
	}

	// Styles last, over the written values: excelize gives a time.Time cell its
	// own default date style, which this replaces with the dd/mm/yyyy one.
	dateFmt := journeyExcelDateTimeFmt
	dateStyle, err := f.NewStyle(&excelize.Style{CustomNumFmt: &dateFmt})
	if err != nil {
		return fail(err)
	}
	for _, h := range []string{journeyColHandedOver, journeyColTrackUpdated} {
		c := colName(h)
		if err := f.SetCellStyle(sheet, c+"2", c+itoa(lastRow), dateStyle); err != nil {
			return fail(err)
		}
	}
	// The column to fill gets a tinted header so the operator sees where to type.
	headStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "1F2A44"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"E9EDF5"}},
		Alignment: &excelize.Alignment{Vertical: "center"},
	})
	if err != nil {
		return fail(err)
	}
	fillHeadStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "7A4B00"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFE7A3"}},
		Alignment: &excelize.Alignment{Vertical: "center"},
		NumFmt:    49,
	})
	if err != nil {
		return fail(err)
	}
	_ = f.SetCellStyle(sheet, "A1", lastCol+"1", headStyle)
	_ = f.SetCellStyle(sheet, trackCol+"1", trackCol+"1", fillHeadStyle)
	_ = f.SetRowHeight(sheet, 1, 22)
	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})
	_ = f.AutoFilter(sheet, "A1:"+lastCol+itoa(lastRow), []excelize.AutoFilterOptions{})

	buf, err := f.WriteToBuffer()
	if err != nil {
		return fail(err)
	}
	return buf.Bytes(), nil
}
