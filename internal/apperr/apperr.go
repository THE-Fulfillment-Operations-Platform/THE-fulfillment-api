// Package apperr defines a small, typed application error used across the
// service layer. Handlers translate these into the unified JSON error response
// so business logic never imports the HTTP layer.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is a typed application error carrying an HTTP status, a stable machine
// code and a human-readable message.
type Error struct {
	Status  int    // HTTP status code
	Code    string // stable machine-readable code, e.g. "NOT_FOUND"
	Message string // human-readable message (safe to expose)
	Err     error  // optional wrapped error (logged, never exposed)
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an application error.
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// Wrap attaches an underlying error for logging without exposing it.
func (e *Error) Wrap(err error) *Error {
	clone := *e
	clone.Err = err
	return &clone
}

// As extracts an *Error from any error chain.
func As(err error) (*Error, bool) {
	var ae *Error
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// Common constructors used throughout the service layer.

func BadRequest(msg string) *Error   { return New(http.StatusBadRequest, "BAD_REQUEST", msg) }
func Unauthorized(msg string) *Error { return New(http.StatusUnauthorized, "UNAUTHORIZED", msg) }
func Forbidden(msg string) *Error    { return New(http.StatusForbidden, "FORBIDDEN", msg) }
func NotFound(msg string) *Error     { return New(http.StatusNotFound, "NOT_FOUND", msg) }
func Conflict(msg string) *Error     { return New(http.StatusConflict, "CONFLICT", msg) }
func Unprocessable(msg string) *Error {
	return New(http.StatusUnprocessableEntity, "UNPROCESSABLE", msg)
}
func Internal(msg string) *Error { return New(http.StatusInternalServerError, "INTERNAL", msg) }
