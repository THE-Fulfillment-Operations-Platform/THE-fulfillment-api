package services

import (
	"errors"
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
