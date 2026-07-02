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
	var in services.MaterialInput
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
	var in services.SKUInput
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
