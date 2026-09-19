package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// MasterImportPreview parses the SKU spreadsheet (step 2 of the parent → child
// setup; also the factory's legacy operational file) and returns the plan:
// materials / SKUs / mappings to create, children to file under their parents,
// plus rows refused with a reason. Nothing is written yet.
//
// POST /api/master-data/import/preview
//
//	multipart/form-data: file=<csv|xlsx>
//	application/json:     { "filename": "x.csv", "rows": [ { "sku", "material", "parent_sku", "length", "width", … } ] }
func (h *Handlers) MasterImportPreview(c *gin.Context) {
	var (
		source   string
		filename string
		rows     []services.LegacyRow
	)
	if isMultipart(c) {
		src, name, f, ok := spreadsheetUpload(c)
		if !ok {
			return
		}
		defer f.Close()
		parsed, err := services.ParseLegacyFile(src, f)
		if err != nil {
			response.Fail(c, err)
			return
		}
		source, filename, rows = src, name, parsed
	} else {
		var body struct {
			Filename string               `json:"filename"`
			Rows     []services.LegacyRow `json:"rows" binding:"required,min=1"`
		}
		if !bindJSON(c, &body) {
			return
		}
		source, filename, rows = "JSON", body.Filename, body.Rows
		for i := range rows {
			if rows[i].RowNumber == 0 {
				rows[i].RowNumber = i + 1
			}
		}
	}

	preview, err := h.svc.MasterImport.Preview(actor(c), source, filename, rows)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, preview)
}

// DownloadMasterTemplate streams the SKU import sample as an .xlsx download.
// GET /api/master-data/template.xlsx
func (h *Handlers) DownloadMasterTemplate(c *gin.Context) {
	data, filename, err := h.svc.MasterImport.MasterTemplateXLSX()
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", data)
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

// ParentSKUImportPreview parses a step-1 file (SKU cha + Tên sản phẩm + Mô tả)
// and returns the plan — create / update / no change per parent SKU, plus bad
// rows. Nothing is written.
//
// POST /api/master-data/parents/import/preview
//
//	multipart/form-data: file=<csv|xlsx>
//	application/json:     { "filename": "x.csv", "rows": [ { "sku": "...", "product_name": "..." } ] }
func (h *Handlers) ParentSKUImportPreview(c *gin.Context) {
	var (
		filename string
		rows     []services.ParentSKURow
	)
	if isMultipart(c) {
		src, name, f, ok := spreadsheetUpload(c)
		if !ok {
			return
		}
		defer f.Close()
		parsed, err := services.ParseParentSKUFile(src, f)
		if err != nil {
			response.Fail(c, err)
			return
		}
		filename, rows = name, parsed
	} else {
		var body struct {
			Filename string                  `json:"filename"`
			Rows     []services.ParentSKURow `json:"rows" binding:"required,min=1"`
		}
		if !bindJSON(c, &body) {
			return
		}
		filename, rows = body.Filename, body.Rows
	}

	pv, err := h.svc.MasterImport.PreviewParents(filename, rows)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, pv)
}

// ParentSKUImportCommit applies the step-1 plan; the client sends the previewed
// rows back and they are re-analysed inside the transaction.
// POST /api/master-data/parents/import/commit
func (h *Handlers) ParentSKUImportCommit(c *gin.Context) {
	var body struct {
		Rows []services.ParentSKURow `json:"rows" binding:"required,min=1"`
	}
	if !bindJSON(c, &body) {
		return
	}
	res, err := h.svc.MasterImport.CommitParents(actor(c), body.Rows)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// DownloadParentSKUTemplate streams the step-1 sample as an .xlsx.
// GET /api/master-data/parents/template.xlsx
func (h *Handlers) DownloadParentSKUTemplate(c *gin.Context) {
	data, filename, err := h.svc.MasterImport.ParentTemplateXLSX()
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", data)
}
