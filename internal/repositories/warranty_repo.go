package repositories

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"the-fulfillment/backend/internal/models"
)

// WarrantyRepository is the data access for warranty orders (nút "Bảo hành").
type WarrantyRepository struct{ db *gorm.DB }

// Create stores the warranty record of a new warranty order.
func (r *WarrantyRepository) Create(w *models.OrderWarranty) error { return r.db.Create(w).Error }

// LockRoot takes a row lock on the chain's root order so two people pressing the
// button on the same chain are serialised. Only Postgres has row locks; elsewhere
// (the sqlite used by tests) it is a no-op.
func (r *WarrantyRepository) LockRoot(rootID uint) error {
	if r.db.Dialector.Name() != "postgres" {
		return nil
	}
	var id uint
	return r.db.Model(&models.Order{}).Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").Where("id = ?", rootID).Scan(&id).Error
}

// MaxSeq is the highest warranty_seq ever used in the chain, soft-deleted
// orders included: a deleted -BH2 must not hand its number to a new order.
func (r *WarrantyRepository) MaxSeq(rootID uint) (int, error) {
	var max int
	err := r.db.Unscoped().Model(&models.Order{}).
		Where("root_order_id = ?", rootID).
		Select("COALESCE(MAX(warranty_seq), 0)").Scan(&max).Error
	return max, err
}

// OpenInChain returns a warranty order of the chain that has not been delivered
// yet (and is neither cancelled nor rejected), or nil. returned lists the tracking
// statuses that mean "came back" — such an order is finished, not open, so the
// chain may be warrantied again from it.
func (r *WarrantyRepository) OpenInChain(rootID uint, returned []models.TrackingStatus) (*models.Order, error) {
	rs := make([]string, 0, len(returned)+1)
	rs = append(rs, string(models.TrackingDelivered))
	for _, t := range returned {
		rs = append(rs, string(t))
	}
	var o models.Order
	res := r.db.Where("root_order_id = ? AND warranty_seq > 0", rootID).
		Where("review_status NOT IN ?", []string{string(models.ReviewRejected), string(models.ReviewCancelled)}).
		Where("cancellation_status NOT IN ?", []string{string(models.CancellationSeller), string(models.CancellationApproved)}).
		Where("seller_status <> ?", models.SellerStatusDelivered).
		Where("tracking_status NOT IN ?", rs).
		Order("id asc").Limit(1).Find(&o)
	if res.Error != nil || res.RowsAffected == 0 {
		return nil, res.Error
	}
	return &o, nil
}

// Counts returns, for each given order, how many warranty orders hang off it: the
// whole chain for a root, the direct children for any other order.
func (r *WarrantyRepository) Counts(ids []uint) (map[uint]int, error) {
	out := map[uint]int{}
	if len(ids) == 0 {
		return out, nil
	}
	type row struct {
		ID uint
		N  int
	}
	var byRoot, byParent []row
	if err := r.db.Model(&models.Order{}).Select("root_order_id AS id, COUNT(*) AS n").
		Where("root_order_id IN ? AND warranty_seq > 0", ids).Group("root_order_id").Scan(&byRoot).Error; err != nil {
		return nil, err
	}
	if err := r.db.Model(&models.Order{}).Select("parent_order_id AS id, COUNT(*) AS n").
		Where("parent_order_id IN ? AND warranty_seq > 0", ids).Group("parent_order_id").Scan(&byParent).Error; err != nil {
		return nil, err
	}
	for _, x := range byParent {
		out[x.ID] = x.N
	}
	for _, x := range byRoot { // a root's chain count is always >= its direct children
		if x.N > out[x.ID] {
			out[x.ID] = x.N
		}
	}
	return out, nil
}

// Chain lists the warranty orders under an order (the chain for a root, the
// direct children otherwise), oldest first.
func (r *WarrantyRepository) Chain(orderID uint) ([]models.Order, error) {
	var rows []models.Order
	err := r.db.Where("warranty_seq > 0 AND (root_order_id = ? OR parent_order_id = ?)", orderID, orderID).
		Order("warranty_seq asc, id asc").Find(&rows).Error
	return rows, err
}

// ByOrderIDs maps a warranty order id to its record.
func (r *WarrantyRepository) ByOrderIDs(ids []uint) (map[uint]models.OrderWarranty, error) {
	out := map[uint]models.OrderWarranty{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []models.OrderWarranty
	if err := r.db.Where("order_id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, w := range rows {
		out[w.OrderID] = w
	}
	return out, nil
}
