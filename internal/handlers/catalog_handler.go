package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// ---------- Materials ----------

func (h *Handlers) CreateMaterial(c *gin.Context) {
	var in services.MaterialInput
	if !bindJSON(c, &in) {
		return
	}
	m, err := h.svc.Catalog.CreateMaterial(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, m)
}

func (h *Handlers) ListMaterials(c *gin.Context) {
	p := pageFrom(c)
	rows, total, err := h.svc.Catalog.ListMaterials(p)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

func (h *Handlers) GetMaterial(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	m, err := h.svc.Catalog.GetMaterial(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, m)
}

func (h *Handlers) UpdateMaterial(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.MaterialUpdateInput
	if !bindJSON(c, &in) {
		return
	}
	m, err := h.svc.Catalog.UpdateMaterial(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, m)
}

func (h *Handlers) DeleteMaterial(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if err := h.svc.Catalog.DeleteMaterial(actor(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"deleted": true})
}

// BulkDeleteMaterials removes many materials in one request. Deleting a selection
// id-at-a-time costs one HTTP round-trip and three statements per material, which
// on a hosted database is minutes for a few hundred rows; this is one round-trip
// and a handful of statements. Materials still used by a SKU or a batch come back
// in `skipped` with a reason instead of being deleted.
// POST /api/materials/bulk-delete  { "ids": [1,2,3] }
func (h *Handlers) BulkDeleteMaterials(c *gin.Context) {
	var in struct {
		IDs []uint `json:"ids" binding:"required,min=1"`
	}
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Catalog.DeleteMaterials(actor(c), in.IDs)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// ---------- SKUs ----------

func (h *Handlers) CreateSKU(c *gin.Context) {
	var in services.SKUInput
	if !bindJSON(c, &in) {
		return
	}
	s, err := h.svc.Catalog.CreateSKU(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, s)
}

func (h *Handlers) ListSKUs(c *gin.Context) {
	p := pageFrom(c)
	rows, total, err := h.svc.Catalog.ListSKUs(p)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

func (h *Handlers) GetSKU(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	s, err := h.svc.Catalog.GetSKU(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, s)
}

func (h *Handlers) UpdateSKU(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.SKUUpdateInput
	if !bindJSON(c, &in) {
		return
	}
	s, err := h.svc.Catalog.UpdateSKU(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, s)
}

func (h *Handlers) DeleteSKU(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if err := h.svc.Catalog.DeleteSKU(actor(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"deleted": true})
}

// BulkDeleteSKUs removes many SKUs in one request — see BulkDeleteMaterials for
// why an id-at-a-time API is the slow path. SKUs an order line points at come
// back in `skipped` with a reason instead of being deleted.
// POST /api/skus/bulk-delete  { "ids": [1,2,3] }
func (h *Handlers) BulkDeleteSKUs(c *gin.Context) {
	var in struct {
		IDs []uint `json:"ids" binding:"required,min=1"`
	}
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Catalog.DeleteSKUs(actor(c), in.IDs)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// BulkSetSKUsActive shows/hides many SKUs in one statement, instead of one full
// SKU save per row.
// POST /api/skus/bulk-active  { "ids": [1,2], "is_active": false }
func (h *Handlers) BulkSetSKUsActive(c *gin.Context) {
	var in struct {
		IDs      []uint `json:"ids" binding:"required,min=1"`
		IsActive *bool  `json:"is_active" binding:"required"`
	}
	if !bindJSON(c, &in) {
		return
	}
	n, err := h.svc.Catalog.SetSKUsActive(actor(c), in.IDs, *in.IsActive)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"updated": n})
}
