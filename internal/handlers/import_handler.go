package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// sellerModeByColumn asks an ops import to assign each row to the seller its
// "Seller ID" column names, instead of importing the whole file for seller_id.
const sellerModeByColumn = "by_column"

// importUpload is what parseImportUpload read from the request.
type importUpload struct {
	rows     []services.ImportRow
	hdr      services.HeaderReport
	source   string
	filename string
	commit   bool
	// sellerID / sellerMode are what the request ASKED for; the caller decides
	// whether to trust them (Ops importing on behalf of sellers) or ignore them
	// in favour of the authenticated seller (seller self-upload).
	sellerID   uint
	sellerMode string
}

// parseImportUpload extracts the import rows / source / filename / commit flag
// from a multipart CSV/XLSX upload or a JSON body. On error it writes the
// response and returns ok=false.
func parseImportUpload(c *gin.Context) (up importUpload, ok bool) {
	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		if sid, err := strconv.ParseUint(c.PostForm("seller_id"), 10, 64); err == nil {
			up.sellerID = uint(sid)
		}
		up.sellerMode = strings.TrimSpace(c.PostForm("seller_mode"))
		up.commit, _ = strconv.ParseBool(c.PostForm("commit"))

		src, name, f, opened := spreadsheetUpload(c)
		if !opened {
			return
		}
		defer f.Close()
		up.source, up.filename = src, name
		var err error
		if src == "XLSX" {
			up.rows, up.hdr, err = services.ParseXLSX(f)
		} else {
			up.rows, up.hdr, err = services.ParseCSV(f)
		}
		if err != nil {
			response.Fail(c, err)
			return
		}
	} else {
		var body struct {
			SellerID   uint                 `json:"seller_id"`
			SellerMode string               `json:"seller_mode"`
			Commit     bool                 `json:"commit"`
			Filename   string               `json:"filename"`
			Rows       []services.ImportRow `json:"rows" binding:"required,min=1"`
		}
		if !bindJSON(c, &body) {
			return
		}
		up.sellerID = body.SellerID
		up.sellerMode = strings.TrimSpace(body.SellerMode)
		up.commit = body.Commit
		up.rows = body.Rows
		up.source = "JSON"
		up.filename = body.Filename
		// A JSON body carries objects, not a header row: the client already did the
		// column mapping. Nothing to report — and nothing to declare missing, or
		// pasting rows through the API would fail the required-column check.
	}
	ok = true
	return
}

// runImport previews (and optionally commits) an import for a resolved sellerID.
func (h *Handlers) runImport(c *gin.Context, sellerID uint, up importUpload) {
	a := actor(c)
	preview, err := h.svc.Import.Preview(a, sellerID, up.source, up.filename, up.rows, up.hdr)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if !up.commit {
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
// the request so ops can import on behalf of any seller — or, with
// seller_mode=by_column, each row goes to the seller its "Seller ID" column
// names (one PREVIEW job per seller, committed job by job). POST /api/orders/import
//
//	multipart/form-data: file=<csv>, seller_id=<id> | seller_mode=by_column, commit=<bool>
//	application/json:     { "seller_id": 1, "commit": false, "rows": [ {...} ] }
func (h *Handlers) ImportOrders(c *gin.Context) {
	up, ok := parseImportUpload(c)
	if !ok {
		return
	}
	if up.sellerMode == sellerModeByColumn {
		// Preview only: one file becomes several jobs, and committing them in one
		// call would half-succeed on a failure mid-way with no clean way back.
		if up.commit {
			response.Fail(c, apperr.BadRequest("seller_mode=by_column chỉ preview; commit từng import_job_id trong sellers[]"))
			return
		}
		res, err := h.svc.Import.PreviewBySellerColumn(actor(c), up.source, up.filename, up.rows, up.hdr)
		if err != nil {
			response.Fail(c, err)
			return
		}
		response.OK(c, res)
		return
	}
	if up.sellerID == 0 {
		response.Fail(c, apperr.BadRequest("seller_id is required"))
		return
	}
	h.runImport(c, up.sellerID, up)
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
	up, ok := parseImportUpload(c)
	if !ok {
		return
	}
	h.runImport(c, sellerID, up)
}

// DownloadOrderImportTemplate streams the order-import template as an .xlsx
// download (all columns split cleanly in Excel on any locale, unlike a comma
// CSV). Shared by the ops import screen and the seller self-upload screen.
// GET /api/orders/import/template.xlsx and /api/seller/orders/import/template.xlsx
func (h *Handlers) DownloadOrderImportTemplate(c *gin.Context) {
	data, filename, err := h.svc.Import.OrderImportTemplateXLSX()
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", data)
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
