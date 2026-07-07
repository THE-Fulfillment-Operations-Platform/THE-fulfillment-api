package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
)

type resetInput struct {
	Scope string `json:"scope"`
}

// ResetData wipes order/production data so the catalog can be re-imported from
// scratch. It is OWNER-only and additionally gated by ALLOW_DATA_RESET at the
// route layer. The body is optional; the only supported scope is "transactional".
func (h *Handlers) ResetData(c *gin.Context) {
	var in resetInput
	// Body is optional — default to the transactional scope when absent/empty.
	_ = c.ShouldBindJSON(&in)
	if in.Scope != "" && in.Scope != "transactional" {
		response.Fail(c, badRequest("Unsupported reset scope: "+in.Scope))
		return
	}
	res, err := h.svc.Admin.ResetTransactional(actor(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}
