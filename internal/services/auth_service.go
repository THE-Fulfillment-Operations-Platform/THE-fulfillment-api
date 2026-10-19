package services

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// AuthService handles login and current-user lookups.
type AuthService struct {
	repo  *repositories.Repositories
	jwt   *auth.Manager
	audit *AuditService
}

// LoginResult is returned to the handler after a successful login.
type LoginResult struct {
	Token     string       `json:"token"`
	ExpiresAt time.Time    `json:"expires_at"`
	User      *models.User `json:"user"`
}

// Login validates credentials and issues a JWT.
func (s *AuthService) Login(email, password string, ip string) (*LoginResult, error) {
	// The "email" is a free-form login name (no longer required to look like an
	// address); accounts store it lower-cased and trimmed, so match the same way.
	email = strings.ToLower(strings.TrimSpace(email))
	user, err := s.repo.User.FindByEmail(email)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.Unauthorized("Invalid email or password")
		}
		return nil, apperr.Internal("login failed").Wrap(err)
	}
	if !user.IsActive {
		return nil, apperr.Forbidden("Account is disabled")
	}
	if !auth.CheckPassword(user.PasswordHash, password) {
		return nil, apperr.Unauthorized("Invalid email or password")
	}

	token, expiresAt, err := s.jwt.Issue(user)
	if err != nil {
		return nil, apperr.Internal("could not issue token").Wrap(err)
	}

	s.audit.Log(Actor{ID: user.ID, Email: user.Email, Role: user.Role, IP: ip},
		"AUTH_LOGIN", "user", &user.ID, "User logged in", nil)

	user.FillEffectivePermissions()
	return &LoginResult{Token: token, ExpiresAt: expiresAt, User: user}, nil
}

// Me returns the current user.
func (s *AuthService) Me(userID uint) (*models.User, error) {
	user, err := s.repo.User.FindByID(userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("User not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	user.FillEffectivePermissions()
	return user, nil
}
