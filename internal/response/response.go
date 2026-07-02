// Package response provides a single, consistent JSON envelope for every API
// response so the frontend can rely on a stable shape.
package response

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"the-fulfillment/backend/internal/apperr"
)

// Envelope is the unified response shape returned by every endpoint.
//
//	{ "success": true,  "data": {...}, "meta": {...} }
//	{ "success": false, "error": { "code": "...", "message": "...", "details": ... } }
type Envelope struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Meta    *Meta       `json:"meta,omitempty"`
	Error   *ErrorBody  `json:"error,omitempty"`
}

// Meta carries pagination / list metadata.
type Meta struct {
	Page       int   `json:"page,omitempty"`
	PageSize   int   `json:"page_size,omitempty"`
	Total      int64 `json:"total"`
	TotalPages int   `json:"total_pages,omitempty"`
}

// ErrorBody is the machine + human readable error payload.
type ErrorBody struct {
	Code    string      `json:"code"`
	Message string      `json:"message"`
	Details interface{} `json:"details,omitempty"`
}

// OK writes a 200 success envelope.
func OK(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, Envelope{Success: true, Data: data})
}

// Created writes a 201 success envelope.
func Created(c *gin.Context, data interface{}) {
	c.JSON(http.StatusCreated, Envelope{Success: true, Data: data})
}

// List writes a 200 success envelope with pagination metadata.
func List(c *gin.Context, data interface{}, meta *Meta) {
	c.JSON(http.StatusOK, Envelope{Success: true, Data: data, Meta: meta})
}

// Fail writes an error envelope, translating *apperr.Error when possible.
func Fail(c *gin.Context, err error) {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		c.JSON(ae.Status, Envelope{
			Success: false,
			Error:   &ErrorBody{Code: ae.Code, Message: ae.Message},
		})
		return
	}
	// Unknown error: do not leak internals.
	c.JSON(http.StatusInternalServerError, Envelope{
		Success: false,
		Error:   &ErrorBody{Code: "INTERNAL", Message: "Internal server error"},
	})
}

// FailValidation writes a 422 with field-level details.
func FailValidation(c *gin.Context, details interface{}) {
	c.JSON(http.StatusUnprocessableEntity, Envelope{
		Success: false,
		Error:   &ErrorBody{Code: "VALIDATION_ERROR", Message: "Request validation failed", Details: details},
	})
}

// AbortUnauthorized is a helper for middleware.
func AbortUnauthorized(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, Envelope{
		Success: false,
		Error:   &ErrorBody{Code: "UNAUTHORIZED", Message: message},
	})
}

// AbortForbidden is a helper for middleware.
func AbortForbidden(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusForbidden, Envelope{
		Success: false,
		Error:   &ErrorBody{Code: "FORBIDDEN", Message: message},
	})
}
