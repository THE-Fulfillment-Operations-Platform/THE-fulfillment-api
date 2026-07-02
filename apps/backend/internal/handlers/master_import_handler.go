package handlers

import (
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// MasterImportPreview parses the factory's legacy operational spreadsheet and
// returns the master-data plan (materials / SKUs / mappings to create, plus rows
// needing review or missing a material). Nothing is written yet.
//
// POST /api/master-data/import/preview
//
//	multipart/form-data: file=<csv|xlsx>
//	application/json:     { "filename": "x.csv", "rows": [ { "sku": "...", "material": "..." } ] }
func (h *Handlers) MasterImportPreview(c *gin.Context) {
	a := actor(c)
	contentType := c.ContentType()

	var (
		source   string
		filename string
		rows     []services.LegacyRow
	)

	if strings.HasPrefix(contentType, "multipart/form-data") {
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
		switch strings.ToLower(filepath.Ext(filename)) {
		case ".xlsx", ".xlsm":
			source = "XLSX"
		case ".xls":
			response.Fail(c, apperr.BadRequest("Định dạng .xls (Excel cũ) chưa hỗ trợ — lưu lại dạng .xlsx hoặc CSV"))
			return
		default:
			source = "CSV"
		}
		parsed, err := services.ParseLegacyFile(source, f)
		if err != nil {
			response.Fail(c, err)
			return
		}
		rows = parsed
	} else {
		var body struct {
			Filename string `json:"filename"`
			Rows     []struct {
				SKU      string `json:"sku"`
				Material string `json:"material"`
			} `json:"rows" binding:"required,min=1"`
		}
		if !bindJSON(c, &body) {
			return
		}
		source = "JSON"
		filename = body.Filename
		for i, r := range body.Rows {
			rows = append(rows, services.LegacyRow{RowNumber: i + 1, SKU: r.SKU, Material: r.Material})
		}
	}

	preview, err := h.svc.MasterImport.Preview(a, source, filename, rows)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, preview)
}

// MasterImportCommit applies a previously previewed master-data import job.
// POST /api/master-data/import/commit
func (h *Handlers) MasterImportCommit(c *gin.Context) {
	var body struct {
		ImportJobID uint `json:"import_job_id" binding:"required"`
	}
	if !bindJSON(c, &body) {
		return
	}
	res, err := h.svc.MasterImport.Commit(actor(c), body.ImportJobID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// ListMasterImportJobs lists master-data import jobs.
// GET /api/master-data/import-jobs
func (h *Handlers) ListMasterImportJobs(c *gin.Context) {
	p := pageFrom(c)
	rows, total, err := h.svc.MasterImport.List(p)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

// GetMasterImportJob returns a master-data import job's full plan.
// GET /api/master-data/import-jobs/:id
func (h *Handlers) GetMasterImportJob(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	job, err := h.svc.MasterImport.Get(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, job)
}
