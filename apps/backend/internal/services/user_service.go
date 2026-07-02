package services

import (
	"errors"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// UserService manages user accounts (admin/owner only at the route layer).
type UserService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// CreateUserInput is the create payload.
type CreateUserInput struct {
	Email    string      `json:"email" binding:"required,email"`
	Password string      `json:"password" binding:"required,min=6"`
	FullName string      `json:"full_name" binding:"required"`
	Role     models.Role `json:"role" binding:"required"`
	SellerID *uint       `json:"seller_id"`
	IsActive *bool       `json:"is_active"`
}

// UpdateUserInput is the update payload (all optional).
type UpdateUserInput struct {
	FullName *string      `json:"full_name"`
	Password *string      `json:"password"`
	Role     *models.Role `json:"role"`
	SellerID *uint        `json:"seller_id"`
	IsActive *bool        `json:"is_active"`
}

func validRole(r models.Role) bool {
	for _, role := range models.AllRoles {
		if role == r {
			return true
		}
	}
	return false
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
	}
	if err := s.repo.User.Create(u); err != nil {
		return nil, apperr.Internal("could not create user").Wrap(err)
	}
	s.audit.Log(actor, "USER_CREATE", "user", &u.ID, "Created user "+u.Email, nil)
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
	return u, nil
}

// List returns a page of users.
func (s *UserService) List(page repositories.Page) ([]models.User, int64, error) {
	return s.repo.User.List(page.Normalize())
}

// Update mutates an existing user.
func (s *UserService) Update(actor Actor, id uint, in UpdateUserInput) (*models.User, error) {
	u, err := s.Get(id)
	if err != nil {
		return nil, err
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
	if err := s.repo.User.Update(u); err != nil {
		return nil, apperr.Internal("could not update user").Wrap(err)
	}
	s.audit.Log(actor, "USER_UPDATE", "user", &u.ID, "Updated user "+u.Email, nil)
	return u, nil
}

// Delete soft-deletes a user.
func (s *UserService) Delete(actor Actor, id uint) error {
	if _, err := s.Get(id); err != nil {
		return err
	}
	if err := s.repo.User.Delete(id); err != nil {
		return apperr.Internal("could not delete user").Wrap(err)
	}
	s.audit.Log(actor, "USER_DELETE", "user", &id, "Deleted user", nil)
	return nil
}
