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

// A label FFM paid for can still die on THE's side: THE staff cancel the
// package in their CMS, an unscanned label expires, a package gets archived.
// FFM used to ask THE about a paid shipment only while it still lacked the
// last-mile number, so once that number was in, nothing ever noticed — the dead
// label kept printing and the void tracking stayed on the order.
//
// Now every paid shipment is re-checked:
//   - by the poll (SyncLabeledStatus), each one at most every recheckEvery, for
//     recheckWindow after it was labeled;
//   - right before FFM USES the label: printing it, "Tạo label THE" (which would
//     otherwise hand the dead one back as "already done"), "Xuất kho";
//   - on demand from the order screen (CheckOrderOnTHE).
//
// A package THE reports dead is "voided" in FFM (voidCancelledOnTHE): the
// shipment row is closed, the order loses the carrier's number, an order that
// was already out of the warehouse goes back to "Chờ gửi hàng", and a note asks
// ops for a new label. "Xuất kho" refuses such an order until it has one, so a
// box carrying the dead label cannot leave unnoticed.

// theCancelledPrefix starts the error of a shipment FFM closed because THE
// killed its package — what export checks and the ship queue flags.
const theCancelledPrefix = "THE đã huỷ đơn"

const (
	recheckEvery  = 10 * time.Minute
	recheckWindow = 30 * 24 * time.Hour
)

// theStatusDead reports a THE package status that can no longer carry a parcel.
func theStatusDead(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "canceled", "cancelled", "archived", "expired":
		return true
	}
	return false
}

// cancelledByTHE reports a shipment row FFM closed because THE killed it.
func cancelledByTHE(sh *models.CarrierShipment) bool {
	return sh != nil && sh.Status == models.ShipmentCancelled && strings.HasPrefix(sh.Error, theCancelledPrefix)
}

// checkLabeled asks THE about one paid shipment and acts on the answer:
// dead → voided; taken back to pending → held for THE's staff (holdReturned);
// alive → THE's status is recorded and a newly assigned last-mile number is put
// on the order. known=false when THE could not be asked (nothing changed then).
func (s *CarrierService) checkLabeled(ctx context.Context, cli theAPI, sh *models.CarrierShipment, o *models.Order) (voided, known bool) {
	if sh.Status != models.ShipmentLabeled || sh.ExternalID == "" || cli == nil {
		return false, false
	}
	now := time.Now()
	cctx, cancel := context.WithTimeout(ctx, theReadTimeout)
	d, err := cli.GetPackage(cctx, sh.ExternalID)
	cancel()
	if err != nil {
		if isNotFound(err) {
			return s.voidCancelledOnTHE(sh, "không còn trên THE"), true
		}
		// THE unreachable: try again on a later round, not the very next one.
		_ = s.repo.Carrier.UpdateShipment(sh.ID, map[string]any{"status_checked_at": now})
		return false, false
	}
	status := strings.TrimSpace(d.Status.String())
	if theStatusDead(status) {
		return s.voidCancelledOnTHE(sh, status), true
	}
	if returnedToPending(d) {
		s.holdReturned(sh)
		return false, true
	}
	fields := map[string]any{"the_status": status, "status_checked_at": now}
	number, carrier := d.LastMile()
	newNumber := sh.LastMileTracking == "" && number != ""
	if newNumber {
		fields["last_mile_tracking"] = number
		if carrier != "" {
			fields["last_mile_carrier"] = carrier
		}
	}
	if err := s.repo.Carrier.UpdateShipment(sh.ID, fields); err != nil {
		return false, true
	}
	sh.TheStatus, sh.StatusCheckedAt = status, &now
	if newNumber {
		sh.LastMileTracking = number
		if o == nil {
			o, _ = s.repo.Order.FindByID(sh.OrderID)
		}
		s.setOrderTracking(o, number)
	}
	return false, true
}

// errAlreadyClosed: another worker closed the shipment first.
var errAlreadyClosed = errors.New("shipment already closed")

