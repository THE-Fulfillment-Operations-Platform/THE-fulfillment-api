package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// UserService manages user accounts (admin/owner only at the route layer).
type UserService struct {
	repo   *repositories.Repositories
	audit  *AuditService
	access *AccessService
}

// PermsInput is a permissions field that may be absent (leave as is), null
// (follow the role's defaults) or a list of ticks (store exactly that).
type PermsInput struct {
	Set  bool
	List []string // nil = role defaults
}

// UnmarshalJSON records that the field was sent; encoding/json calls it for an
// explicit null too, which is how "back to role defaults" is told apart from
// "not touched".
func (p *PermsInput) UnmarshalJSON(b []byte) error {
	p.Set = true
	if string(b) == "null" {
		p.List = nil
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	if l == nil {
		l = []string{}
	}
	p.List = l
	return nil
}

// CreateUserInput is the create payload.
type CreateUserInput struct {
	Email    string      `json:"email" binding:"required,email"`
	Password string      `json:"password" binding:"required,min=6"`
	FullName string      `json:"full_name" binding:"required"`
	Role     models.Role `json:"role" binding:"required"`
	SellerID *uint       `json:"seller_id"`
	IsActive *bool       `json:"is_active"`
	// Permissions: omitted or null = the role's defaults; a list = exactly
	// those ticks.
	Permissions PermsInput `json:"permissions"`
	// RestoreDeleted confirms taking over a soft-deleted account that already
	// owns this email. Without it such a create is refused with
	// ErrCodeDeletedEmail so the operator sees WHOSE account it was first —
	// restoring hands that person's entire history (batches created, QC checks,
	// audit trail) to whoever is being created now.
	RestoreDeleted bool `json:"restore_deleted"`
}

// ErrCodeDeletedEmail is the stable code the client keys off to offer
// "khôi phục tài khoản cũ?" instead of showing a raw conflict.
const ErrCodeDeletedEmail = "USER_DELETED_EMAIL"

// UpdateUserInput is the update payload (all optional).
type UpdateUserInput struct {
	FullName *string      `json:"full_name"`
	Password *string      `json:"password"`
	Role     *models.Role `json:"role"`
	SellerID *uint        `json:"seller_id"`
	IsActive *bool        `json:"is_active"`
	// Permissions: omitted = unchanged (but reset to the new role's defaults
	// when the role changes); null = role defaults; a list = exactly those.
	Permissions PermsInput `json:"permissions"`
}

// resolvePerms turns a permissions input into what is stored: nil (role
// defaults) or a normalised explicit list. OWNER and SELLER never store a list —
// OWNER has everything, SELLER none of it. An unknown key is an error rather
// than silently dropped, so a typo in a client never passes as "saved".
func resolvePerms(role models.Role, list []string) (models.PermList, error) {
	if list == nil || role == models.RoleOwner || role == models.RoleSeller {
		return nil, nil
	}
	for _, p := range list {
		if !models.IsKnownPerm(p) {
			return nil, apperr.BadRequest("Quyền không hợp lệ: " + p)
		}
	}
	return models.PermList(models.NormalizePerms(list)), nil
}

// guardGrant enforces who may hand out what:
//   - only an OWNER creates, promotes to or edits an OWNER account;
//   - an ADMIN can only grant ticks they hold themselves.
func guardGrant(actor Actor, targetRole models.Role, perms models.PermList) error {
	if actor.Role == models.RoleOwner {
		return nil
	}
	if targetRole == models.RoleOwner {
		return apperr.Forbidden("Chỉ chủ sở hữu (OWNER) mới tạo hoặc sửa được tài khoản OWNER.")
	}
	for _, p := range perms {
		if !actor.Can(p) {
			return apperr.Forbidden("Không thể cấp quyền mà chính bạn không có: " + p)
		}
	}
	return nil
}

func permsAudit(p models.PermList) interface{} {
	if p == nil {
		return "role_default"
	}
	return []string(p)
}

func validRole(r models.Role) bool {
	for _, role := range models.AllRoles {
		if role == r {
			return true
		}
	}
	return false
}

// ensureSellerExists validates that a referenced seller row exists. Without this
// pre-check a non-existent seller_id only fails at INSERT time as a foreign-key
// violation, which the caller wraps into a generic "could not create user" with
// no usable reason. Returning a clear BadRequest lets the UI show what's wrong.
func (s *UserService) ensureSellerExists(sellerID *uint) error {
	if sellerID == nil {
		return nil
	}
	if _, err := s.repo.Seller.FindByID(*sellerID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperr.BadRequest(fmt.Sprintf("Seller ID %d không tồn tại. Vui lòng nhập ID của seller đã có trong hệ thống.", *sellerID))
		}
		return apperr.Internal("seller lookup failed").Wrap(err)
	}
	return nil
}

