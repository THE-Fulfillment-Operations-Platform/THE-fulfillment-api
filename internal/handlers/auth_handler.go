package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/middleware"
	"the-fulfillment/backend/internal/response"
)

// LoginRequest is the login payload.
type LoginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

// Login authenticates a user and returns a JWT.
// POST /api/auth/login
func (h *Handlers) Login(c *gin.Context) {
	var req LoginRequest
	if !bindJSON(c, &req) {
		return
	}
	result, err := h.svc.Auth.Login(req.Email, req.Password, c.ClientIP())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// Me returns the currently authenticated user.
// GET /api/me
func (h *Handlers) Me(c *gin.Context) {
	claims := middleware.CurrentClaims(c)
	if claims == nil {
		response.AbortUnauthorized(c, "Authentication required")
		return
	}
	user, err := h.svc.Auth.Me(claims.UserID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, user)
}
