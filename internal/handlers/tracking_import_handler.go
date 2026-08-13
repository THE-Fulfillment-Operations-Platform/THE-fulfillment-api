package handlers

import (
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// formTimePtr parses an optional RFC3339 or date (YYYY-MM-DD) multipart form
// field — the PostForm twin of timeQueryPtr.
func formTimePtr(c *gin.Context, name string) *time.Time {
	s := c.PostForm(name)
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return &t
	}
	return nil
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
	fileHeader, err := c.FormFile("file")
	if err != nil {
		response.Fail(c, apperr.BadRequest(`Thiếu file upload (field "file")`))
		return
	}
	f, err := fileHeader.Open()
	if err != nil {
		response.Fail(c, apperr.BadRequest("Không mở được file upload"))
		return
	}
	defer f.Close()

	var rows []services.TrackingImportRow
	switch strings.ToLower(filepath.Ext(fileHeader.Filename)) {
	case ".xlsx", ".xlsm":
		rows, err = services.ParseTrackingXLSX(f)
	case ".xls":
		response.Fail(c, apperr.BadRequest("Định dạng .xls (Excel cũ) chưa hỗ trợ — lưu lại dạng .xlsx hoặc CSV"))
		return
	default:
		rows, err = services.ParseTrackingCSV(f)
	}
	if err != nil {
		response.Fail(c, err)
		return
	}

	preview, err := h.svc.Order.PreviewTrackingImport(actor(c), rows,
		formTimePtr(c, "handed_over_from"), formTimePtr(c, "handed_over_to"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, preview)
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
