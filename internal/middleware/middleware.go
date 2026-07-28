// Package middleware contains Gin middleware: request logging, panic recovery,
// CORS, JWT authentication and role-based authorization.
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
)

// RequestIDHeader carries the per-request correlation id. An inbound value (set
// by a reverse proxy) is honored so one id follows the request across hops;
// otherwise a fresh id is generated. The id is echoed back to the client and
// attached to every log line, so a user-reported error can be matched to the
// exact server-side logs.
const RequestIDHeader = "X-Request-ID"

// requestIDKey is the gin context key the request id is stored under.
const requestIDKey = "request_id"

// RequestID ensures every request has a correlation id.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(RequestIDHeader)
		// Cap inbound ids: a proxy sets short tokens; anything oversized is
		// untrusted junk that would bloat logs.
		if rid == "" || len(rid) > 64 {
			rid = newRequestID()
		}
		c.Set(requestIDKey, rid)
		c.Header(RequestIDHeader, rid)
		c.Next()
	}
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "rid-unavailable"
	}
	return hex.EncodeToString(b[:])
}

// requestID reads the correlation id set by RequestID (empty if not set).
func requestID(c *gin.Context) string {
	return c.GetString(requestIDKey)
}

// RequestLogger logs method, path, status, latency, client IP and request id
// for every request. Uses slog so production (JSON handler) gets queryable
// fields while dev keeps a readable line.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		if raw := c.Request.URL.RawQuery; raw != "" {
			path = path + "?" + raw
		}

		c.Next()

		slog.Info("http",
			"status", c.Writer.Status(),
			"latency", time.Since(start).String(),
			"ip", c.ClientIP(),
			"method", c.Request.Method,
			"path", path,
			"rid", requestID(c),
		)
	}
}

// Recovery converts panics into a unified 500 JSON response instead of crashing
// the process or leaking a stack trace to the client.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic recovered",
					"panic", fmt.Sprintf("%v", r),
					"method", c.Request.Method,
					"path", c.Request.URL.Path,
					"rid", requestID(c),
				)
				c.AbortWithStatusJSON(500, response.Envelope{
					Success: false,
					Error:   &response.ErrorBody{Code: "INTERNAL", Message: "Internal server error"},
				})
			}
		}()
		c.Next()
	}
}

// BodyLimit rejects request bodies larger than maxBytes. Without it a single
// oversized (or malicious) upload gets buffered into memory by the Excel-import
// handlers and can take the whole server down. Requests that declare an
// oversized Content-Length are refused up front; chunked/lying clients are cut
// off mid-read by MaxBytesReader, which surfaces as a handler read/bind error.
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	msg := fmt.Sprintf("Nội dung gửi lên vượt quá giới hạn %d MB", maxBytes>>20)
	return func(c *gin.Context) {
		if c.Request.ContentLength > maxBytes {
			response.AbortPayloadTooLarge(c, msg)
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

// CORS applies permissive-but-configurable cross-origin headers. When the
// allowed origins contains "*" every origin is echoed back (handy for local FE
// development); otherwise only listed origins are allowed.
func CORS(allowedOrigins []string) gin.HandlerFunc {
	allowAll := false
	set := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o == "*" {
			allowAll = true
		}
		set[o] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			_, explicit := set[origin]
			if allowAll {
				c.Header("Access-Control-Allow-Origin", origin)
			} else if explicit {
				c.Header("Access-Control-Allow-Origin", origin)
			}
			// Only grant credentialed CORS to explicitly allow-listed origins.
			// Reflecting an arbitrary origin AND allowing credentials is unsafe, so
			// never combine "*" (reflect-any) with Allow-Credentials.
			if explicit && !allowAll {
				c.Header("Access-Control-Allow-Credentials", "true")
			}
			c.Header("Vary", "Origin")
		}
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin,Content-Type,Accept,Authorization,X-Request-ID")
		c.Header("Access-Control-Max-Age", "86400")

		if strings.EqualFold(c.Request.Method, "OPTIONS") {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}
