package models

import "time"

// APIKey lets a seller's own system call the open API (/api/open/v1) without a
// person logging in. One key belongs to exactly one seller, so the seller is
// never something the caller states — it comes from the key.
//
// Only the SHA-256 of the key is stored: the plain key is shown once, when it is
// created, and cannot be read back. Prefix is the first characters of the plain
// key, kept so the list can tell keys apart ("ffm_3fa9c2d1…").
//
// A key is never deleted, only revoked: the row is the record of who could send
// orders and until when.
type APIKey struct {
	Base
	SellerID    uint       `json:"seller_id" gorm:"index;not null"`
	Name        string     `json:"name" gorm:"size:120;not null"`
	Prefix      string     `json:"prefix" gorm:"size:24;not null"`
	KeyHash     string     `json:"-" gorm:"uniqueIndex;size:64;not null"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	CreatedByID *uint      `json:"created_by_id"`
}

func (APIKey) TableName() string { return "api_keys" }

// APIPrincipal is who an open-API request is: the key it presented and the
// seller that key belongs to. It plays the role auth.Claims plays for a logged-in
// user.
type APIPrincipal struct {
	KeyID      uint
	KeyName    string
	KeyPrefix  string
	SellerID   uint
	SellerCode string
	SellerName string
}
