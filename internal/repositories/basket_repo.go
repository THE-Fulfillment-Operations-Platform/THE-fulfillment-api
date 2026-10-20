package repositories

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// BasketRepository stores the waiting baskets (giỏ chờ). Everything here reads
// "live" baskets — released_at IS NULL — unless it says otherwise.
type BasketRepository struct{ db *gorm.DB }

func (r *BasketRepository) live() *gorm.DB {
	return r.db.Model(&models.Basket{}).Where("released_at IS NULL")
}

// LiveNumbers lists the numbers currently in use.
func (r *BasketRepository) LiveNumbers() ([]int, error) {
	var out []int
	err := r.live().Pluck("number", &out).Error
	return out, err
}

// ListLive returns every live basket, lowest number first.
func (r *BasketRepository) ListLive() ([]models.Basket, error) {
	var out []models.Basket
	err := r.live().Order("number ASC").Find(&out).Error
	return out, err
}

// LiveByOrderIDs maps order id → its live basket, for the orders that have one.
func (r *BasketRepository) LiveByOrderIDs(ids []uint) (map[uint]models.Basket, error) {
	out := map[uint]models.Basket{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []models.Basket
	if err := r.live().Where("order_id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, b := range rows {
		out[b.OrderID] = b
	}
	return out, nil
}

// LiveByOrder returns the order's live basket, or nil when it has none.
func (r *BasketRepository) LiveByOrder(orderID uint) (*models.Basket, error) {
	return r.first(r.live().Where("order_id = ?", orderID))
}

// LiveByNumber returns the live basket carrying number n, or nil.
func (r *BasketRepository) LiveByNumber(n int) (*models.Basket, error) {
	return r.first(r.live().Where("number = ?", n))
}

// FindByID loads any basket, live or released. nil when it does not exist.
func (r *BasketRepository) FindByID(id uint) (*models.Basket, error) {
	return r.first(r.db.Model(&models.Basket{}).Where("id = ?", id))
}

func (r *BasketRepository) first(q *gorm.DB) (*models.Basket, error) {
	var b models.Basket
	if err := q.First(&b).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

// Create inserts a basket. A number or order already live fails on the partial
// unique indexes; the caller retries with a fresh number.
func (r *BasketRepository) Create(b *models.Basket) error {
	return r.db.Create(b).Error
}

// ReleaseForOrders frees the live baskets of the given orders (the orders were
// deleted — nothing will ever come to complete them).
func (r *BasketRepository) ReleaseForOrders(orderIDs []uint, by *uint, note string, at time.Time) error {
	if len(orderIDs) == 0 {
		return nil
	}
	return r.db.Model(&models.Basket{}).
		Where("order_id IN ? AND released_at IS NULL", orderIDs).
		Updates(map[string]any{"released_at": at, "released_by_id": by, "release_note": note}).Error
}

// Release frees a live basket. Returns how many rows changed: 0 means someone
// released it first.
func (r *BasketRepository) Release(id uint, by *uint, note string, at time.Time) (int64, error) {
	res := r.db.Model(&models.Basket{}).
		Where("id = ? AND released_at IS NULL", id).
		Updates(map[string]any{"released_at": at, "released_by_id": by, "release_note": note})
	return res.RowsAffected, res.Error
}
