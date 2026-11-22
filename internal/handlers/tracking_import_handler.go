package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// formTimePtr parses an optional RFC3339 or date (YYYY-MM-DD) multipart form
// field — the PostForm twin of timeQueryPtr (ngày trơn = 00:00 giờ xưởng).
func formTimePtr(c *gin.Context, name string) *time.Time {
	return parseTimeBound(c.PostForm(name))
}

// DownloadTrackingImportTemplate streams the 2-column template (OrderID, Mã vận
// đơn) the CS desk fills to bulk-assign tracking numbers.
// GET /api/orders/tracking/import/template.xlsx
func (h *Handlers) DownloadTrackingImportTemplate(c *gin.Context) {
	data, filename, err := h.svc.Order.TrackingImportTemplateXLSX()
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", data)
}

// PreviewTrackingImport parses the uploaded tracking file and returns the match
// preview without writing anything. POST /api/orders/tracking/import
//
//	multipart/form-data: file=<xlsx|csv>,
//	                     handed_over_from=<RFC3339|YYYY-MM-DD> (optional),
//	                     handed_over_to=<RFC3339|YYYY-MM-DD>   (optional)
func (h *Handlers) PreviewTrackingImport(c *gin.Context) {
	src, _, f, ok := spreadsheetUpload(c)
	if !ok {
		return
	}
	defer f.Close()

	var file *services.TrackingImportFile
	var err error
	if src == "XLSX" {
		file, err = services.ParseTrackingXLSXFile(f)
	} else {
		file, err = services.ParseTrackingCSVFile(f)
	}
	if err != nil {
		response.Fail(c, err)
		return
	}

	preview, err := h.svc.Order.PreviewTrackingImport(actor(c), file.Rows,
		formTimePtr(c, "handed_over_from"), formTimePtr(c, "handed_over_to"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	preview.Summary.SkippedBlank = file.SkippedBlank
	response.OK(c, preview)
}

// DownloadJourneyExport streams every handed-over order matching the journey
// screen's filter as .xlsx — the same query parameters as GET /api/orders
// (has_tracking, handed_over_from/to, search…), without paging. The file is
// re-importable through POST /api/orders/tracking/import once the "Mã vận đơn"
// column is filled. GET /api/orders/tracking/export.xlsx
func (h *Handlers) DownloadJourneyExport(c *gin.Context) {
	data, filename, err := h.svc.Order.JourneyExportXLSX(orderFilterFrom(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", data)
}

// CommitTrackingImport applies the assignments the operator confirmed in the
// preview. POST /api/orders/tracking/import/commit
func (h *Handlers) CommitTrackingImport(c *gin.Context) {
	var in services.TrackingImportCommitInput
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Order.CommitTrackingImport(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}
