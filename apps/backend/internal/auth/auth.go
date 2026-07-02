// Package auth handles password hashing and JWT issuing/parsing.
package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"the-fulfillment/backend/internal/models"
)

// ErrInvalidToken is returned when a token cannot be validated.
var ErrInvalidToken = errors.New("invalid or expired token")

// Claims is the JWT payload carried with every authenticated request.
type Claims struct {
	UserID   uint        `json:"uid"`
	Email    string      `json:"email"`
	Role     models.Role `json:"role"`
	SellerID *uint       `json:"seller_id,omitempty"`
	jwt.RegisteredClaims
}

// Manager issues and validates JWTs.
type Manager struct {
	secret    []byte
	expiresIn time.Duration
}

// NewManager builds a JWT manager.
func NewManager(secret string, expiresIn time.Duration) *Manager {
	return &Manager{secret: []byte(secret), expiresIn: expiresIn}
}

// HashPassword returns a bcrypt hash of the plaintext password.
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword reports whether the plaintext matches the stored hash.
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// Issue creates a signed JWT for a user.
func (m *Manager) Issue(u *models.User) (string, time.Time, error) {
	expiresAt := time.Now().Add(m.expiresIn)
	claims := Claims{
		UserID:   u.ID,
		Email:    u.Email,
		Role:     u.Role,
		SellerID: u.SellerID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Subject:   u.Email,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(m.secret)
	return signed, expiresAt, err
}

// Parse validates a token string and returns its claims.
func (m *Manager) Parse(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return m.secret, nil
	})
	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
