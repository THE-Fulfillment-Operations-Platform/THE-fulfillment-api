package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// DownloadSKUShippingExport — every SKU with its shipping declaration, to fill
// and upload back. GET /api/skus/shipping/export.xlsx
func (h *Handlers) DownloadSKUShippingExport(c *gin.Context) {
	data, filename, err := h.svc.Catalog.SKUShippingExportXLSX()
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", data)
}

// parseSKUShippingUpload reads the multipart "file" field into rows.
func parseSKUShippingUpload(c *gin.Context) ([]services.SKUShippingFileRow, []string, bool) {
	src, _, f, ok := spreadsheetUpload(c)
	if !ok {
		return nil, nil, false
	}
	defer f.Close()
	var (
		rows    []services.SKUShippingFileRow
		present []string
		err     error
	)
	if src == "XLSX" {
		rows, present, err = services.ParseSKUShippingXLSX(f)
	} else {
		rows, present, err = services.ParseSKUShippingCSV(f)
	}
	if err != nil {
		response.Fail(c, err)
		return nil, nil, false
	}
	return rows, present, true
}

// PreviewSKUShippingImport — dry run, nothing written.
// POST /api/skus/shipping/import/preview (multipart: file)
func (h *Handlers) PreviewSKUShippingImport(c *gin.Context) {
	rows, present, ok := parseSKUShippingUpload(c)
	if !ok {
		return
	}
	preview, err := h.svc.Catalog.PreviewSKUShippingImport(rows, present)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, preview)
}

// CommitSKUShippingImport — the same file again, written all-or-nothing.
// POST /api/skus/shipping/import/commit (multipart: file)
func (h *Handlers) CommitSKUShippingImport(c *gin.Context) {
	rows, present, ok := parseSKUShippingUpload(c)
	if !ok {
		return
	}
	res, err := h.svc.Catalog.CommitSKUShippingImport(actor(c), rows, present)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}
