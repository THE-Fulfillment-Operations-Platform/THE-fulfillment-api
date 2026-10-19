package services

import (
	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// RolePermissionView is one row of the "Quyền theo vai trò" screen.
type RolePermissionView struct {
	Role models.Role `json:"role"`
	// Permissions is what the role's users get right now: the configured set,
	// else the built-in one.
	Permissions []string `json:"permissions"`
	// Builtin is the out-of-the-box set — what "Về mặc định gốc" goes back to.
	Builtin []string `json:"builtin"`
	// Customized: the role has its own configured set (roles.permissions not NULL).
	Customized bool `json:"customized"`
	// Editable / ReadOnlyReason: whether THIS caller may change the role, and if
	// not, why — so the screen explains instead of failing on save.
	Editable       bool   `json:"editable"`
	ReadOnlyReason string `json:"read_only_reason,omitempty"`
	// Following counts the accounts with no ticks of their own: who a change here
	// reaches. Accounts with their own ticks keep them.
	Following int64 `json:"following"`
}

// roleEditBlock says why actor may not change role's ticks ("" = may). Same lines
// as editing an account: nobody but an OWNER changes their own access, the
// TECHNICIAN role is the OWNER's to shape, and OWNER/SELLER are not tick sets.
func roleEditBlock(actor Actor, role models.Role) string {
	switch {
	case !models.RoleTickable(role):
		return "Vai trò này không phân quyền theo tick."
	case !actor.Role.HasAdminRights():
		return "Chỉ Quản trị viên / Chủ sở hữu được đổi quyền của vai trò."
	case actor.Role == models.RoleOwner:
		return ""
	case role == actor.Role:
		return "Không tự đổi quyền vai trò của chính mình — nhờ chủ sở hữu sửa giúp."
	case role == models.RoleTechnician:
		return "Chỉ chủ sở hữu (OWNER) được đổi quyền của Kỹ thuật viên."
	}
	return ""
}

// RolePermissions lists every tickable role with its current and built-in ticks.
func (s *UserService) RolePermissions(actor Actor) ([]RolePermissionView, error) {
	overrides, err := s.repo.Role.PermissionOverrides()
	if err != nil {
		return nil, apperr.Internal("could not load role permissions").Wrap(err)
	}
	following, err := s.repo.Role.CountFollowing()
	if err != nil {
		return nil, apperr.Internal("could not count users").Wrap(err)
	}
	out := make([]RolePermissionView, 0, len(models.AllRoles))
	for _, r := range models.AllRoles {
		if !models.RoleTickable(r) {
			continue
		}
		v := RolePermissionView{
			Role: r, Builtin: models.BuiltinRolePerms(r), Following: following[r],
		}
		if perms, ok := overrides[r]; ok {
			v.Permissions, v.Customized = models.NormalizePerms(perms), true
		} else {
			v.Permissions = v.Builtin
		}
		v.ReadOnlyReason = roleEditBlock(actor, r)
		v.Editable = v.ReadOnlyReason == ""
		out = append(out, v)
	}
	return out, nil
}

// SetRolePermissions stores a role's tick set. list nil = back to the built-in
// set; a list equal to the built-in set is stored as NULL too, so the role keeps
// following later changes to the built-in defaults.
func (s *UserService) SetRolePermissions(actor Actor, role models.Role, in PermsInput) (*RolePermissionView, error) {
	if !validRole(role) {
		return nil, apperr.BadRequest("Vai trò không hợp lệ")
	}
	if !in.Set {
		return nil, apperr.BadRequest("Thiếu danh sách quyền (permissions)")
	}
	if reason := roleEditBlock(actor, role); reason != "" {
		return nil, apperr.Forbidden(reason)
	}
	var stored models.PermList
	if in.List != nil {
		for _, p := range in.List {
			if !models.IsKnownPerm(p) {
				return nil, apperr.BadRequest("Quyền không hợp lệ: " + p)
			}
		}
		normalized := models.NormalizePerms(in.List)
		// An ADMIN hands a role only what they hold themselves — same rule as
		// ticking an account.
		for _, p := range normalized {
			if !actor.Can(p) {
				return nil, apperr.Forbidden("Không thể cấp quyền mà chính bạn không có: " + p)
			}
		}
		if !samePerms(models.PermList(normalized), models.PermList(models.BuiltinRolePerms(role))) {
			stored = models.PermList(normalized)
		}
	}
	before := models.RoleDefaultPerms(role)
	if err := s.repo.Role.SetPermissions(role, stored); err != nil {
		return nil, apperr.Internal("could not save role permissions").Wrap(err)
	}
	s.access.RoleTicksChanged()
	if s.access == nil {
		// No access service (tests built without one): apply in-process directly.
		if overrides, err := s.repo.Role.PermissionOverrides(); err == nil {
			models.SetRoleOverrides(overrides)
		}
	}
	after := models.RoleDefaultPerms(role)
	s.audit.Log(actor, "ROLE_PERMS_UPDATE", "role", nil, "Đổi quyền vai trò "+string(role),
		models.JSONMap{"role": string(role), "before": before, "after": after, "builtin": stored == nil})

	views, err := s.RolePermissions(actor)
	if err != nil {
		return nil, err
	}
	for i := range views {
		if views[i].Role == role {
			return &views[i], nil
		}
	}
	return nil, apperr.Internal("role vanished after save")
}
