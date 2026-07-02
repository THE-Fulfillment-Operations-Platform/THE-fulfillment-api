package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/response"
)

// contextKey constants for values stashed on the Gin context.
const (
	ctxClaims = "auth_claims"
)

// Auth validates the Bearer JWT and stores the claims on the context. Requests
// without a valid token are rejected with 401.
func Auth(jwtManager *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			response.AbortUnauthorized(c, "Missing Authorization header")
			return
		}
		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			response.AbortUnauthorized(c, "Authorization header must be 'Bearer <token>'")
			return
		}
		claims, err := jwtManager.Parse(strings.TrimSpace(parts[1]))
		if err != nil {
			response.AbortUnauthorized(c, "Invalid or expired token")
			return
		}
		c.Set(ctxClaims, claims)
		c.Next()
	}
}

// RequireRoles authorizes the request only if the caller holds one of the roles.
// It must run after Auth.
func RequireRoles(roles ...models.Role) gin.HandlerFunc {
	allowed := make(map[models.Role]struct{}, len(roles))
	for _, r := range roles {
		allowed[r] = struct{}{}
	}
	return func(c *gin.Context) {
		claims := CurrentClaims(c)
		if claims == nil {
			response.AbortUnauthorized(c, "Authentication required")
			return
		}
		if _, ok := allowed[claims.Role]; !ok {
			response.AbortForbidden(c, "You do not have permission to perform this action")
			return
		}
		c.Next()
	}
}

// CurrentClaims returns the authenticated claims, or nil if unauthenticated.
func CurrentClaims(c *gin.Context) *auth.Claims {
	v, ok := c.Get(ctxClaims)
	if !ok {
		return nil
	}
	claims, _ := v.(*auth.Claims)
	return claims
}
