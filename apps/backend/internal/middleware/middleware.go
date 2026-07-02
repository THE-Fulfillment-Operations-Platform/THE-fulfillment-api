// Package middleware contains Gin middleware: request logging, panic recovery,
// CORS, JWT authentication and role-based authorization.
package middleware

import (
	"log"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
)

// RequestLogger logs method, path, status, latency and client IP for every
// request in a compact single line.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		if raw := c.Request.URL.RawQuery; raw != "" {
			path = path + "?" + raw
		}

		c.Next()

		log.Printf("[HTTP] %3d | %12v | %-15s | %-6s %s",
			c.Writer.Status(),
			time.Since(start),
			c.ClientIP(),
			c.Request.Method,
			path,
		)
	}
}

// Recovery converts panics into a unified 500 JSON response instead of crashing
// the process or leaking a stack trace to the client.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC] %v | %s %s", r, c.Request.Method, c.Request.URL.Path)
				c.AbortWithStatusJSON(500, response.Envelope{
					Success: false,
					Error:   &response.ErrorBody{Code: "INTERNAL", Message: "Internal server error"},
				})
			}
		}()
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
			if allowAll {
				c.Header("Access-Control-Allow-Origin", origin)
			} else if _, ok := set[origin]; ok {
				c.Header("Access-Control-Allow-Origin", origin)
			}
			c.Header("Vary", "Origin")
		}
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin,Content-Type,Accept,Authorization")
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Max-Age", "86400")

		if strings.EqualFold(c.Request.Method, "OPTIONS") {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}
