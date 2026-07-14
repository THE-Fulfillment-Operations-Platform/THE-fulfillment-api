package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/middleware"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/response"
)

// noteBody is the shared body for review/cancellation actions that carry a note.
type noteBody struct {
	Note   string `json:"note"`
	Reason string `json:"reason"`
}

// bindNote binds the optional note/reason body without failing on an empty body.
func bindNote(c *gin.Context) noteBody {
	var b noteBody
	_ = c.ShouldBindJSON(&b)
	return b
}

// ---------- Review queue (OPS / ADMIN / DESIGNER) ----------

// ListReviewOrders lists orders awaiting operational review. GET /api/review/orders
func (h *Handlers) ListReviewOrders(c *gin.Context) {
	p := pageFrom(c)
	f := repositories.OrderFilter{
		Page:         p,
		SellerID:     uintQueryPtr(c, "seller_id"),
		ReviewStatus: c.Query("status"),
		StoreOrderID: c.Query("store_order_id"),
	}
	rows, total, err := h.svc.Review.ListReviewOrders(f)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(f.Page, total))
}

// GetReviewOrder returns a full order plus its validation issues.
// GET /api/review/orders/:id
func (h *Handlers) GetReviewOrder(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	detail, err := h.svc.Review.GetReviewOrder(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, detail)
}

// ApproveReviewOrder approves an order. POST /api/review/orders/:id/approve
func (h *Handlers) ApproveReviewOrder(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	o, err := h.svc.Review.Approve(actor(c), id, bindNote(c).Note)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}

// RejectReviewOrder rejects an order. POST /api/review/orders/:id/reject
func (h *Handlers) RejectReviewOrder(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	o, err := h.svc.Review.Reject(actor(c), id, bindNote(c).Note)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}

// RequestReviewCorrection sends an order back for correction.
// POST /api/review/orders/:id/request-correction
func (h *Handlers) RequestReviewCorrection(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	o, err := h.svc.Review.RequestCorrection(actor(c), id, bindNote(c).Note)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}

// ---------- Seller cancellation (SELLER, own orders only) ----------

func sellerIDFrom(c *gin.Context) (uint, bool) {
	claims := middleware.CurrentClaims(c)
	if claims == nil || claims.SellerID == nil {
		response.AbortForbidden(c, "Seller account required")
		return 0, false
	}
	return *claims.SellerID, true
}

// SellerCancelOrder directly cancels a pending-review order.
// POST /api/seller/orders/:id/cancel
func (h *Handlers) SellerCancelOrder(c *gin.Context) {
	sellerID, ok := sellerIDFrom(c)
	if !ok {
		return
	}
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	o, err := h.svc.Review.SellerCancel(actor(c), sellerID, id, bindNote(c).Reason)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}

// SellerRequestCancellation submits a cancellation request for an approved order.
// POST /api/seller/orders/:id/cancellation-request
func (h *Handlers) SellerRequestCancellation(c *gin.Context) {
	sellerID, ok := sellerIDFrom(c)
	if !ok {
		return
	}
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	o, err := h.svc.Review.SellerRequestCancellation(actor(c), sellerID, id, bindNote(c).Reason)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}

func (h *Handlers) SellerCancelOrderItem(c *gin.Context) {
	sellerID, ok := sellerIDFrom(c)
	if !ok {
		return
	}
	orderID, ok := uintParam(c, "id")
	if !ok {
		return
	}
	itemID, ok := uintParam(c, "item_id")
	if !ok {
		return
	}
	o, err := h.svc.Review.SellerCancelItem(actor(c), sellerID, orderID, itemID, bindNote(c).Reason)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}

func (h *Handlers) SellerRequestItemCancellation(c *gin.Context) {
	sellerID, ok := sellerIDFrom(c)
	if !ok {
		return
	}
	orderID, ok := uintParam(c, "id")
	if !ok {
		return
	}
	itemID, ok := uintParam(c, "item_id")
	if !ok {
		return
	}
	o, err := h.svc.Review.SellerRequestItemCancellation(actor(c), sellerID, orderID, itemID, bindNote(c).Reason)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}

// ---------- Cancellation requests (OPS / ADMIN) ----------

func (h *Handlers) ListItemCancellationRequests(c *gin.Context) {
	p := pageFrom(c)
	rows, total, err := h.svc.Review.ListItemCancellationRequests(p)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

func (h *Handlers) ApproveItemCancellation(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	it, err := h.svc.Review.ResolveItemCancellation(actor(c), id, true, bindNote(c).Note)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, it)
}

func (h *Handlers) RejectItemCancellation(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	it, err := h.svc.Review.ResolveItemCancellation(actor(c), id, false, bindNote(c).Note)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, it)
}

// ListCancellationRequests lists orders with a pending cancellation request.
// GET /api/cancellation-requests
func (h *Handlers) ListCancellationRequests(c *gin.Context) {
	p := pageFrom(c)
	f := repositories.OrderFilter{
		Page:         p,
		SellerID:     uintQueryPtr(c, "seller_id"),
		StoreOrderID: c.Query("store_order_id"),
	}
	rows, total, err := h.svc.Review.ListCancellationRequests(f)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(f.Page, total))
}

// ApproveCancellation approves a cancellation request (cancels the order).
// POST /api/cancellation-requests/:id/approve
func (h *Handlers) ApproveCancellation(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	o, err := h.svc.Review.ApproveCancellation(actor(c), id, bindNote(c).Note)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}

// RejectCancellation denies a cancellation request. POST /api/cancellation-requests/:id/reject
func (h *Handlers) RejectCancellation(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	o, err := h.svc.Review.RejectCancellation(actor(c), id, bindNote(c).Note)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}
