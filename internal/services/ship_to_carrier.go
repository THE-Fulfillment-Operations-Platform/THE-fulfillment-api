package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// THE is the shipment created on THE for this order (nil when the
	// integration is off or the send was a manual handoff).
	THE *ShipmentOutcome `json:"the,omitempty"`
}

type SkippedShipOrder struct {
	OrderID      uint   `json:"order_id"`
	InternalCode string `json:"internal_code"`
	Reason       string `json:"reason"`
	// Code tells the screen which next step fits (ShipErr*): e.g. ADDRESS →
	// offer "send without the address check".
	Code string `json:"code,omitempty"`
}

// MaxShipBatch caps one bulk send. Large enough for a full day's output, small
// enough that the transaction and the audit trail stay comprehensible.
const MaxShipBatch = 200

// MaxTHEShipBatch caps one send that creates THE shipments: each order costs
// two or three THE calls (up to ~80 s for a slow delivery), so the screen sends
// small chunks and shows progress instead of one request that runs for minutes.
const MaxTHEShipBatch = 10

// ShipSendOptions: ShipOptions (THE choices) plus ManualHandoff — record the
// handoff WITHOUT creating a THE shipment, for orders THE cannot take through
// the API (a country/service it refuses) while the integration is on.
type ShipSendOptions struct {
	ShipOptions
	ManualHandoff bool `json:"manual_handoff"`
}

// ShipOrdersToCarrier hands finished orders to the carrier.
//
// An order qualifies when it is approved, not cancelled, has at least one live
// item, and every live item has passed QC. Anything else is skipped with a
// reason rather than silently dropped.
func (s *PackingService) ShipOrdersToCarrier(actor Actor, orderIDs []uint) (*ShipToCarrierResult, error) {
	return s.ShipOrdersToCarrierWith(context.Background(), actor, orderIDs, ShipSendOptions{})
}

