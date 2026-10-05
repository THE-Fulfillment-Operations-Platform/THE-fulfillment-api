package repositories

import (
	"errors"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"the-fulfillment/backend/internal/models"
)

// CarrierRepository stores the carrier connection (one row per provider) and
// the shipments FFM created on the carrier.
type CarrierRepository struct{ db *gorm.DB }

// ErrLiveShipmentExists is returned by ClaimShipment when the order already has
// a live shipment row — the one-per-order index refused a second one.
var ErrLiveShipmentExists = errors.New("carrier: order already has a live shipment")

// Config returns the provider's connection row, or (nil, nil) when none was
// ever saved.
func (r *CarrierRepository) Config(provider string) (*models.CarrierConfig, error) {
	var c models.CarrierConfig
	err := r.db.Where("provider = ?", provider).Limit(1).Find(&c).Error
	if err != nil {
		return nil, err
	}
	if c.ID == 0 {
		return nil, nil
	}
	return &c, nil
}

// SaveConfig inserts or updates the provider's connection row.
func (r *CarrierRepository) SaveConfig(c *models.CarrierConfig) error {
	return r.db.Save(c).Error
}

// LiveShipment is the order's live shipment (any state but CANCELLED/FAILED),
// or (nil, nil).
func (r *CarrierRepository) LiveShipment(orderID uint) (*models.CarrierShipment, error) {
	var s models.CarrierShipment
	err := r.db.Where("order_id = ? AND status NOT IN ?", orderID,
		[]string{models.ShipmentCancelled, models.ShipmentFailed}).
		Order("id DESC").Limit(1).Find(&s).Error
	if err != nil {
		return nil, err
	}
	if s.ID == 0 {
		return nil, nil
	}
	return &s, nil
}

// ShipmentsForOrder lists every shipment row of an order, newest first (the
// order detail shows the history: a cancelled one, then the live one).
func (r *CarrierRepository) ShipmentsForOrder(orderID uint) ([]models.CarrierShipment, error) {
	var rows []models.CarrierShipment
	err := r.db.Omit("label").Where("order_id = ?", orderID).Order("id DESC").Find(&rows).Error
	return rows, err
}

// LiveShipmentsByOrder maps order id → its live shipment (label omitted), for
// lists that show the THE tracking next to each order.
func (r *CarrierRepository) LiveShipmentsByOrder(orderIDs []uint) (map[uint]models.CarrierShipment, error) {
	out := map[uint]models.CarrierShipment{}
	if len(orderIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(orderIDs); start += idChunk {
		end := start + idChunk
		if end > len(orderIDs) {
			end = len(orderIDs)
		}
		var rows []models.CarrierShipment
		if err := r.db.Omit("label").Where("order_id IN ? AND status NOT IN ?", orderIDs[start:end],
			[]string{models.ShipmentCancelled, models.ShipmentFailed}).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, s := range rows {
			out[s.OrderID] = s
		}
	}
	return out, nil
}

// ClaimShipment inserts a CREATING row for the order. The partial unique index
// uniq_carrier_shipments_live_order turns a concurrent second claim into
// ErrLiveShipmentExists — the guard that stands between a double click and a
// second paid label.
func (r *CarrierRepository) ClaimShipment(s *models.CarrierShipment) error {
	err := r.db.Create(s).Error
	if err != nil && isUniqueViolation(err) {
		return ErrLiveShipmentExists
	}
	return err
}

// UpdateShipment writes the given columns of a shipment row.
func (r *CarrierRepository) UpdateShipment(id uint, fields map[string]any) error {
	return r.db.Model(&models.CarrierShipment{}).Where("id = ?", id).Updates(fields).Error
}

// ShipmentByID loads one shipment (label included).
func (r *CarrierRepository) ShipmentByID(id uint) (*models.CarrierShipment, error) {
	var s models.CarrierShipment
	if err := r.db.First(&s, id).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

// AwaitingLastMile lists labeled shipments that still lack the last-mile
// tracking (USPS…), oldest first, created after `since` — the poll that fills
// the order's tracking number once the carrier has assigned it.
func (r *CarrierRepository) AwaitingLastMile(limit int) ([]models.CarrierShipment, error) {
	var rows []models.CarrierShipment
	err := r.db.Omit("label").
		Where("status = ? AND last_mile_tracking = ''", models.ShipmentLabeled).
		Order("labeled_at ASC").Limit(limit).Find(&rows).Error
	return rows, err
}

// LockShipment re-reads a shipment row FOR UPDATE inside tx (Postgres; a no-op
// clause on SQLite) so two workers never advance the same row at once.
func LockShipment(tx *gorm.DB, id uint) (*models.CarrierShipment, error) {
	var s models.CarrierShipment
	q := tx
	if tx.Dialector.Name() == "postgres" {
		q = tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := q.First(&s, id).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

// isUniqueViolation recognises a unique-index refusal on Postgres (SQLSTATE
// 23505) and SQLite.
func isUniqueViolation(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "23505") || strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "unique constraint")
}
