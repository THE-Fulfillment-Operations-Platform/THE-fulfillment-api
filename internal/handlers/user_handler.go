package handlers

import (
	"github.com/gin-gonic/gin"

	"the-fulfillment/backend/internal/models"
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

// PermissionCatalog lists the tickable screens and each role's default ticks.
// GET /api/permission-catalog
func (h *Handlers) PermissionCatalog(c *gin.Context) {
	response.OK(c, h.svc.User.PermissionCatalog())
}

// ListRolePermissions lists each tickable role with its ticks, for the
// "Quyền theo vai trò" screen. GET /api/role-permissions
func (h *Handlers) ListRolePermissions(c *gin.Context) {
	out, err := h.svc.User.RolePermissions(actor(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

// UpdateRolePermissions sets a role's ticks; {"permissions": null} goes back to
// the built-in set. PUT /api/role-permissions/:role
func (h *Handlers) UpdateRolePermissions(c *gin.Context) {
	var in struct {
		Permissions services.PermsInput `json:"permissions"`
	}
	if !bindJSON(c, &in) {
		return
	}
	out, err := h.svc.User.SetRolePermissions(actor(c), models.Role(c.Param("role")), in.Permissions)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}
