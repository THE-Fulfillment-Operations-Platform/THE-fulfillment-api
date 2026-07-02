package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// QCScan pulls up item/order/sku/batch + mockup for QC comparison. POST /api/qc/scan
func (h *Handlers) QCScan(c *gin.Context) {
	var in services.ScanRef
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.QC.Scan(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// QCPass confirms the product matches the mockup. POST /api/qc/pass
func (h *Handlers) QCPass(c *gin.Context) {
	var in services.QCDecisionInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.svc.QC.Pass(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// QCFail records a mismatch and opens a rework note. POST /api/qc/fail
func (h *Handlers) QCFail(c *gin.Context) {
	var in services.QCDecisionInput
	if !bindJSON(c, &in) {
		return
	}
	note, err := h.svc.QC.Fail(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, note)
}
