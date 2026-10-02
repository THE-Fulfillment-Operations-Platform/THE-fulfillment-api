package repositories

import (
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// APIKeyRepository stores the open-API keys issued to sellers.
type APIKeyRepository struct{ db *gorm.DB }

func (r *APIKeyRepository) Create(k *models.APIKey) error { return r.db.Create(k).Error }

// ListBySeller returns every key ever issued to the seller, newest first —
// revoked ones included, they are the history.
func (r *APIKeyRepository) ListBySeller(sellerID uint) ([]models.APIKey, error) {
	var rows []models.APIKey
	err := r.db.Where("seller_id = ?", sellerID).Order("id desc").Find(&rows).Error
	return rows, err
}

// CountActive is how many of the seller's keys still work.
func (r *APIKeyRepository) CountActive(sellerID uint) (int64, error) {
	var n int64
	err := r.db.Model(&models.APIKey{}).
		Where("seller_id = ? AND revoked_at IS NULL", sellerID).Count(&n).Error
	return n, err
}

// FindForSeller loads one key, scoped to its seller so a key id from another
// seller's list is simply not found.
func (r *APIKeyRepository) FindForSeller(sellerID, id uint) (*models.APIKey, error) {
	var k models.APIKey
	if err := r.db.Where("seller_id = ? AND id = ?", sellerID, id).First(&k).Error; err != nil {
		return nil, err
	}
	return &k, nil
}

// Revoke stamps revoked_at once. It reports whether THIS call revoked the key,
// so revoking twice does not move the timestamp.
func (r *APIKeyRepository) Revoke(id uint, at time.Time) (bool, error) {
	res := r.db.Model(&models.APIKey{}).
		Where("id = ? AND revoked_at IS NULL", id).
		Update("revoked_at", at)
	return res.RowsAffected > 0, res.Error
}

// APIKeyLookup is what authenticating a request needs: the key and the seller
// behind it, in one row.
type APIKeyLookup struct {
	KeyID        uint
	KeyName      string
	Prefix       string
	RevokedAt    *time.Time
	SellerID     uint
	SellerCode   string
	SellerName   string
	SellerStatus string
}

// LookupByHash resolves a key hash to its key + seller in one query. The join
// only matches a live seller, so deleting a seller ends its keys with it.
// found=false means no such key (or its seller is gone).
func (r *APIKeyRepository) LookupByHash(hash string) (APIKeyLookup, bool, error) {
	var out []APIKeyLookup
	err := r.db.Table("api_keys").
		Select("api_keys.id AS key_id, api_keys.name AS key_name, api_keys.prefix AS prefix, "+
			"api_keys.revoked_at AS revoked_at, sellers.id AS seller_id, sellers.code AS seller_code, "+
			"sellers.name AS seller_name, sellers.status AS seller_status").
		Joins("JOIN sellers ON sellers.id = api_keys.seller_id AND sellers.deleted_at IS NULL").
		Where("api_keys.key_hash = ? AND api_keys.deleted_at IS NULL", hash).
		Limit(1).
		Scan(&out).Error
	if err != nil || len(out) == 0 {
		return APIKeyLookup{}, false, err
	}
	return out[0], true, nil
}

// TouchLastUsed records that the key was used. UpdateColumn so the row's
// updated_at keeps meaning "the key itself was changed".
func (r *APIKeyRepository) TouchLastUsed(id uint, at time.Time) error {
	return r.db.Model(&models.APIKey{}).Where("id = ?", id).UpdateColumn("last_used_at", at).Error
}
