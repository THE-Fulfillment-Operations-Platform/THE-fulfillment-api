package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// PackingScan scans a QC-passed item into its order's package. POST /api/packing/scan
func (h *Handlers) PackingScan(c *gin.Context) {
	var in services.PackingScanInput
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.Packing.Scan(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// GetOrderPackage returns the package state for an order. GET /api/packing/order/:id
func (h *Handlers) GetOrderPackage(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	pkg, err := h.svc.Packing.GetPackageForOrder(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, pkg)
}

// CreateHandoff records a THE handoff for a fully-packed order/package.
// POST /api/handoffs
func (h *Handlers) CreateHandoff(c *gin.Context) {
	var in services.HandoffInput
	if !bindJSON(c, &in) {
		return
	}
	handoff, err := h.svc.Packing.CreateHandoff(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, handoff)
}

// ListHandoffs lists handoffs. GET /api/handoffs
func (h *Handlers) ListHandoffs(c *gin.Context) {
	p := pageFrom(c)
	rows, total, err := h.svc.Packing.ListHandoffs(p)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

// MarkHandoffShipped records carrier + tracking and moves a handed-off parcel to
// SHIPPED. POST /api/handoffs/:id/ship
func (h *Handlers) MarkHandoffShipped(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.MarkShippedInput
	if !bindJSON(c, &in) {
		return
	}
	handoff, err := h.svc.Packing.MarkShipped(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, handoff)
}
