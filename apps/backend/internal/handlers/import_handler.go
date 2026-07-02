package handlers

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// ImportOrders accepts a seller's order file as JSON rows or a multipart CSV and
// validates it (preview). Pass commit=true to immediately commit valid rows.
//
// POST /api/orders/import
//
//	multipart/form-data: file=<csv>, seller_id=<id>, commit=<bool>
//	application/json:     { "seller_id": 1, "commit": false, "rows": [ {...} ] }
func (h *Handlers) ImportOrders(c *gin.Context) {
	contentType := c.ContentType()
	a := actor(c)

	var (
		sellerID uint
		commit   bool
		source   string
		filename string
		rows     []services.ImportRow
	)

	if strings.HasPrefix(contentType, "multipart/form-data") {
		sid, err := strconv.ParseUint(c.PostForm("seller_id"), 10, 64)
		if err != nil {
			response.Fail(c, apperr.BadRequest("seller_id form field is required"))
			return
		}
		sellerID = uint(sid)
		commit, _ = strconv.ParseBool(c.PostForm("commit"))

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
		var parsed []services.ImportRow
		switch strings.ToLower(filepath.Ext(filename)) {
		case ".xlsx", ".xlsm":
			parsed, err = services.ParseXLSX(f)
			source = "XLSX"
		case ".xls":
			response.Fail(c, apperr.BadRequest("Định dạng .xls (Excel cũ) chưa hỗ trợ — lưu lại dạng .xlsx hoặc CSV"))
			return
		default:
			parsed, err = services.ParseCSV(f)
			source = "CSV"
		}
		if err != nil {
			response.Fail(c, err)
			return
		}
		rows = parsed
	} else {
		var body struct {
			SellerID uint                 `json:"seller_id" binding:"required"`
			Commit   bool                 `json:"commit"`
			Filename string               `json:"filename"`
			Rows     []services.ImportRow `json:"rows" binding:"required,min=1"`
		}
		if !bindJSON(c, &body) {
			return
		}
		sellerID = body.SellerID
		commit = body.Commit
		rows = body.Rows
		source = "JSON"
		filename = body.Filename
	}

	preview, err := h.svc.Import.Preview(a, sellerID, source, filename, rows)
	if err != nil {
		response.Fail(c, err)
		return
	}

	if !commit {
		response.OK(c, preview)
		return
	}

	job, err := h.svc.Import.Commit(a, preview.ImportJobID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"preview": preview, "commit": job})
}

// CommitImport commits a previously previewed import job.
// POST /api/orders/import/commit
func (h *Handlers) CommitImport(c *gin.Context) {
	var body struct {
		ImportJobID uint `json:"import_job_id" binding:"required"`
	}
	if !bindJSON(c, &body) {
		return
	}
	job, err := h.svc.Import.Commit(actor(c), body.ImportJobID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, job)
}

// ListImportJobs lists import jobs. GET /api/import-jobs
func (h *Handlers) ListImportJobs(c *gin.Context) {
	p := pageFrom(c)
	rows, total, err := h.svc.Import.List(p)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

// GetImportJob fetches an import job with its errors. GET /api/import-jobs/:id
func (h *Handlers) GetImportJob(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	job, err := h.svc.Import.Get(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, job)
}
