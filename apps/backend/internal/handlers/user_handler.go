package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/response"
	"the-fulfillment/backend/internal/services"
)

// CreateUser creates a user. POST /api/users
func (h *Handlers) CreateUser(c *gin.Context) {
	var in services.CreateUserInput
	if !bindJSON(c, &in) {
		return
	}
	u, err := h.svc.User.Create(actor(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, u)
}

// ListUsers lists users. GET /api/users
func (h *Handlers) ListUsers(c *gin.Context) {
	p := pageFrom(c)
	users, total, err := h.svc.User.List(p)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, users, metaFor(p, total))
}

// GetUser fetches a user. GET /api/users/:id
func (h *Handlers) GetUser(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	u, err := h.svc.User.Get(id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, u)
}

// UpdateUser updates a user. PUT /api/users/:id
func (h *Handlers) UpdateUser(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	var in services.UpdateUserInput
	if !bindJSON(c, &in) {
		return
	}
	u, err := h.svc.User.Update(actor(c), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, u)
}

// DeleteUser deletes a user. DELETE /api/users/:id
func (h *Handlers) DeleteUser(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if err := h.svc.User.Delete(actor(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"deleted": true})
}