// Create creates a new user with a hashed password.
func (s *UserService) Create(actor Actor, in CreateUserInput) (*models.User, error) {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if !validRole(in.Role) {
		return nil, apperr.BadRequest("Invalid role")
	}
	if in.Role == models.RoleSeller && in.SellerID == nil {
		return nil, apperr.BadRequest("seller_id is required for SELLER users")
	}
	if err := s.ensureSellerExists(in.SellerID); err != nil {
		return nil, err
	}
	perms, err := resolvePerms(in.Role, in.Permissions.List)
	if err != nil {
		return nil, err
	}
	if err := guardGrant(actor, in.Role, perms); err != nil {
		return nil, err
	}
	exists, err := s.repo.User.ExistsByEmail(in.Email)
	if err != nil {
		return nil, apperr.Internal("user lookup failed").Wrap(err)
	}
	if exists {
		return nil, apperr.Conflict("A user with this email already exists")
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, apperr.Internal("could not hash password").Wrap(err)
	}
	active := true
	if in.IsActive != nil {
		active = *in.IsActive
	}
	u := &models.User{
		Email:        in.Email,
		PasswordHash: hash,
		FullName:     in.FullName,
		Role:         in.Role,
		SellerID:     in.SellerID,
		IsActive:     active,
		Permissions:  perms,
	}
	// A soft-deleted account still owns its email (users.email is a plain unique
	// index, with no deleted_at predicate), so inserting a second row with that
	// address just hits the constraint and surfaces as an unreadable 500. The
	// only way to reuse the address is to take the old row back over — and that
	// hands the old account's whole history (batches it created, QC it signed
	// off, its audit trail) to the person being created now. If the company
	// reassigned the mailbox to someone else, that is the wrong person's work
	// under the new name. So it is never silent: refuse once, name the account,
	// and let the operator confirm.
	prev, err := s.repo.User.FindDeletedByEmail(in.Email)
	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
		return nil, apperr.Internal("user lookup failed").Wrap(err)
	case prev != nil && !in.RestoreDeleted:
		return nil, apperr.New(http.StatusConflict, ErrCodeDeletedEmail, fmt.Sprintf(
			"Email này thuộc tài khoản %q (vai trò %s) đã bị xoá. Khôi phục sẽ dùng lại chính tài khoản đó và giữ toàn bộ lịch sử công việc của nó — nếu đây là người khác, hãy dùng email khác.",
			prev.FullName, prev.Role))
	case prev != nil:
		u.ID = prev.ID
		if err := s.repo.User.RestoreWith(u); err != nil {
			return nil, apperr.Internal("could not restore user").Wrap(err)
		}
		s.access.Invalidate(prev.ID)
		restored, err := s.Get(prev.ID)
		if err != nil {
			return nil, err
		}
		s.audit.Log(actor, "USER_RESTORE", "user", &restored.ID,
			"Restored previously deleted user "+restored.Email,
			models.JSONMap{
				"email": restored.Email, "role": string(restored.Role),
				"previous_full_name": prev.FullName, "previous_role": string(prev.Role),
			})
		return restored, nil
	}
	if err := s.repo.User.Create(u); err != nil {
		return nil, apperr.Internal("could not create user").Wrap(err)
	}
	s.audit.Log(actor, "USER_CREATE", "user", &u.ID, "Created user "+u.Email,
		models.JSONMap{"role": string(u.Role), "permissions": permsAudit(u.Permissions)})
	u.FillEffectivePermissions()
	return u, nil
}

