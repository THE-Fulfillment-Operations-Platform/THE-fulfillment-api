package handlers

import (
	"net/http"

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
		Page:       p,
		MaterialID: uintQueryPtr(c, "material_id"),
		Status:     c.Query("status"),
		Priority:   c.Query("priority"),
		DateFrom:   timeQueryPtr(c, "date_from"),
		DateTo:     timeQueryPtr(c, "date_to"),
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
	b, err := h.svc.Batch.Get(id)
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
