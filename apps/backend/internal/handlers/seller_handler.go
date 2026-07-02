package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// ---------- Sellers ----------

func (h *Handlers) CreateSeller(c *gin.Context) {
	var in services.SellerInput
	if !bindJSON(c, &in) {
		return
	}
	s, err := h.svc.Seller.Create(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, s)
}

func (h *Handlers) ListSellers(c *gin.Context) {
	p := pageFrom(c)
	rows, total, err := h.svc.Seller.List(p)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

func (h *Handlers) GetSeller(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	s, err := h.svc.Seller.Get(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, s)
}

func (h *Handlers) UpdateSeller(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.SellerInput
	if !bindJSON(c, &in) {
		return
	}
	s, err := h.svc.Seller.Update(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, s)
}

func (h *Handlers) DeleteSeller(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if err := h.svc.Seller.Delete(actor(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"deleted": true})
}

// ---------- Stores ----------

func (h *Handlers) CreateStore(c *gin.Context) {
	var in services.StoreInput
	if !bindJSON(c, &in) {
		return
	}
	s, err := h.svc.Seller.CreateStore(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, s)
}

func (h *Handlers) ListStores(c *gin.Context) {
	p := pageFrom(c)
	rows, total, err := h.svc.Seller.ListStores(p, uintQueryPtr(c, "seller_id"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

func (h *Handlers) GetStore(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	s, err := h.svc.Seller.GetStore(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, s)
}

func (h *Handlers) UpdateStore(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.StoreInput
	if !bindJSON(c, &in) {
		return
	}
	s, err := h.svc.Seller.UpdateStore(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, s)
}

func (h *Handlers) DeleteStore(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if err := h.svc.Seller.DeleteStore(actor(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"deleted": true})
}
