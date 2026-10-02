package services

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

const (
	// apiKeyMarker starts every key, so one pasted into a chat or a log is
	// recognisable for what it is.
	apiKeyMarker = "ffm_"
	// apiKeyRandomBytes of entropy behind each key (hex-encoded: 48 characters).
	apiKeyRandomBytes = 24
	// apiKeyPrefixLen is how much of the plain key is kept for the list: the
	// marker plus 8 hex characters.
	apiKeyPrefixLen = len(apiKeyMarker) + 8
	// MaxActiveAPIKeys per seller. A seller needs one key per system that connects;
	// a longer list is keys nobody remembers issuing.
	MaxActiveAPIKeys = 5

	// apiKeyTTL bounds how long a resolved key is trusted without re-reading it.
	// Revoking through this process drops it at once; the TTL only matters for a
	// second instance during a rollout, or a change made straight in the database.
	apiKeyTTL = 30 * time.Second
	// apiKeyTouchEvery throttles the last_used_at write: "is this key in use" needs
	// minutes of precision, not a database write on every request.
	apiKeyTouchEvery = 5 * time.Minute
)

// The open API answers an authentication failure with its own codes, so the
// caller's system can tell "the key is wrong" from "the key is fine but the
// seller is paused" without parsing a sentence.
var (
	errAPIKeyInvalid = apperr.New(http.StatusUnauthorized, "API_KEY_INVALID",
		"API key không hợp lệ hoặc đã bị thu hồi")
	errSellerPaused = apperr.New(http.StatusForbidden, "SELLER_PAUSED",
		"Seller đang tạm dừng — liên hệ xưởng để mở lại")
)

// APIKeyService issues, lists and revokes sellers' open-API keys, and resolves
// the key a request presents.
type APIKeyService struct {
	repo  *repositories.Repositories
	audit *AuditService

	mu      sync.Mutex
	cache   map[string]apiKeyEntry // by key hash
	touched map[uint]time.Time     // key id → last last_used_at write
	now     func() time.Time
}

type apiKeyEntry struct {
	principal *models.APIPrincipal
	expires   time.Time
}

func NewAPIKeyService(repo *repositories.Repositories, audit *AuditService) *APIKeyService {
	return &APIKeyService{
		repo: repo, audit: audit,
		cache: map[string]apiKeyEntry{}, touched: map[uint]time.Time{}, now: time.Now,
	}
}

// hashAPIKey is what is stored and looked up. Plain SHA-256, no salt or slow
// hash: the key is 192 random bits, so there is nothing to brute-force, and the
// lookup has to be a single indexed equality on every request.
func hashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func newAPIKey() (raw string, err error) {
	buf := make([]byte, apiKeyRandomBytes)
	if _, err = rand.Read(buf); err != nil {
		return "", err
	}
	return apiKeyMarker + hex.EncodeToString(buf), nil
}

// CreatedAPIKey is the one response that carries the plain key.
type CreatedAPIKey struct {
	models.APIKey
	// Key is the plain key. It exists only in this response — the database keeps
	// its hash — so whoever creates it must copy it now.
	Key string `json:"key"`
}

