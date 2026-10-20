package models

import "time"

// MaxBaskets is how many physical waiting baskets (giỏ chờ) the floor has. A new
// basket always takes the LOWEST free number in 1..MaxBaskets.
const MaxBaskets = 100

// Basket (giỏ chờ) holds the finished products of a multi-product order while
// the rest of the order is still being made. The first product to pass QC goes
// into a basket; every later product of the same order is told which basket to
// join; when the last one passes, the order is complete and a picker bundles the
// basket and confirms with a scan — only then is the basket released and its
// number free again.
//
// A basket is "live" while ReleasedAt is NULL. At most one live basket per number
// and one per order — enforced by the two partial unique indexes below, so two QC
// stations creating a basket at the same instant can never share a number or
// give one order two baskets.
type Basket struct {
	Base
	Number      int    `json:"number" gorm:"not null;index:uniq_baskets_live_number,unique,where:released_at IS NULL AND deleted_at IS NULL"`
	OrderID     uint   `json:"order_id" gorm:"not null;index:uniq_baskets_live_order,unique,where:released_at IS NULL AND deleted_at IS NULL"`
	Order       *Order `json:"order,omitempty" gorm:"foreignKey:OrderID"`
	CreatedByID *uint  `json:"created_by_id"`
	// Released: the picker confirmed the bundle (or an admin freed the basket of a
	// cancelled order). History is kept — a released row is never reused.
	ReleasedAt   *time.Time `json:"released_at" gorm:"index"`
	ReleasedByID *uint      `json:"released_by_id"`
	ReleaseNote  string     `json:"release_note" gorm:"size:255"`
}

func (Basket) TableName() string { return "baskets" }