// ShipOrdersToCarrierWith is ShipOrdersToCarrier with the operator's choices.
// When the THE integration is on, each order first gets a paid THE shipment
// (CarrierService.ShipOrderOnTHE) and only then the handoff is recorded — an
// order THE refused stays in the queue with the reason.
func (s *PackingService) ShipOrdersToCarrierWith(ctx context.Context, actor Actor, orderIDs []uint, opts ShipSendOptions) (*ShipToCarrierResult, error) {
	if !canShipToCarrier(actor) {
		return nil, apperr.Forbidden("Bạn không có quyền gửi hàng cho THE")
	}
	orderIDs = dedupeIDs(orderIDs)
	if len(orderIDs) == 0 {
		return nil, apperr.BadRequest("Chưa chọn đơn nào để gửi")
	}
	if len(orderIDs) > MaxShipBatch {
		return nil, apperr.BadRequest(fmt.Sprintf("Chọn tối đa %d đơn mỗi lần", MaxShipBatch))
	}
	cfg, cli, err := s.theFor(opts)
	if err != nil {
		return nil, err
	}
	if cli != nil && len(orderIDs) > MaxTHEShipBatch {
		return nil, apperr.BadRequest(fmt.Sprintf("Khi tạo đơn THE, gửi tối đa %d đơn mỗi lượt", MaxTHEShipBatch))
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

		var outcome *ShipmentOutcome
		if cli != nil {
			outcome, err = s.the.ShipOrderOnTHE(ctx, actor, cfg, cli, order, opts.ShipOptions)
			if err != nil {
				out.Skipped = append(out.Skipped, skippedFor(order, err))
				continue
			}
		}
		handoff, err := s.shipOne(actor, order)
		if err != nil {
			reason := err.Error()
			if outcome != nil {
				// Paid on THE, handoff not written: resending reuses the paid
				// shipment and only writes the handoff.
				reason = "Đã tạo đơn THE " + outcome.TrackingCode + " nhưng chưa ghi được bàn giao — bấm gửi lại (không tạo đơn THE mới)"
			}
			out.Skipped = append(out.Skipped, SkippedShipOrder{
				OrderID: id, InternalCode: order.InternalCode, Reason: reason,
			})
			continue
		}
		if outcome != nil {
			s.the.FollowUp(ctx, cli, order.ID, outcome)
		}
		if opts.ManualHandoff && s.the.Enabled() {
			s.audit.Log(actor, "ORDER_SHIP_MANUAL", "order", &order.ID,
				"Đơn "+order.InternalCode+" ghi nhận bàn giao THE thủ công, không tạo đơn THE qua API", nil)
		}
		out.Shipped = append(out.Shipped, ShippedOrder{
			OrderID: id, InternalCode: order.InternalCode, HandoffCode: handoff.Code, THE: outcome,
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

// ScannedShip is the outcome of one scan at the ship station: which order the
// code named and the handoff it produced. One scan, one order, one answer —
// there is no shipped/skipped split here because a scan that cannot ship is an
// error the operator must see immediately, not a row in a report.
type ScannedShip struct {
	OrderID      uint   `json:"order_id"`
	InternalCode string `json:"internal_code"`
	StoreOrderID string `json:"store_order_id"`
	SellerName   string `json:"seller_name,omitempty"`
	HandoffCode  string `json:"handoff_code"`
	// THE is the shipment the scan created on THE (nil when the integration is
	// off).
	THE *ShipmentOutcome `json:"the,omitempty"`
}

// ShipScannedOrder ships exactly one order, identified by whatever the ship
// station's scanner just read, applying the same qualification rules as the
// bulk action. This turns "tick boxes, press send" into "scan the parcel" — the
// scan itself is the confirmation.
func (s *PackingService) ShipScannedOrder(actor Actor, code string) (*ScannedShip, error) {
	return s.ShipScannedOrderWith(context.Background(), actor, code, ShipSendOptions{})
}

// ShipScannedOrderWith is ShipScannedOrder with the operator's choices; with
// the THE integration on, the scan creates the paid THE shipment first.
func (s *PackingService) ShipScannedOrderWith(ctx context.Context, actor Actor, code string, opts ShipSendOptions) (*ScannedShip, error) {
	if !canShipToCarrier(actor) {
		return nil, apperr.Forbidden("Bạn không có quyền gửi hàng cho THE")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, apperr.BadRequest("Hãy quét hoặc nhập mã nội bộ của đơn")
	}

	order, err := s.orderByScanCode(code)
	if err != nil {
		return nil, err
	}
	if reason := shipBlockReason(order); reason != "" {
		// 409, not 400: the code was valid and named a real order — it is the
		// order's state that refuses the scan.
		return nil, apperr.Conflict(reason)
	}
	cfg, cli, err := s.theFor(opts)
	if err != nil {
		return nil, err
	}
	var outcome *ShipmentOutcome
	if cli != nil {
		if outcome, err = s.the.ShipOrderOnTHE(ctx, actor, cfg, cli, order, opts.ShipOptions); err != nil {
			var se *ShipError
			if errors.As(err, &se) {
				return nil, apperr.Conflict(se.Message)
			}
			return nil, err
		}
	}

	handoff, err := s.shipOne(actor, order)
	if err != nil {
		if outcome != nil {
			return nil, apperr.Internal("Đã tạo đơn THE " + outcome.TrackingCode + " nhưng chưa ghi được bàn giao — quét lại (không tạo đơn THE mới)").Wrap(err)
		}
		return nil, err
	}
	if outcome != nil {
		s.the.FollowUp(ctx, cli, order.ID, outcome)
	}
	s.tracking.RegisterOrderAsync(order)
	return &ScannedShip{
		OrderID:      order.ID,
		InternalCode: order.InternalCode,
		StoreOrderID: order.StoreOrderID,
		SellerName:   order.Seller.Name,
		HandoffCode:  handoff.Code,
		THE:          outcome,
	}, nil
}

// theFor resolves the THE integration for one send: (nil, nil, nil) when it is
// off or the operator chose a manual handoff.
func (s *PackingService) theFor(opts ShipSendOptions) (*models.CarrierConfig, theAPI, error) {
	if opts.ManualHandoff || s.the == nil {
		return nil, nil, nil
	}
	return s.the.activeTHE()
}

// skippedFor turns a THE refusal into a skipped row with its next-step code.
func skippedFor(o *models.Order, err error) SkippedShipOrder {
	row := SkippedShipOrder{OrderID: o.ID, InternalCode: o.InternalCode, Reason: err.Error()}
	var se *ShipError
	if errors.As(err, &se) {
		row.Reason, row.Code = se.Message, se.Code
	} else if ae, ok := apperr.As(err); ok {
		row.Reason = ae.Message
	}
	return row
}

// orderByScanCode resolves what the scanner read into an order. The QR on the
// order sheet carries the order's internal code ("100048"); the tem on each
// product carries the item code ("100048_1/3"). Both name exactly one order, so
// both are accepted — the operator scans whichever label is on top.
func (s *PackingService) orderByScanCode(code string) (*models.Order, error) {
	id, found, err := s.repo.Order.IDByInternalCode(code)
	if err != nil {
		return nil, err
	}
	if !found {
		if id, found, err = s.repo.OrderItem.OrderIDByCode(code); err != nil {
			return nil, err
		}
	}
	if !found {
		return nil, apperr.NotFound("Không tìm thấy đơn nào khớp mã vừa quét")
	}
	return s.repo.Order.FindByID(id)
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
		if order.HandedOverAt == nil {
			order.HandedOverAt = &now
		}
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

// canShipToCarrier: sending finished goods out is the "Chờ gửi hàng" screen's
// action — by role default OWNER, ADMIN, OPS, PACKING and SHIPPING, not QC or
// the print floor.
func canShipToCarrier(a Actor) bool {
	return a.Can(models.Manage(models.FeatShipQueue))
}

// (dedupeIDs lives in catalog_service.go — same job, same package.)
