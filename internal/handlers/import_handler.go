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

// parseImportUpload extracts the import rows / source / filename / commit flag
// from a multipart CSV/XLSX upload or a JSON body. It also returns any seller_id
// present in the request; the caller decides whether to trust it (Ops importing
// on behalf of a seller) or ignore it in favour of the authenticated seller
// (seller self-upload). On error it writes the response and returns ok=false.
func parseImportUpload(c *gin.Context) (rows []services.ImportRow, source, filename string, commit bool, bodySellerID uint, ok bool) {
	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		if sid, err := strconv.ParseUint(c.PostForm("seller_id"), 10, 64); err == nil {
			bodySellerID = uint(sid)
		}
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
		switch strings.ToLower(filepath.Ext(filename)) {
		case ".xlsx", ".xlsm":
			rows, err = services.ParseXLSX(f)
			source = "XLSX"
		case ".xls":
			response.Fail(c, apperr.BadRequest("Định dạng .xls (Excel cũ) chưa hỗ trợ — lưu lại dạng .xlsx hoặc CSV"))
			return
		default:
			rows, err = services.ParseCSV(f)
			source = "CSV"
		}
		if err != nil {
			response.Fail(c, err)
			return
		}
	} else {
		var body struct {
			SellerID uint                 `json:"seller_id"`
			Commit   bool                 `json:"commit"`
			Filename string               `json:"filename"`
			Rows     []services.ImportRow `json:"rows" binding:"required,min=1"`
		}
		if !bindJSON(c, &body) {
			return
		}
		bodySellerID = body.SellerID
		commit = body.Commit
		rows = body.Rows
		source = "JSON"
		filename = body.Filename
	}
	ok = true
	return
}

// runImport previews (and optionally commits) an import for a resolved sellerID.
func (h *Handlers) runImport(c *gin.Context, sellerID uint, rows []services.ImportRow, source, filename string, commit bool) {
	a := actor(c)
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

// ImportOrders (Ops/Admin) imports a seller's order file. seller_id comes from
// the request so ops can import on behalf of any seller. POST /api/orders/import
//
//	multipart/form-data: file=<csv>, seller_id=<id>, commit=<bool>
//	application/json:     { "seller_id": 1, "commit": false, "rows": [ {...} ] }
func (h *Handlers) ImportOrders(c *gin.Context) {
	rows, source, filename, commit, bodySellerID, ok := parseImportUpload(c)
	if !ok {
		return
	}
	if bodySellerID == 0 {
		response.Fail(c, apperr.BadRequest("seller_id is required"))
		return
	}
	h.runImport(c, bodySellerID, rows, source, filename, commit)
}

// SellerImportOrders lets a seller upload their OWN order file. seller_id is
// always the authenticated seller — never taken from the request — so a seller
// can only import into their own account. Imported orders still land in
// PENDING_REVIEW and must be approved by Ops before production.
// POST /api/seller/orders/import
func (h *Handlers) SellerImportOrders(c *gin.Context) {
	sellerID, ok := sellerIDFrom(c)
	if !ok {
		return
	}
	rows, source, filename, commit, _, ok := parseImportUpload(c)
	if !ok {
		return
	}
	h.runImport(c, sellerID, rows, source, filename, commit)
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

// SellerCommitImport commits a seller's OWN previewed import job, after checking
// the job belongs to the authenticated seller. POST /api/seller/orders/import/commit
func (h *Handlers) SellerCommitImport(c *gin.Context) {
	sellerID, ok := sellerIDFrom(c)
	if !ok {
		return
	}
	var body struct {
		ImportJobID uint `json:"import_job_id" binding:"required"`
	}
	if !bindJSON(c, &body) {
		return
	}
	job, err := h.svc.Import.Get(body.ImportJobID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if job.SellerID == nil || *job.SellerID != sellerID {
		response.Fail(c, apperr.Forbidden("Import job does not belong to your seller account"))
		return
	}
	committed, err := h.svc.Import.Commit(actor(c), body.ImportJobID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, committed)
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
