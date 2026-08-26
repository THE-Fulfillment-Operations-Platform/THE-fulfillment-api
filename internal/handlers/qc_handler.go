package handlers

import (
	"strings"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/repositories"
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

// QCFail records a mismatch, writes off the defective production part and sends
// the product back to be re-made — returning the note plus what was done, so the
// station can tell the operator where the item went. POST /api/qc/fail
func (h *Handlers) QCFail(c *gin.Context) {
	var in services.QCDecisionInput
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.QC.Fail(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, res)
}

// QCResults lists orders with the QC state of each product in them — the screen
// that answers "đơn nào QC xong, đơn nào chưa, kẹt ở sản phẩm nào".
// GET /api/qc/results?state=done|partial|none|rework&q=&page=&page_size=
func (h *Handlers) QCResults(c *gin.Context) {
	p := pageFrom(c)
	f := repositories.QCResultFilter{
		Page:     p,
		State:    c.Query("state"),
		Search:   strings.TrimSpace(c.Query("q")),
		SellerID: uintQueryPtr(c, "seller_id"),
		DateFrom: timeQueryPtr(c, "date_from"),
		DateTo:   timeQueryPtr(c, "date_to"),
		// handed_over=false → hàng đợi "chờ gửi cho THE"; true → màn hành trình.
		HandedOver: boolQueryPtr(c, "handed_over"),
	}
	rows, summary, total, err := h.svc.QC.QCResults(f)
	if err != nil {
		response.Fail(c, err)
		return
	}
	// The summary covers the WHOLE filtered set, so it ships next to the page
	// rather than inside it — the tiles must not change as the user pages.
	response.List(c, gin.H{"orders": rows, "summary": summary}, metaFor(p, total))
}

// QCUndoPass hạ kết luận "đã QC" của một sản phẩm về "đã cắt" — dành cho ca bấm
// nhầm, không phải ca hàng hỏng (hàng hỏng đi lối QC fail / huỷ batch).
// POST /api/qc/undo
func (h *Handlers) QCUndoPass(c *gin.Context) {
	var in services.UndoQCInput
	if !bindJSON(c, &in) {
		return
	}
	res, err := h.svc.QC.UndoPass(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}