// voidCancelledOnTHE closes a shipment THE killed: a paid one, or one held for
// THE's staff that they cancelled instead of handling. Returns true when this
// call did it (false: someone else already had, or the write failed).
func (s *CarrierService) voidCancelledOnTHE(sh *models.CarrierShipment, theStatus string) bool {
	now := time.Now()
	code := sh.TrackingCode
	if code == "" {
		code = sh.OrderNumber
	}
	why := fmt.Sprintf("%s %s trên hệ thống THE (trạng thái THE: %s) — label và tracking cũ không dùng được nữa, phải tạo label mới",
		theCancelledPrefix, code, theStatus)
	var order *models.Order
	backToQueue := false
	err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&models.CarrierShipment{}).
			Where("id = ? AND status IN ?", sh.ID, []string{models.ShipmentLabeled, models.ShipmentCreated}).
			Updates(map[string]any{
				"status": models.ShipmentCancelled, "cancelled_at": now, "error": why,
				"the_status": theStatus, "status_checked_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errAlreadyClosed
		}
		txRepo := repositories.New(tx)
		o, err := txRepo.Order.FindByID(sh.OrderID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil // order deleted meanwhile: closing the shipment is all there is
			}
			return err
		}
		order = o
		next := "Vào Chờ gửi hàng, bấm \"1. Tạo label THE\" để lấy label mới, dán đè label cũ rồi mới Xuất kho."
		if o.SellerStatus == models.SellerStatusDelivered {
			// The carrier already reported it delivered: the parcel's journey is
			// real history, only the record is odd. Keep it, ask a human to look.
			next = "Đơn đã được báo giao thành công nên FFM giữ nguyên trạng thái và mã vận đơn. Kiểm tra lại với THE vì sao huỷ sau khi đã giao."
		} else {
			fields := map[string]interface{}{}
			// The carrier's number belonged to the dead parcel (never touch a
			// number CS typed in themselves).
			if sh.LastMileTracking != "" && strings.TrimSpace(o.TrackingNumber) == sh.LastMileTracking {
				fields["tracking_number"] = ""
				fields["tracking_status"] = models.TrackingNone
				fields["tracking_detail"] = ""
				fields["tracking_location"] = ""
				fields["tracking_raw_status"] = ""
				fields["tracking_delivered_at"] = nil
				fields["tracking_synced_at"] = nil
				fields["tracking_sync_error"] = ""
			}
			// Already out of the warehouse with a label that no longer exists: back
			// to "Chờ gửi hàng" so it gets a new label and is sent again.
			if o.SellerStatus.HandedOver() {
				fields["seller_status"] = models.SellerStatusProduction
				fields["handed_over_at"] = nil
				backToQueue = true
				next = "Đơn đã xuất kho với label cũ nên được đưa lại về Chờ gửi hàng. Tìm kiện, tạo label mới (\"1. Tạo label THE\"), dán đè label cũ rồi Xuất kho lại."
			}
			if len(fields) > 0 {
				if err := tx.Model(&models.Order{}).Where("id = ?", o.ID).Updates(fields).Error; err != nil {
					return err
				}
			}
			if backToQueue {
				_ = recordStatus(txRepo, models.EntityOrder, o.ID, string(o.SellerStatus),
					string(models.SellerStatusProduction), Actor{}, why)
			}
		}
		return txRepo.Note.Create(&models.Note{
			Title:               fmt.Sprintf("THE đã huỷ vận đơn %s — đơn %s cần label mới", code, o.InternalCode),
			Body:                why + "\n\n" + next,
			ReasonCode:          "THE_CANCELLED",
			Severity:            models.SeverityHigh,
			Status:              models.NoteOpen,
			IsRequiredAttention: true,
			EntityType:          models.EntityOrder,
			EntityID:            &o.ID,
			OwnerRole:           models.RoleOps,
		})
	})
	if err != nil {
		return false
	}
	sh.Status, sh.CancelledAt, sh.Error, sh.TheStatus, sh.StatusCheckedAt = models.ShipmentCancelled, &now, why, theStatus, &now
	meta := models.JSONMap{"shipment_id": sh.ID, "the_id": sh.ExternalID, "tracking": sh.TrackingCode,
		"last_mile": sh.LastMileTracking, "the_status": theStatus, "back_to_queue": backToQueue}
	label := "đơn"
	if order != nil {
		label = order.InternalCode
	}
	s.audit.Log(Actor{}, "THE_SHIPMENT_CANCELLED_BY_THE", "order", &sh.OrderID,
		fmt.Sprintf("THE đã huỷ vận đơn %s của %s — FFM bỏ label/tracking cũ", code, label), meta)
	return true
}

