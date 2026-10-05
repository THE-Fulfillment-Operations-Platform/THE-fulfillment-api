package handlers

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// THE calls must finish even if the operator closes the tab mid-send: a
// delivery aborted half way is exactly the "paid or not?" state the shipment
// flow works hard to avoid. Each call still has its own timeout.
func detached(c *gin.Context) context.Context {
	return context.WithoutCancel(c.Request.Context())
}

// GET /api/carrier/the/config
func (h *Handlers) GetCarrierConfig(c *gin.Context) {
	res, err := h.svc.Carrier.GetConfig(actor(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// PUT /api/carrier/the/config
func (h *Handlers) UpdateCarrierConfig(c *gin.Context) {
	var in services.CarrierConfigInput
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Carrier.UpdateConfig(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// POST /api/carrier/the/check — read-only: wallet + services.
func (h *Handlers) CheckCarrierConnection(c *gin.Context) {
	res, err := h.svc.Carrier.CheckConnection(c.Request.Context(), actor(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// POST /api/orders/the/preflight { order_ids } — what a send would do, before
// any money moves.
func (h *Handlers) PreflightTHE(c *gin.Context) {
	var in struct {
		OrderIDs []uint `json:"order_ids" binding:"required,min=1"`
	}
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Carrier.PreflightTHE(c.Request.Context(), actor(c), in.OrderIDs)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// GET /api/orders/:id/the/shipments
func (h *Handlers) OrderTHEShipments(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	res, err := h.svc.Carrier.ShipmentsForOrder(actor(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// GET /api/orders/:id/the/label — the label file (PDF/PNG/GIF) to print.
func (h *Handlers) OrderTHELabel(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	f, err := h.svc.Carrier.Label(c.Request.Context(), actor(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", `inline; filename="`+f.Filename+`"`)
	c.Header("Cache-Control", "private, max-age=300")
	ctype := f.ContentType
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	c.Data(http.StatusOK, ctype, f.Data)
}

// POST /api/orders/:id/the/cancel { reason }
func (h *Handlers) CancelOrderTHEShipment(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Carrier.CancelShipment(detached(c), actor(c), id, in.Reason)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}
