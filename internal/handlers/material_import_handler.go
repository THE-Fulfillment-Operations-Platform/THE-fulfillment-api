package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// MaterialImportPreview parses a material spreadsheet (`Loại VL` + `Dài (mm)` +
// `Rộng (mm)` + optional `Mô tả`) and returns the plan (create/update/no-change
// per material, plus bad rows). Nothing is written.
//
// POST /api/materials/import/preview
//
//	multipart/form-data: file=<csv|xlsx>
//	application/json:     { "filename": "x.csv", "rows": [ { "material": "...", "length_mm": 1220, "width_mm": 2440 } ] }
func (h *Handlers) MaterialImportPreview(c *gin.Context) {
	var (
		filename    string
		rows        []services.MaterialImportRow
		parseErrors []services.MaterialImportRowError
		notices     []string
	)

	if isMultipart(c) {
		src, name, f, ok := spreadsheetUpload(c)
		if !ok {
			return
		}
		defer f.Close()
		parsed, perrs, ns, err := services.ParseMaterialImportFile(src, f)
		if err != nil {
			response.Fail(c, err)
			return
		}
		filename, rows, parseErrors, notices = name, parsed, perrs, ns
	} else {
		var body struct {
			Filename string                       `json:"filename"`
			Rows     []services.MaterialImportRow `json:"rows" binding:"required,min=1"`
		}
		if !bindJSON(c, &body) {
			return
		}
		filename = body.Filename
		rows = body.Rows
	}

	pv, err := h.svc.Catalog.PreviewMaterialImport(filename, rows, parseErrors, notices)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, pv)
}

// MaterialImportCommit applies the material plan; the client sends the
// previewed rows back and they are re-analysed inside the transaction.
// POST /api/materials/import/commit
func (h *Handlers) MaterialImportCommit(c *gin.Context) {
	var body struct {
		Rows []services.MaterialImportRow `json:"rows" binding:"required,min=1"`
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

// DownloadMaterialTemplate streams the material import sample as an .xlsx.
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
