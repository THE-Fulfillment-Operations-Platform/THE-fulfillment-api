package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/middleware"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// APIKeyResolver exposes the open API's key lookup to the router.
func (h *Handlers) APIKeyResolver() middleware.APIKeyResolver { return h.svc.APIKey }

// ---------- API keys (internal: Master Data → Seller) ----------

// ListSellerAPIKeys lists a seller's open-API keys, revoked ones included.
// GET /api/sellers/:id/api-keys
func (h *Handlers) ListSellerAPIKeys(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	rows, err := h.svc.APIKey.List(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, rows)
}

// CreateSellerAPIKey issues a key. The response is the only time the plain key
// is ever shown. POST /api/sellers/:id/api-keys  { name }
func (h *Handlers) CreateSellerAPIKey(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !bindJSON(c, &in) {
		return
	}
	key, err := h.svc.APIKey.Create(actor(c), id, in.Name)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, key)
}

// RevokeSellerAPIKey ends a key. DELETE /api/sellers/:id/api-keys/:key_id
func (h *Handlers) RevokeSellerAPIKey(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	keyID, ok := uintParam(c, "key_id")
	if !ok {
		return
	}
	key, err := h.svc.APIKey.Revoke(actor(c), id, keyID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, key)
}

// ---------- Open API (/api/open/v1, API key) ----------

// openPrincipal returns the key + seller behind the request. The routes sit
// behind APIKeyAuth, so nil only happens if one is ever wired without it.
func openPrincipal(c *gin.Context) (*models.APIPrincipal, bool) {
	p := middleware.CurrentAPIPrincipal(c)
	if p == nil {
		response.AbortUnauthorized(c, "Authentication required")
		return nil, false
	}
	return p, true
}

// OpenPing answers "does my key work, and whose is it". The first call a new
// integration makes. GET /api/open/v1/ping
func (h *Handlers) OpenPing(c *gin.Context) {
	p, ok := openPrincipal(c)
	if !ok {
		return
	}
	response.OK(c, gin.H{
		"seller_code": p.SellerCode, "seller_name": p.SellerName,
		"key_name": p.KeyName, "key_prefix": p.KeyPrefix,
	})
}

// OpenCreateOrder creates an order for the key's seller — or, when this order id
// was sent before, returns the order it already created (200 instead of 201).
// POST /api/open/v1/orders
func (h *Handlers) OpenCreateOrder(c *gin.Context) {
	p, ok := openPrincipal(c)
	if !ok {
		return
	}
	var in services.OpenOrderInput
	if err := c.ShouldBindJSON(&in); err != nil {
		// The body is not the JSON this endpoint takes (broken syntax, a string
		// where a list belongs). Field-level rules are the service's, and answer
		// with 422 + one entry per field.
		response.Fail(c, apperr.New(http.StatusBadRequest, "INVALID_JSON",
			"Body không phải JSON hợp lệ theo mẫu của API"+jsonBindHint(err)))
		return
	}
	res, err := h.svc.Open.CreateOrder(p, c.ClientIP(), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if res.Created {
		response.Created(c, res)
		return
	}
	response.OK(c, res)
}

// OpenGetOrder returns one order by the caller's order id (or our internal code).
// GET /api/open/v1/orders/:ref
func (h *Handlers) OpenGetOrder(c *gin.Context) {
	p, ok := openPrincipal(c)
	if !ok {
		return
	}
	o, err := h.svc.Open.GetOrder(p.SellerID, c.Param("ref"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, o)
}

// OpenListOrders pages the seller's orders, newest first. ?order_id= narrows it
// to that one order — the lookup for an order id that cannot travel in a URL
// path (one containing "/"). GET /api/open/v1/orders
func (h *Handlers) OpenListOrders(c *gin.Context) {
	p, ok := openPrincipal(c)
	if !ok {
		return
	}
	from, err := services.ParseOpenTimeBound(c.Query("created_from"), "created_from", false)
	if err != nil {
		response.Fail(c, err)
		return
	}
	to, err := services.ParseOpenTimeBound(c.Query("created_to"), "created_to", true)
	if err != nil {
		response.Fail(c, err)
		return
	}
	rows, page, total, err := h.svc.Open.ListOrders(p.SellerID, services.OpenOrderQuery{
		Page:        pageFrom(c),
		OrderID:     c.Query("order_id"),
		Status:      c.Query("status"),
		CreatedFrom: from,
		CreatedTo:   to,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, rows, metaFor(page, total))
}

// jsonBindHint says where a body failed to parse, in the caller's terms: the
// field path they sent and what was expected there — never a Go type name.
func jsonBindHint(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return ": trường \"" + typeErr.Field + "\" sai kiểu dữ liệu"
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return ": lỗi cú pháp ở ký tự thứ " + strconv.FormatInt(syntaxErr.Offset, 10)
	}
	if errors.Is(err, io.EOF) {
		return ": body rỗng"
	}
	return ""
}

// OpenListSKUs lists the SKU codes an order may use. GET /api/open/v1/skus
func (h *Handlers) OpenListSKUs(c *gin.Context) {
	if _, ok := openPrincipal(c); !ok {
		return
	}
	rows, err := h.svc.Open.ListSKUs()
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, rows)
}
