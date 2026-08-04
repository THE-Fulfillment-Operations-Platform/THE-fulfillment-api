package services

import (
	"fmt"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// ShipToCarrier is the single step that ends the factory's half of an order and
// starts the shipping half.
//
// The older path (scan every item into a package, then create a handoff from
// that package) is still in the code for the stations that use it, but the
// operational reality is simpler: QC is the last thing the factory does, and
// after that an operator picks the finished orders and sends them to THE in one
// go. This file is that action — no packing scan required, many orders at once.
//
// It deliberately reuses the SAME end state as CreateHandoff (a Handoff row plus
// seller_status = HANDED_OFF) so everything downstream — the tracking gate, the
// seller view, the journey screen — keeps working through one concept instead of
// two parallel ones.

// ShipToCarrierResult reports per-order outcomes. A bulk action must never fail
// wholesale because one order in the selection was not ready: the operator ticked
// twenty boxes and deserves to know which nineteen went and why the last did not.
type ShipToCarrierResult struct {
	Shipped []ShippedOrder     `json:"shipped"`
	Skipped []SkippedShipOrder `json:"skipped"`
}

type ShippedOrder struct {
	OrderID      uint   `json:"order_id"`
	InternalCode string `json:"internal_code"`
	HandoffCode  string `json:"handoff_code"`
}

type SkippedShipOrder struct {
	OrderID      uint   `json:"order_id"`
	InternalCode string `json:"internal_code"`
	Reason       string `json:"reason"`
}

// MaxShipBatch caps one bulk send. Large enough for a full day's output, small
// enough that the transaction and the audit trail stay comprehensible.
const MaxShipBatch = 200

// ShipOrdersToCarrier hands finished orders to the carrier.
//
// An order qualifies when it is approved, not cancelled, has at least one live
// item, and every live item has passed QC. Anything else is skipped with a
// reason rather than silently dropped.
func (s *PackingService) ShipOrdersToCarrier(actor Actor, orderIDs []uint) (*ShipToCarrierResult, error) {
	if !canShipToCarrier(actor.Role) {
		return nil, apperr.Forbidden("Bạn không có quyền gửi hàng cho THE")
	}
	orderIDs = dedupeIDs(orderIDs)
	if len(orderIDs) == 0 {
		return nil, apperr.BadRequest("Chưa chọn đơn nào để gửi")
	}
	if len(orderIDs) > MaxShipBatch {
		return nil, apperr.BadRequest(fmt.Sprintf("Chọn tối đa %d đơn mỗi lần", MaxShipBatch))
	}

	out := &ShipToCarrierResult{Shipped: []ShippedOrder{}, Skipped: []SkippedShipOrder{}}
	// Orders that actually shipped, kept for the post-commit provider push.
	var pushed []*models.Order

	for _, id := range orderIDs {
		order, err := s.repo.Order.FindByID(id)
		if err != nil {
			out.Skipped = append(out.Skipped, SkippedShipOrder{OrderID: id, Reason: "Không tìm thấy đơn"})
			continue
		}
		if reason := shipBlockReason(order); reason != "" {
			out.Skipped = append(out.Skipped, SkippedShipOrder{
				OrderID: id, InternalCode: order.InternalCode, Reason: reason,
			})
			continue
		}

		handoff, err := s.shipOne(actor, order)
		if err != nil {
			out.Skipped = append(out.Skipped, SkippedShipOrder{
				OrderID: id, InternalCode: order.InternalCode, Reason: err.Error(),
			})
			continue
		}
		out.Shipped = append(out.Shipped, ShippedOrder{
			OrderID: id, InternalCode: order.InternalCode, HandoffCode: handoff.Code,
		})
		pushed = append(pushed, order)
	}

	// Only now, with the rows committed, tell the tracking provider. Orders that
	// already carry a number (CS attached it early) start being watched at once;
	// the rest wait for the periodic reverse lookup, which only considers
	// handed-over orders.
	for _, o := range pushed {
		s.tracking.RegisterOrderAsync(o)
	}
	return out, nil
}

// shipBlockReason explains, in the operator's language, why an order cannot go
// to the carrier yet. Empty string means it can.
func shipBlockReason(o *models.Order) string {
	if o.ReviewStatus != models.ReviewApproved {
		return "Đơn chưa được duyệt"
	}
	if o.CancellationStatus == models.CancellationApproved || o.CancellationStatus == models.CancellationSeller {
		return "Đơn đã huỷ"
	}
	if o.SellerStatus.HandedOver() {
		return "Đơn đã gửi cho THE rồi"
	}
	live := activeOrderItems(o.Items)
	if len(live) == 0 {
		return "Đơn không còn sản phẩm nào"
	}
	waiting := 0
	for _, it := range live {
		if it.InternalStatus != models.StatusQCPassed {
			waiting++
		}
	}
	if waiting > 0 {
		return fmt.Sprintf("Còn %d/%d sản phẩm chưa QC đạt", waiting, len(live))
	}
	return ""
}

// shipOne records the handoff for a single order inside one transaction.
func (s *PackingService) shipOne(actor Actor, order *models.Order) (*models.Handoff, error) {
	var handoff *models.Handoff
	now := time.Now()
	// Kept so a rolled-back transaction does not leave the in-memory order looking
	// handed over — the caller uses that struct to decide whether to push to the
	// tracking provider.
	previousStatus := order.SellerStatus

	err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		handoff = &models.Handoff{
			OrderID: &order.ID, Carrier: s.carrier.Name(),
			Status: models.HandoffHandedOff, HandedOffByID: actor.IDPtr(), HandedOffAt: now,
			Note: "Gửi hàng cho THE sau QC",
		}
		if err := txRepo.Handoff.Create(handoff); err != nil {
			return err
		}
		// The code embeds the id, so it can only be written after the insert.
		handoff.Code = fmt.Sprintf("THE-HO-%06d", handoff.ID)
		if err := txRepo.Handoff.Update(handoff); err != nil {
			return err
		}

		old := string(order.SellerStatus)
		order.SellerStatus = models.SellerStatusHandedOff
		if err := txRepo.Order.Update(order); err != nil {
			return err
		}
		_ = recordStatus(txRepo, models.EntityOrder, order.ID, old,
			string(models.SellerStatusHandedOff), actor, "shipped to "+s.carrier.Name())
		return nil
	})
	if err != nil {
		order.SellerStatus = previousStatus
		return nil, fmt.Errorf("không ghi được bàn giao: %w", err)
	}

	s.audit.Log(actor, "ORDER_SHIP_TO_CARRIER", "order", &order.ID,
		fmt.Sprintf("Đơn %s gửi cho %s (handoff %s)", order.InternalCode, s.carrier.Name(), handoff.Code), nil)
	return handoff, nil
}

// canShipToCarrier: sending finished goods out is an operations decision, so it
// sits with the roles that own the order — not with QC or the print floor.
func canShipToCarrier(role models.Role) bool {
	switch role {
	case models.RoleOwner, models.RoleAdmin, models.RoleOps, models.RolePacking, models.RoleShipping:
		return true
	}
	return false
}

// (dedupeIDs lives in catalog_service.go — same job, same package.)