// Get returns a user by id.
func (s *UserService) Get(id uint) (*models.User, error) {
	u, err := s.repo.User.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("User not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	u.FillEffectivePermissions()
	return u, nil
}

// List returns a page of users.
func (s *UserService) List(page repositories.Page) ([]models.User, int64, error) {
	users, total, err := s.repo.User.List(page.Normalize())
	for i := range users {
		users[i].FillEffectivePermissions()
	}
	return users, total, err
}

// PermissionCatalog is what the user form renders: the tickable screens and
// each role's default ticks.
type PermissionCatalog struct {
	Features     []models.Feature         `json:"features"`
	RoleDefaults map[models.Role][]string `json:"role_defaults"`
}

func (s *UserService) PermissionCatalog() PermissionCatalog {
	defaults := make(map[models.Role][]string, len(models.AllRoles))
	for _, r := range models.AllRoles {
		defaults[r] = models.RoleDefaultPerms(r)
	}
	return PermissionCatalog{Features: models.Features, RoleDefaults: defaults}
}

// Update mutates an existing user.
func (s *UserService) Update(actor Actor, id uint, in UpdateUserInput) (*models.User, error) {
	u, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	prevRole, prevPerms := u.Role, u.Permissions
	// Same line as Delete: an ADMIN must not be able to lock, re-password or
	// re-role the supervision above them.
	if prevRole == models.RoleOwner && actor.Role != models.RoleOwner {
		return nil, apperr.Forbidden("Chỉ chủ sở hữu (OWNER) mới tạo hoặc sửa được tài khoản OWNER.")
	}
	if in.FullName != nil {
		u.FullName = *in.FullName
	}
	if in.Role != nil {
		if !validRole(*in.Role) {
			return nil, apperr.BadRequest("Invalid role")
		}
		u.Role = *in.Role
	}
	switch {
	case in.Permissions.Set:
		if u.Permissions, err = resolvePerms(u.Role, in.Permissions.List); err != nil {
			return nil, err
		}
	case u.Role != prevRole:
		// A new role without new ticks starts from that role's defaults; ticks
		// customised for the old job would silently carry over otherwise.
		u.Permissions = nil
	}
	accessChanged := u.Role != prevRole || !samePerms(u.Permissions, prevPerms)
	if accessChanged {
		// Nobody but an OWNER widens their own access: an ADMIN ticking extra
		// boxes on themselves would make the ticks meaningless.
		if actor.ID == u.ID && actor.Role != models.RoleOwner {
			return nil, apperr.Forbidden("Không thể tự đổi vai trò hoặc quyền của chính mình — nhờ chủ sở hữu sửa giúp.")
		}
		if err := guardGrant(actor, u.Role, u.Permissions); err != nil {
			return nil, err
		}
	}
	if in.SellerID != nil {
		u.SellerID = in.SellerID
	}
	if in.IsActive != nil {
		u.IsActive = *in.IsActive
	}
	if in.Password != nil && *in.Password != "" {
		hash, err := auth.HashPassword(*in.Password)
		if err != nil {
			return nil, apperr.Internal("could not hash password").Wrap(err)
		}
		u.PasswordHash = hash
	}
	if err := s.ensureSellerExists(u.SellerID); err != nil {
		return nil, err
	}
	if err := s.repo.User.Update(u); err != nil {
		return nil, apperr.Internal("could not update user").Wrap(err)
	}
	s.access.Invalidate(u.ID)
	var meta models.JSONMap
	if accessChanged {
		meta = models.JSONMap{
			"role": string(u.Role), "previous_role": string(prevRole),
			"permissions": permsAudit(u.Permissions), "previous_permissions": permsAudit(prevPerms),
		}
	}
	s.audit.Log(actor, "USER_UPDATE", "user", &u.ID, "Updated user "+u.Email, meta)
	u.FillEffectivePermissions()
	return u, nil
}

func samePerms(a, b models.PermList) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Delete soft-deletes a user. The row itself stays: audit entries, batches and
// QC records point at the person who did the work, and a hard delete would turn
// all of that history into dangling ids.
//
// Three things it refuses, because each one locks somebody out of a system
// nobody can reopen from inside the app:
//   - deleting yourself (you lose your own session mid-shift),
//   - deleting the last OWNER (OWNER is the gate for user admin, quotas and
//     production corrections),
//   - an ADMIN deleting an OWNER (an admin must not be able to clear away the
//     supervision above them).
func (s *UserService) Delete(actor Actor, id uint) error {
	victim, err := s.Get(id)
	if err != nil {
		return err
	}
	if actor.ID != 0 && actor.ID == id {
		return apperr.Unprocessable("Không thể xoá chính tài khoản đang đăng nhập — nhờ chủ sở hữu khác xoá giúp.")
	}
	if victim.Role == models.RoleOwner {
		if actor.Role != models.RoleOwner {
			return apperr.Forbidden("Chỉ chủ sở hữu (OWNER) mới xoá được một tài khoản OWNER.")
		}
		owners, err := s.repo.User.CountByRole(models.RoleOwner)
		if err != nil {
			return apperr.Internal("could not count owners").Wrap(err)
		}
		if owners <= 1 {
			return apperr.Unprocessable("Đây là tài khoản OWNER duy nhất — tạo một OWNER khác trước khi xoá, nếu không sẽ không ai quản trị được hệ thống.")
		}
	}
	if err := s.repo.User.Delete(id); err != nil {
		return apperr.Internal("could not delete user").Wrap(err)
	}
	s.access.Invalidate(id)
	s.audit.Log(actor, "USER_DELETE", "user", &id, "Deleted user "+victim.Email,
		models.JSONMap{"email": victim.Email, "role": string(victim.Role), "full_name": victim.FullName})
	return nil
}
