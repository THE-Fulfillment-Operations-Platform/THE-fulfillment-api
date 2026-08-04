package handlers

import (
	"context"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
)

// trackingCtx bounds a provider call chain and still cancels when the caller
// disconnects, so an abandoned request stops burning provider rate limit.
func trackingCtx(c *gin.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), d)
}

// GetOrderTracking returns the stored journey of an order's parcel, newest scan
// first, together with whether the provider integration is even on (so the UI
// can explain an empty timeline instead of showing a bare "no data").
// GET /api/orders/:id/tracking/events
func (h *Handlers) GetOrderTracking(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	events, err := h.svc.TrackingSync.Timeline(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"events":  events,
		"enabled": h.svc.TrackingSync.Enabled(),
	})
}

// GetSellerOrderTracking returns the journey of one of the seller's own orders.
// GET /api/seller/orders/:id/tracking/events
func (h *Handlers) GetSellerOrderTracking(c *gin.Context) {
	sellerID, ok := sellerIDFrom(c)
	if !ok {
		return
	}
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	events, err := h.svc.TrackingSync.SellerTimeline(sellerID, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"events":  events,
		"enabled": h.svc.TrackingSync.Enabled(),
	})
}

// SyncOrderTracking refreshes ONE order from the provider on demand. When the
// order has no tracking number yet it first asks the provider whether a parcel
// is registered under this store order id.
// POST /api/orders/:id/tracking/sync
func (h *Handlers) SyncOrderTracking(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	// Bounded independently of the client: a provider call chain (login + resolve
	// + detail + timeline) is slow, and the browser may well have given up first.
	ctx, cancel := trackingCtx(c,90*time.Second)
	defer cancel()

	order, err := h.svc.TrackingSync.SyncOrderByID(ctx, actor(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, order)
}

// RunTrackingSync triggers one full pass (resolve + refresh) by hand — the same
// work the scheduler does, for when an operator does not want to wait for the
// next tick.
// POST /api/tracking/sync
func (h *Handlers) RunTrackingSync(c *gin.Context) {
	// A manual pass is deliberately smaller than the scheduled one: it must answer
	// while someone is watching, not grind through the whole backlog.
	batch := 40
	if n, err := strconv.Atoi(c.Query("limit")); err == nil && n > 0 && n <= 500 {
		batch = n
	}
	ctx, cancel := trackingCtx(c,4*time.Minute)
	defer cancel()

	stats, err := h.svc.TrackingSync.RunOnceForActor(ctx, actor(c), batch)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, stats)
}
