package handlers

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// PairQuotaImportPreview parses a SKU + Loại VL + Định mức sheet and returns the
// per-pair plan. Nothing is written. OWNER-only (see routes).
//
// POST /api/materials/pair-quota/import/preview
//
//	multipart/form-data: file=<csv|xlsx>
//	application/json:     { "filename": "x.xlsx", "rows": [ { "sku": "...", "material": "...", "quota": 12 } ] }
func (h *Handlers) PairQuotaImportPreview(c *gin.Context) {
	var (
		filename    string
		rows        []services.PairQuotaFileRow
		parseErrors []services.PairQuotaRowError
	)
	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		fileHeader, err := c.FormFile("file")
		if err != nil {
			response.Fail(c, apperr.BadRequest("file form field is required"))
			return
		}
		f, err := fileHeader.Open()
		if err != nil {
			response.Fail(c, apperr.BadRequest("could not open uploaded file"))
			return
		}
		defer f.Close()

		filename = fileHeader.Filename
		source := "CSV"
		switch strings.ToLower(filepath.Ext(filename)) {
		case ".xlsx", ".xlsm":
			source = "XLSX"
		case ".xls":
			response.Fail(c, apperr.BadRequest("Định dạng .xls (Excel cũ) chưa hỗ trợ — lưu lại dạng .xlsx hoặc CSV"))
			return
		}
		parsed, perrs, err := services.ParsePairQuotaFile(source, f)
		if err != nil {
			response.Fail(c, err)
			return
		}
		rows, parseErrors = parsed, perrs
	} else {
		var body struct {
			Filename string                      `json:"filename"`
			Rows     []services.PairQuotaFileRow `json:"rows" binding:"required,min=1"`
		}
		if !bindJSON(c, &body) {
			return
		}
		filename = body.Filename
		rows = body.Rows
	}
	pv, err := h.svc.Catalog.PreviewPairQuotaImport(filename, rows, parseErrors)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, pv)
}

// PairQuotaImportCommit writes the pair quotas. OWNER-only.
// POST /api/materials/pair-quota/import/commit
func (h *Handlers) PairQuotaImportCommit(c *gin.Context) {
	var body struct {
		Rows []services.PairQuotaFileRow `json:"rows" binding:"required,min=1"`
	}
	if !bindJSON(c, &body) {
		return
	}
	res, err := h.svc.Catalog.CommitPairQuotaImport(actor(c), body.Rows)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// ExportPairQuotas streams every (SKU, NVL) pair with its current quota — the
// sheet to fill in and import back. GET /api/materials/pair-quota/export.xlsx
func (h *Handlers) ExportPairQuotas(c *gin.Context) {
	data, filename, err := h.svc.Catalog.ExportPairQuotasXLSX()
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", data)
}
