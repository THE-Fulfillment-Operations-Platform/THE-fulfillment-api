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

// MaterialImportPreview parses a 2-column material-quota spreadsheet (`Loại VL` +
// `Định mức`) and returns the plan (create/update/no-change per material, plus
// bad rows). Nothing is written. OWNER-only (see routes).
//
// POST /api/materials/import/preview
//
//	multipart/form-data: file=<csv|xlsx>
//	application/json:     { "filename": "x.csv", "rows": [ { "material": "...", "quota": 20 } ] }
func (h *Handlers) MaterialImportPreview(c *gin.Context) {
	var (
		filename    string
		rows        []services.MaterialQuotaRow
		parseErrors []services.MaterialImportRowError
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
		parsed, perrs, err := services.ParseMaterialQuotaFile(source, f)
		if err != nil {
			response.Fail(c, err)
			return
		}
		rows, parseErrors = parsed, perrs
	} else {
		var body struct {
			Filename string                      `json:"filename"`
			Rows     []services.MaterialQuotaRow `json:"rows" binding:"required,min=1"`
		}
		if !bindJSON(c, &body) {
			return
		}
		filename = body.Filename
		rows = body.Rows
	}

	response.OK(c, h.svc.Catalog.PreviewMaterialImport(filename, rows, parseErrors))
}

// MaterialImportCommit applies the material-quota plan. OWNER-only.
// POST /api/materials/import/commit
func (h *Handlers) MaterialImportCommit(c *gin.Context) {
	var body struct {
		Rows []services.MaterialQuotaRow `json:"rows" binding:"required,min=1"`
	}
	if !bindJSON(c, &body) {
		return
	}
	res, err := h.svc.Catalog.CommitMaterialImport(actor(c), body.Rows)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// DownloadMaterialTemplate streams the material-quota import sample as an .xlsx.
// GET /api/materials/import/template.xlsx
func (h *Handlers) DownloadMaterialTemplate(c *gin.Context) {
	data, filename, err := h.svc.Catalog.MaterialTemplateXLSX()
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", data)
}
