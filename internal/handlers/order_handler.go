package handlers

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/middleware"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// ListOrders lists orders with filters (store, sku, status, date). GET /api/orders
func (h *Handlers) ListOrders(c *gin.Context) {
	p := pageFrom(c)
	f := repositories.OrderFilter{
		Page:         p,
		SellerID:     uintQueryPtr(c, "seller_id"),
		StoreID:      uintQueryPtr(c, "store_id"),
		SKUCode:      c.Query("sku"),
		SellerStatus: c.Query("status"),
		StoreOrderID: c.Query("store_order_id"),
		DateFrom:     timeQueryPtr(c, "date_from"),
		DateTo:       timeQueryPtr(c, "date_to"),
	}
	rows, total, err := h.svc.Order.ListOrders(f)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

// GetOrder returns full order detail. GET /api/orders/:id
func (h *Handlers) GetOrder(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	o, err := h.svc.Order.GetOperationalOrder(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}

// CreateOrderDirect manually creates a single order (convenience/TODO path).
// POST /api/orders
func (h *Handlers) CreateOrderDirect(c *gin.Context) {
	var in services.DirectOrderInput
	if !bindJSON(c, &in) {
		return
	}
	o, err := h.svc.Order.CreateOrderDirect(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, o)
}

// ---------- Items ----------

func itemFilterFrom(c *gin.Context) repositories.ItemFilter {
	return repositories.ItemFilter{
		Page:           pageFrom(c),
		SellerID:       uintQueryPtr(c, "seller_id"),
		StoreID:        uintQueryPtr(c, "store_id"),
		SKUCode:        c.Query("sku"),
		InternalStatus: c.Query("status"),
		DesignStatus:   c.Query("design_status"),
		BatchID:        uintQueryPtr(c, "batch_id"),
		DateFrom:       timeQueryPtr(c, "date_from"),
		DateTo:         timeQueryPtr(c, "date_to"),
	}
}

// ListItems lists order items with filters. GET /api/items
func (h *Handlers) ListItems(c *gin.Context) {
	f := itemFilterFrom(c)
	rows, total, err := h.svc.Order.ListItems(f)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(f.Page, total))
}

// GetItem returns a single item. GET /api/items/:id
func (h *Handlers) GetItem(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	it, err := h.svc.Order.GetItem(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, it)
}

// ---------- Design queue ----------

// DesignQueue lists items needing design. GET /api/design-queue
func (h *Handlers) DesignQueue(c *gin.Context) {
	f := itemFilterFrom(c)
	rows, total, err := h.svc.Order.DesignQueue(f)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(f.Page, total))
}

// DownloadDesignAssetsZip streams the design + mockup files of the design queue
// as a ZIP, one folder per internal code. An optional ?item_ids=1,2,3 restricts
// the export to the ticked rows; omitting it bundles the whole queue.
// GET /api/design-queue/assets.zip
func (h *Handlers) DownloadDesignAssetsZip(c *gin.Context) {
	ids := parseUintCSV(c.Query("item_ids"))
	c.Header("Content-Disposition", `attachment; filename="design-assets.zip"`)
	c.Header("Content-Type", "application/zip")
	if err := h.svc.Order.StreamDesignAssetsZip(c.Request.Context(), c.Writer, ids); err != nil {
		// Only surface a JSON error if nothing has been streamed yet; once the ZIP
		// body has started we can't switch to an error envelope.
		if !c.Writer.Written() {
			response.Fail(c, err)
		}
		return
	}
}

// parseUintCSV turns "1,2,3" into []uint, silently dropping blanks and non-numeric
// tokens. Returns nil for an empty string so callers treat it as "no filter".
func parseUintCSV(s string) []uint {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []uint
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if v, err := strconv.ParseUint(tok, 10, 64); err == nil {
			out = append(out, uint(v))
		}
	}
	return out
}

// UpdateItemDesign saves print/cut/mockup files and (optionally) sets design ready.
// PATCH /api/items/:id/design
func (h *Handlers) UpdateItemDesign(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.UpdateDesignInput
	if !bindJSON(c, &in) {
		return
	}
	it, err := h.svc.Order.UpdateItemDesign(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, it)
}

// MaterialBuckets returns design-ready unbatched item counts per material.
// GET /api/design-queue/material-buckets
func (h *Handlers) MaterialBuckets(c *gin.Context) {
	buckets, err := h.svc.Order.MaterialBuckets()
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, buckets)
}

// DesignReadyItemsForMaterial lists design-ready, unbatched items for a material.
// GET /api/design-queue/material/:materialId/items
func (h *Handlers) DesignReadyItemsForMaterial(c *gin.Context) {
	materialID, ok := uintParam(c, "materialId")
	if !ok {
		return
	}
	p := pageFrom(c)
	rows, total, err := h.svc.Order.DesignReadyItemsForMaterial(materialID, p)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

// ---------- Seller view ----------

// SellerOrders returns the seller-scoped, sanitized order list (high-level
// status only). GET /api/seller/orders
func (h *Handlers) SellerOrders(c *gin.Context) {
	claims := middleware.CurrentClaims(c)
	if claims == nil || claims.SellerID == nil {
		response.AbortForbidden(c, "Seller account required")
		return
	}
	p := pageFrom(c)
	f := repositories.OrderFilter{
		Page:         p,
		SellerStatus: c.Query("status"),
		StoreOrderID: c.Query("store_order_id"),
		DateFrom:     timeQueryPtr(c, "date_from"),
		DateTo:       timeQueryPtr(c, "date_to"),
	}
	rows, total, err := h.svc.Order.SellerOrders(*claims.SellerID, f)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(p, total))
}

// SellerOrderDetail returns one sanitized order for the seller. GET /api/seller/orders/:id
func (h *Handlers) SellerOrderDetail(c *gin.Context) {
	claims := middleware.CurrentClaims(c)
	if claims == nil || claims.SellerID == nil {
		response.AbortForbidden(c, "Seller account required")
		return
	}
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	v, err := h.svc.Order.SellerOrderDetail(*claims.SellerID, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, v)
}
