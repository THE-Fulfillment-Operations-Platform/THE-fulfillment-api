package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// ListBaskets returns every waiting basket in use. GET /api/baskets
func (h *Handlers) ListBaskets(c *gin.Context) {
	res, err := h.svc.Basket.List()
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// BasketForOrder tells whether an order sits in a basket or needs one — shown
// next to the product at the QC station. GET /api/baskets/order/:id
func (h *Handlers) BasketForOrder(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	res, err := h.svc.Basket.ForOrder(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// LookupBasket resolves a scan at the basket screen (basket label, order code or
// product tem). POST /api/baskets/lookup
func (h *Handlers) LookupBasket(c *gin.Context) {
	var in struct {
		Code string `json:"code"`
	}
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Basket.Lookup(in.Code)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// CreateBasket puts an order into the lowest free basket (or returns the one it
// already has). POST /api/baskets
func (h *Handlers) CreateBasket(c *gin.Context) {
	var in struct {
		OrderID uint `json:"order_id" binding:"required"`
	}
	if !bindJSON(c, &in) {
		return
	}
	b, created, err := h.svc.Basket.Create(actor(c), in.OrderID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	res := gin.H{"basket": b, "created": created}
	if created {
		response.Created(c, res)
		return
	}
	response.OK(c, res)
}

// ReleaseBasket confirms the order was bundled and frees the basket number.
// POST /api/baskets/:id/release
func (h *Handlers) ReleaseBasket(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.ReleaseInput
	if c.Request.ContentLength > 0 && !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Basket.Release(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}
