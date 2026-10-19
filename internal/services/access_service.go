package services

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// accessTTL bounds how stale a cached access can be. Edits made through this
// process invalidate at once; the TTL only matters for a second instance during
// a rollout, or a change made straight in the database.
const accessTTL = 30 * time.Second

// ErrAccessRevoked means the token is still valid but the account behind it has
// been locked or deleted since it was issued.
var ErrAccessRevoked = errors.New("account locked or deleted")

// AccessService resolves who a request's user is RIGHT NOW — role, permissions,
// whether the account still works — from the database rather than the token, so
// an admin's change to someone's ticks applies on their next request instead of
// at their next login. A short per-user cache keeps that to about one query per
// user per 30 seconds.
type AccessService struct {
	repo  *repositories.Repositories
	mu    sync.Mutex
	cache map[uint]accessEntry
	now   func() time.Time

	// Role tick sets (roles.permissions) are re-read on the same TTL as users, so
	// a change saved through another instance reaches this one within accessTTL.
	rolesMu     sync.Mutex
	rolesLoaded time.Time
}

type accessEntry struct {
	access  *models.Access
	expires time.Time
}

func NewAccessService(repo *repositories.Repositories) *AccessService {
	return &AccessService{repo: repo, cache: map[uint]accessEntry{}, now: time.Now}
}

// LoadAccess returns the user's current access, or ErrAccessRevoked when the
// account no longer exists or is locked.
func (s *AccessService) LoadAccess(userID uint) (*models.Access, error) {
	now := s.now()
	s.mu.Lock()
	if e, ok := s.cache[userID]; ok && now.Before(e.expires) {
		s.mu.Unlock()
		return e.access, nil
	}
	s.mu.Unlock()

	u, err := s.repo.User.FindByID(userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAccessRevoked
		}
		return nil, err
	}
	if !u.IsActive {
		return nil, ErrAccessRevoked
	}
	s.refreshRoles(now)
	a := models.NewAccess(u)
	s.mu.Lock()
	s.cache[userID] = accessEntry{access: a, expires: now.Add(accessTTL)}
	s.mu.Unlock()
	return a, nil
}

// Invalidate drops a user's cached access so the next request re-reads it.
func (s *AccessService) Invalidate(userID uint) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.cache, userID)
	s.mu.Unlock()
}

// RoleTicksChanged is called after a role's tick set is saved: reload the role
// table now and drop every cached access, since everyone following that role
// changed at once.
func (s *AccessService) RoleTicksChanged() {
	if s == nil {
		return
	}
	s.rolesMu.Lock()
	s.rolesLoaded = time.Time{}
	s.rolesMu.Unlock()
	s.refreshRoles(s.now())
	s.mu.Lock()
	s.cache = map[uint]accessEntry{}
	s.mu.Unlock()
}

// refreshRoles re-reads roles.permissions when the last read is older than
// accessTTL. A failed read keeps the previous sets (built-in defaults at worst)
// and waits a full TTL before trying again rather than hammering the database.
func (s *AccessService) refreshRoles(now time.Time) {
	s.rolesMu.Lock()
	defer s.rolesMu.Unlock()
	if !s.rolesLoaded.IsZero() && now.Sub(s.rolesLoaded) < accessTTL {
		return
	}
	s.rolesLoaded = now
	if s.repo == nil || s.repo.Role == nil {
		return
	}
	overrides, err := s.repo.Role.PermissionOverrides()
	if err != nil {
		slog.Warn("access: could not load role permissions, keeping previous", "err", err)
		return
	}
	models.SetRoleOverrides(overrides)
}
