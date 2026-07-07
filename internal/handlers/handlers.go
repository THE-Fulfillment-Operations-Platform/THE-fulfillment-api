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

// actor builds a services.Actor from the authenticated claims on the context.
func actor(c *gin.Context) services.Actor {
	claims := middleware.CurrentClaims(c)
	if claims == nil {
		return services.Actor{IP: c.ClientIP()}
	}
	return services.Actor{
		ID: claims.UserID, Email: claims.Email, Role: claims.Role,
		SellerID: claims.SellerID, IP: c.ClientIP(),
	}
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
	if p.PageSize > 0 {
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
