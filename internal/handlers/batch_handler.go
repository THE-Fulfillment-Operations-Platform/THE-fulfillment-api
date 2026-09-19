package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// CreateBatch creates a production batch for one material. POST /api/batches
func (h *Handlers) CreateBatch(c *gin.Context) {
	var in services.CreateBatchInput
	if !bindJSON(c, &in) {
		return
	}
	batch, skipped, err := h.svc.Batch.Create(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, gin.H{"batch": batch, "skipped_item_ids": skipped})
}

// ListBatches lists batches with filters (material, status, priority, date).
// GET /api/batches
func (h *Handlers) ListBatches(c *gin.Context) {
	p := pageFrom(c)
	f := repositories.BatchFilter{
		Page:          p,
		MaterialID:    uintQueryPtr(c, "material_id"),
		Status:        c.Query("status"),
		Priority:      c.Query("priority"),
		DateFrom:      timeQueryPtr(c, "date_from"),
		DateTo:        timeQueryPtr(c, "date_to"),
		ParentBatchID: uintQueryPtr(c, "parent_batch_id"),
		// ?code= — mã nội bộ đơn ("100047") hoặc mã tem item ("100047_1/1"):
		// trả về (các) batch đang sản xuất đơn đó.
		Code: c.Query("code"),
		// ?open=1 — chỉ batch còn việc (bảng sản xuất dùng), bỏ batch đã đóng vì
		// toàn bộ hàng bị huỷ ở QC.
		ExcludeClosed: c.Query("open") == "1" || c.Query("open") == "true",
	}
	rows, total, err := h.svc.Batch.List(f)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

// GetBatch returns batch detail (items, SKU, mockup, files, status). GET /api/batches/:id
func (h *Handlers) GetBatch(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	// GetWithScrapHistory: batch đã đóng không còn phần nào sống, nên bản Get
	// thường trả về một batch trông như trống rỗng. Màn chi tiết của một tấm vừa
	// huỷ phải kể được nó đã làm ra những gì.
	b, err := h.svc.Batch.GetWithScrapHistory(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, b)
}

// ExportProductionTemplate streams a batch's legacy-compatible production
// template as an .xlsx download (columns split cleanly in Excel on any locale).
// GET /api/batches/:id/production-template.xlsx
func (h *Handlers) ExportProductionTemplate(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	data, filename, err := h.svc.Batch.ProductionTemplateXLSX(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", data)
}

// DownloadBatchAssetsZip streams a batch asset bundle as a ZIP download.
// GET /api/batches/:id/assets.zip
func (h *Handlers) DownloadBatchAssetsZip(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	batch, err := h.svc.Batch.GetWithScrapHistory(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	// assets=design → design-only bundle (front/back, no mockup/print/cut) named
	// per the INTERNALCODE_SKU_QUANTITY rule, in a Batch_<code>.zip. Anything else keeps the
	// full production bundle (backward compatible with the existing board button).
	designOnly := c.Query("assets") == "design"
	filename := "batch-" + strings.ReplaceAll(batch.Code, "#", "") + "-assets.zip"
	if designOnly {
		filename = services.BatchZipName(batch)
	}
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
	c.Header("Content-Type", "application/zip")
	if err := h.svc.Batch.StreamBatchAssetsZip(c.Request.Context(), c.Writer, id, designOnly); err != nil {
		failZipStream(c, err)
	}
}

// SetBatchLink attaches/updates a batch's print or cut link (shared by every
// design in the batch). PATCH /api/batches/:id/links
func (h *Handlers) SetBatchLink(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.SetBatchLinkInput
	if !bindJSON(c, &in) {
		return
	}
	link, err := h.svc.Batch.SetBatchLink(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, link)
}

// DeleteBatch removes a not-yet-produced batch (for a split parent: the whole
// child tree), releasing its items back to the batching pool. Batches already
// printed/cut/QC'd are refused — that is the scrap/close flow's territory.
// DELETE /api/batches/:id
func (h *Handlers) DeleteBatch(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if err := h.svc.Batch.Delete(actor(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"deleted": true})
}

// UpdateBatchStatus moves a batch through Pending/Đã in/Đã cắt/Đã QC.
// PATCH /api/batches/:id/status
func (h *Handlers) UpdateBatchStatus(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.UpdateStatusInput
	if !bindJSON(c, &in) {
		return
	}
	b, err := h.svc.Batch.UpdateStatus(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, b)
}

// ScrapBatch huỷ một batch đã sản xuất: ghi bỏ mọi phần còn sống kèm lý do,
// đóng batch và trả sản phẩm về hàng chờ làm lại. Ngược lại với DeleteBatch —
// cái đó chỉ chạy khi chưa ai đụng vào và xoá sạch dấu vết.
// POST /api/batches/:id/scrap
func (h *Handlers) ScrapBatch(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.ScrapBatchInput
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Batch.Scrap(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// AutoCreateBatches batches the whole design-ready pool in one deliberate
// action: the system groups by material, splits by quota and generates codes —
// nobody picks rows or types names. POST /api/batches/auto
func (h *Handlers) AutoCreateBatches(c *gin.Context) {
	res, err := h.svc.Batch.AutoCreateBatches(actor(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// SetBatchLinkPair saves a batch's print + cut links as one atomic package.
// PUT /api/batches/:id/links
func (h *Handlers) SetBatchLinkPair(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.SetBatchLinkPairInput
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Batch.SetBatchLinkPair(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// ExportBatchLinksXLSX streams the designer worksheet: one row per open PENDING
// batch, with immutable Batch ID + code + current links.
// GET /api/batches/links/export.xlsx
func (h *Handlers) ExportBatchLinksXLSX(c *gin.Context) {
	data, filename, err := h.svc.Batch.ExportBatchLinksXLSX()
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", data)
}

// PreviewBatchLinkImport parses the uploaded batch-links sheet and returns the
// dry-run comparison without writing anything.
// POST /api/batches/links/import/preview (multipart: file=<xlsx|xlsm|csv>)
func (h *Handlers) PreviewBatchLinkImport(c *gin.Context) {
	src, _, f, ok := spreadsheetUpload(c)
	if !ok {
		return
	}
	defer f.Close()

	var rows []services.BatchLinkImportRow
	var err error
	if src == "XLSX" {
		rows, err = services.ParseBatchLinkImportXLSX(f)
	} else {
		rows, err = services.ParseBatchLinkImportCSV(f)
	}
	if err != nil {
		response.Fail(c, err)
		return
	}
	preview, err := h.svc.Batch.PreviewBatchLinkImport(actor(c), rows)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, preview)
}

// CommitBatchLinkImport applies a previewed batch-links import as one
// all-or-nothing transaction. POST /api/batches/links/import/commit
func (h *Handlers) CommitBatchLinkImport(c *gin.Context) {
	var in services.BatchLinkImportCommitInput
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Batch.CommitBatchLinkImport(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}