// SyncLabeledStatus is the poll: paid shipments of the last recheckWindow not
// asked about for recheckEvery are re-checked with THE. Returns how many were
// voided because THE had killed them.
func (s *CarrierService) SyncLabeledStatus(ctx context.Context, limit int) (int, error) {
	_, cli, err := s.activeTHE()
	if err != nil || cli == nil {
		return 0, err
	}
	now := time.Now()
	rows, err := s.repo.Carrier.LabeledToRecheck(limit, now.Add(-recheckWindow), now.Add(-recheckEvery))
	if err != nil {
		return 0, err
	}
	voided := 0
	for i := range rows {
		if ctx.Err() != nil {
			break
		}
		v, known := s.checkLabeled(ctx, cli, &rows[i], nil)
		if v {
			voided++
		}
		if !known {
			break // THE is not answering: do not sit through a timeout per row
		}
	}
	return voided, nil
}

// CheckOrderOnTHE re-checks the order's paid shipment with THE right now (the
// order screen's "Kiểm tra với THE") and returns the order's shipments.
func (s *CarrierService) CheckOrderOnTHE(ctx context.Context, actor Actor, orderID uint) ([]ShipmentView, error) {
	if !canSeeShipments(actor) {
		return nil, apperr.Forbidden("Bạn không có quyền xem đơn THE")
	}
	live, err := s.repo.Carrier.LiveShipment(orderID)
	if err != nil {
		return nil, apperr.Internal("could not read shipment").Wrap(err)
	}
	if live != nil && live.Status == models.ShipmentLabeled {
		_, cli, err := s.activeTHE()
		if err != nil {
			return nil, err
		}
		if cli == nil {
			return nil, apperr.Conflict("Kết nối THE đang tắt")
		}
		if _, known := s.checkLabeled(ctx, cli, live, nil); !known {
			return nil, apperr.Conflict("Chưa hỏi được THE (mạng / THE quá tải) — thử lại sau")
		}
	}
	return s.ShipmentsForOrder(actor, orderID)
}

// labelUsable re-checks a paid shipment before FFM hands its label out. An
// error says THE killed it (FFM has just voided it); THE being unreachable is
// not an error — the stored label is what there is.
func (s *CarrierService) labelUsable(ctx context.Context, sh *models.CarrierShipment) error {
	if s == nil || sh == nil || sh.Status != models.ShipmentLabeled {
		return nil
	}
	_, cli, err := s.activeTHE()
	if err != nil || cli == nil {
		return nil
	}
	if voided, _ := s.checkLabeled(ctx, cli, sh, nil); voided {
		return apperr.Conflict(theCancelledPrefix + " " + nonEmpty(sh.TrackingCode, sh.OrderNumber) +
			" trên hệ thống THE — label cũ không dùng được. Tạo label mới (Chờ gửi hàng → \"1. Tạo label THE\").")
	}
	return nil
}

// ExportLabelBlock is why "Xuất kho" must not let the order out right now: its
// paid label was killed on THE (checked live when it still looks paid). ""
// means go ahead — including an order that never had a THE label (Xuất kho does
// not require one).
func (s *CarrierService) ExportLabelBlock(ctx context.Context, orderID uint) string {
	if s == nil {
		return ""
	}
	live, err := s.repo.Carrier.LiveShipment(orderID)
	if err != nil {
		return ""
	}
	if live != nil {
		if err := s.labelUsable(ctx, live); err != nil {
			return "THE đã huỷ label của đơn này — tạo label mới (\"1. Tạo label THE\") và dán đè lên kiện rồi mới xuất kho"
		}
		return ""
	}
	latest, err := s.repo.Carrier.LatestShipmentsByOrder([]uint{orderID})
	if err != nil {
		return ""
	}
	if last, ok := latest[orderID]; ok && cancelledByTHE(&last) {
		return "THE đã huỷ label cũ của đơn này — tạo label mới (\"1. Tạo label THE\") và dán đè lên kiện rồi mới xuất kho"
	}
	return ""
}

func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