// Create issues a new key for the seller and returns it in the clear, once.
func (s *APIKeyService) Create(actor Actor, sellerID uint, name string) (*CreatedAPIKey, error) {
	seller, found, err := s.repo.Seller.IdentityByID(sellerID)
	if err != nil {
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	if !found {
		return nil, apperr.NotFound("Seller not found")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, apperr.Unprocessable("Nhập tên cho key — ví dụ tên hệ thống sẽ dùng nó")
	}
	if len([]rune(name)) > 120 {
		return nil, apperr.Unprocessable("Tên key tối đa 120 ký tự")
	}
	active, err := s.repo.APIKey.CountActive(sellerID)
	if err != nil {
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	if active >= MaxActiveAPIKeys {
		return nil, apperr.Conflict("Seller này đã có " + itoa(MaxActiveAPIKeys) +
			" key đang hoạt động — thu hồi key không dùng trước khi tạo thêm")
	}

	raw, err := newAPIKey()
	if err != nil {
		return nil, apperr.Internal("could not generate key").Wrap(err)
	}
	key := &models.APIKey{
		SellerID: sellerID, Name: name, Prefix: raw[:apiKeyPrefixLen],
		KeyHash: hashAPIKey(raw), CreatedByID: actor.IDPtr(),
	}
	if err := s.repo.APIKey.Create(key); err != nil {
		return nil, apperr.Internal("could not create key").Wrap(err)
	}
	s.audit.Log(actor, "API_KEY_CREATE", "seller", &sellerID,
		"Created API key "+key.Prefix+"… ("+name+") for seller "+seller.Code,
		map[string]interface{}{"api_key_id": key.ID})
	return &CreatedAPIKey{APIKey: *key, Key: raw}, nil
}

// List returns the seller's keys, revoked ones included.
func (s *APIKeyService) List(sellerID uint) ([]models.APIKey, error) {
	exists, err := s.repo.Seller.Exists(sellerID)
	if err != nil {
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	if !exists {
		return nil, apperr.NotFound("Seller not found")
	}
	rows, err := s.repo.APIKey.ListBySeller(sellerID)
	if err != nil {
		return nil, apperr.Internal("could not list keys").Wrap(err)
	}
	return rows, nil
}

// Revoke ends a key. Requests already past authentication finish; the next one
// with this key is refused. Revoking an already revoked key is a no-op.
func (s *APIKeyService) Revoke(actor Actor, sellerID, keyID uint) (*models.APIKey, error) {
	key, err := s.repo.APIKey.FindForSeller(sellerID, keyID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("API key not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	now := s.now()
	revoked, err := s.repo.APIKey.Revoke(key.ID, now)
	if err != nil {
		return nil, apperr.Internal("could not revoke key").Wrap(err)
	}
	// Drop it from the cache whether or not this call was the one that revoked
	// it: a stale entry is the only way a revoked key keeps working.
	s.forget(key.ID)
	if revoked {
		key.RevokedAt = &now
		s.audit.Log(actor, "API_KEY_REVOKE", "seller", &sellerID,
			"Revoked API key "+key.Prefix+"… ("+key.Name+")",
			map[string]interface{}{"api_key_id": key.ID})
	}
	return key, nil
}

func (s *APIKeyService) forget(keyID uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, e := range s.cache {
		if e.principal.KeyID == keyID {
			delete(s.cache, hash)
		}
	}
}

// ResolveAPIKey turns the key a request presented into the seller it acts for,
// or an error the middleware answers with as-is. It is the open API's
// counterpart to parsing a JWT and loading the user's access.
func (s *APIKeyService) ResolveAPIKey(raw string) (*models.APIPrincipal, error) {
	raw = strings.TrimSpace(raw)
	// Anything that is not shaped like one of our keys — a JWT from the web app,
	// say — is refused without a database round trip.
	if !strings.HasPrefix(raw, apiKeyMarker) || len(raw) != len(apiKeyMarker)+apiKeyRandomBytes*2 {
		return nil, errAPIKeyInvalid
	}
	hash := hashAPIKey(raw)
	now := s.now()

	s.mu.Lock()
	e, ok := s.cache[hash]
	s.mu.Unlock()
	if ok && now.Before(e.expires) {
		s.touch(e.principal.KeyID, now)
		return e.principal, nil
	}

	row, found, err := s.repo.APIKey.LookupByHash(hash)
	if err != nil {
		return nil, apperr.Internal("could not verify API key").Wrap(err)
	}
	if !found || row.RevokedAt != nil {
		return nil, errAPIKeyInvalid
	}
	// A refusal is never cached, so re-activating a paused seller works on the
	// very next request. (Pausing one takes up to apiKeyTTL to bite: an already
	// resolved key is trusted until its cache entry expires.)
	if strings.EqualFold(strings.TrimSpace(row.SellerStatus), "paused") {
		return nil, errSellerPaused
	}
	p := &models.APIPrincipal{
		KeyID: row.KeyID, KeyName: row.KeyName, KeyPrefix: row.Prefix,
		SellerID: row.SellerID, SellerCode: row.SellerCode, SellerName: row.SellerName,
	}
	s.mu.Lock()
	s.cache[hash] = apiKeyEntry{principal: p, expires: now.Add(apiKeyTTL)}
	s.mu.Unlock()
	s.touch(p.KeyID, now)
	return p, nil
}

// touch records the key's use, at most once per apiKeyTouchEvery. A failed write
// is dropped: the request it rode on is still a valid request.
func (s *APIKeyService) touch(keyID uint, now time.Time) {
	s.mu.Lock()
	last, seen := s.touched[keyID]
	due := !seen || now.Sub(last) >= apiKeyTouchEvery
	if due {
		s.touched[keyID] = now
	}
	s.mu.Unlock()
	if due {
		_ = s.repo.APIKey.TouchLastUsed(keyID, now)
	}
}
