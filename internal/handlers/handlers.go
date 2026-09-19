// Package handlers contains the HTTP layer. Handlers bind/validate input, build
// the Actor from the JWT claims, call a service and write the unified response.
// They contain no business logic.
package handlers

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/middleware"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

func badRequest(msg string) error { return apperr.BadRequest(msg) }

// Handlers bundles all handlers around the service layer.
type Handlers struct {
	svc *services.Services
}

// New builds the handler bundle.
func New(svc *services.Services) *Handlers {
	return &Handlers{svc: svc}
}

// AccessLoader exposes the per-request access lookup to the router.
func (h *Handlers) AccessLoader() middleware.AccessLoader { return h.svc.Access }

// actor builds a services.Actor from the authenticated claims on the context.
func actor(c *gin.Context) services.Actor {
	claims := middleware.CurrentClaims(c)
	if claims == nil {
		return services.Actor{IP: c.ClientIP()}
	}
	a := services.Actor{
		ID: claims.UserID, Email: claims.Email, Role: claims.Role,
		SellerID: claims.SellerID, IP: c.ClientIP(),
	}
	// The token's role may be stale; LoadAccess read the current one.
	if acc := middleware.CurrentAccess(c); acc != nil {
		a.Role, a.SellerID, a.Perms = acc.Role, acc.SellerID, acc.Perms
	}
	return a
}

// pageFrom reads page/page_size query params.
func pageFrom(c *gin.Context) repositories.Page {
	page, _ := strconv.Atoi(c.Query("page"))
	size, _ := strconv.Atoi(c.Query("page_size"))
	return repositories.Page{Page: page, PageSize: size}
}

// metaFor builds list pagination metadata.
func metaFor(p repositories.Page, total int64) *response.Meta {
	p = p.Normalize()
	totalPages := 0
	switch {
	case p.All():
		// "Tất cả" fits everything on one page; reporting 0 pages would make the
		// pager render "Trang 1 / 0" and disable both nav buttons for no reason.
		if total > 0 {
			totalPages = 1
		}
	case p.PageSize > 0:
		totalPages = int((total + int64(p.PageSize) - 1) / int64(p.PageSize))
	}
	return &response.Meta{Page: p.Page, PageSize: p.PageSize, Total: total, TotalPages: totalPages}
}

// uintParam parses a uint path parameter.
func uintParam(c *gin.Context, name string) (uint, bool) {
	v, err := strconv.ParseUint(c.Param(name), 10, 64)
	if err != nil {
		response.Fail(c, badRequest("Invalid "+name))
		return 0, false
	}
	return uint(v), true
}

// uintQueryPtr parses an optional uint query parameter.
func uintQueryPtr(c *gin.Context, name string) *uint {
	s := c.Query(name)
	if s == "" {
		return nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return nil
	}
	u := uint(v)
	return &u
}

// boolQueryPtr parses an optional boolean query parameter. Absent (or
// unparseable) yields nil = "don't filter on this", which is a different answer
// from false — a caller asking for has_tracking=false wants the orders that have
// none, not every order.
func boolQueryPtr(c *gin.Context, name string) *bool {
	s := c.Query(name)
	if s == "" {
		return nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return nil
	}
	return &v
}

// timeQueryPtr parses an optional RFC3339 or date (YYYY-MM-DD) query parameter.
func timeQueryPtr(c *gin.Context, name string) *time.Time {
	s := c.Query(name)
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return &t
	}
	return nil
}

// bindJSON binds and validates a JSON body, writing a 422 on failure.
func bindJSON(c *gin.Context, dst interface{}) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		message, details := humanizeBindErr(err)
		response.FailValidation(c, message, details)
		return false
	}
	return true
}

// failZipStream reports a ZIP download that failed before any byte of the archive
// went out. The attachment headers were set up front for the success path; left
// in place they would label the JSON error "application/zip" and ask the browser
// to save it as the archive. Once the body has started there is no way back to an
// error envelope, so nothing is written then.
func failZipStream(c *gin.Context, err error) {
	if c.Writer.Written() {
		return
	}
	c.Writer.Header().Del("Content-Disposition")
	c.Writer.Header().Del("Content-Type")
	response.Fail(c, err)
}
