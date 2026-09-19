package middleware

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/response"
)

// contextKey constants for values stashed on the Gin context.
const (
	ctxClaims = "auth_claims"
	ctxAccess = "auth_access"
)

// AccessLoader resolves a user's current role and permissions (see
// services.AccessService). errRevoked is what it returns for a locked or
// deleted account.
type AccessLoader interface {
	LoadAccess(userID uint) (*models.Access, error)
}

// LoadAccess runs after Auth and replaces what the token says about the caller
// with what the database says now: a role change, new permission ticks or a
// locked account take effect on the next request. revoked is the loader's
// sentinel for "account gone" (answered 401 so the client signs out); any other
// failure is a 500.
func LoadAccess(loader AccessLoader, revoked error) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims := CurrentClaims(c)
		if claims == nil {
			response.AbortUnauthorized(c, "Authentication required")
			return
		}
		a, err := loader.LoadAccess(claims.UserID)
		if err != nil {
			if errors.Is(err, revoked) {
				response.AbortUnauthorized(c, "Tài khoản đã bị khoá hoặc xoá")
				return
			}
			response.Fail(c, apperr.Internal("could not load access").Wrap(err))
			c.Abort()
			return
		}
		c.Set(ctxAccess, a)
		c.Next()
	}
}

// CurrentAccess returns the caller's access loaded by LoadAccess, or nil.
func CurrentAccess(c *gin.Context) *models.Access {
	v, ok := c.Get(ctxAccess)
	if !ok {
		return nil
	}
	a, _ := v.(*models.Access)
	return a
}

// currentRole prefers the fresh role from LoadAccess over the token's.
func currentRole(c *gin.Context) (models.Role, bool) {
	if a := CurrentAccess(c); a != nil {
		return a.Role, true
	}
	if claims := CurrentClaims(c); claims != nil {
		return claims.Role, true
	}
	return "", false
}

// RequirePerm authorizes the request if the caller holds ANY of perms (OWNER
// holds all). Screens share some endpoints — the order list feeds the orders
// screen, CS lookup and the journeys board — so a route names every tick that
// legitimately needs it. Must run after LoadAccess.
func RequirePerm(perms ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		a := CurrentAccess(c)
		if a == nil {
			response.AbortUnauthorized(c, "Authentication required")
			return
		}
		for _, p := range perms {
			if a.Can(p) {
				c.Next()
				return
			}
		}
		response.AbortForbidden(c, "Tài khoản chưa được cấp quyền cho thao tác này — nhờ quản trị tick thêm quyền ở màn Người dùng")
	}
}

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
		role, ok := currentRole(c)
		if !ok {
			response.AbortUnauthorized(c, "Authentication required")
			return
		}
		if _, ok := allowed[role]; !ok {
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
