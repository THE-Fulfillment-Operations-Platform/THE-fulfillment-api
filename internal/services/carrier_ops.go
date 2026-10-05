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

// Thao tác quanh đơn THE: xem, in label, huỷ, kiểm tra trước khi gửi.

func canSeeShipments(a Actor) bool {
	return a.Can(models.View(models.FeatShipQueue)) || a.Can(models.View(models.FeatJourneys)) ||
		a.Can(models.View(models.FeatOrders))
}

// ShipmentView is a shipment as screens show it (no label bytes).
type ShipmentView struct {
	models.CarrierShipment
	HasLabel bool `json:"has_label"`
}

// ShipmentsForOrder lists the order's THE shipments, newest first.
func (s *CarrierService) ShipmentsForOrder(actor Actor, orderID uint) ([]ShipmentView, error) {
	if !canSeeShipments(actor) {
		return nil, apperr.Forbidden("Bạn không có quyền xem đơn THE")
	}
	rows, err := s.repo.Carrier.ShipmentsForOrder(orderID)
	if err != nil {
		return nil, apperr.Internal("could not list shipments").Wrap(err)
	}
	out := make([]ShipmentView, 0, len(rows))
	for _, r := range rows {
		out = append(out, ShipmentView{CarrierShipment: r, HasLabel: r.Status == models.ShipmentLabeled && r.TrackingCode != ""})
	}
	return out, nil
}

// LabelFile is a printable label.
type LabelFile struct {
	Data        []byte
	ContentType string
	Filename    string
}

// Label returns the label of the order's paid THE shipment: the copy FFM kept
// at delivery, or — when it has none — fetched from THE now and kept.
func (s *CarrierService) Label(ctx context.Context, actor Actor, orderID uint) (*LabelFile, error) {
	if !canSeeShipments(actor) {
		return nil, apperr.Forbidden("Bạn không có quyền in label THE")
	}
	live, err := s.repo.Carrier.LiveShipment(orderID)
	if err != nil {
		return nil, apperr.Internal("could not read shipment").Wrap(err)
	}
	if live == nil || live.Status != models.ShipmentLabeled || live.TrackingCode == "" {
		return nil, apperr.NotFound("Đơn này chưa có đơn THE đã chốt — chưa có label")
	}
	full, err := s.repo.Carrier.ShipmentByID(live.ID)
	if err != nil {
		return nil, apperr.Internal("could not read shipment").Wrap(err)
	}
	if len(full.Label) == 0 {
		c, err := s.loadConfig()
		if err != nil {
			return nil, err
		}
		cli, err := s.client(c)
		if err != nil {
			return nil, err
		}
		cctx, cancel := context.WithTimeout(ctx, theReadTimeout)
		defer cancel()
		data, ctype, err := cli.FetchLabel(cctx, full.TrackingCode)
		if err != nil {
			return nil, apperr.BadRequest("Chưa lấy được label từ THE: " + theErrorMessage(err))
		}
		full.Label, full.LabelType = data, ctype
		_ = s.repo.Carrier.UpdateShipment(full.ID, map[string]any{"label": data, "label_type": ctype})
	}
	ext := "bin"
	switch {
	case strings.Contains(full.LabelType, "pdf"):
		ext = "pdf"
	case strings.Contains(full.LabelType, "png"):
		ext = "png"
	case strings.Contains(full.LabelType, "gif"):
		ext = "gif"
	case strings.Contains(full.LabelType, "jpeg"):
		ext = "jpg"
	}
	return &LabelFile{
		Data: full.Label, ContentType: full.LabelType,
		Filename: fmt.Sprintf("label-%s-%s.%s", full.OrderNumber, full.TrackingCode, ext),
	}, nil
}

// CancelShipment cancels the order's THE shipment (Owner/Admin): a pending
// package is archived on THE; a paid one is cancelled there (THE refunds it)
// and the order goes back to the ship queue, since its parcel will not leave.
// THE refuses once it has scanned the parcel.
func (s *CarrierService) CancelShipment(ctx context.Context, actor Actor, orderID uint, reason string) (*ShipmentView, error) {
	if !canManageCarrier(actor) {
		return nil, apperr.Forbidden("Chỉ Owner/Admin huỷ được đơn THE")
	}
	live, err := s.repo.Carrier.LiveShipment(orderID)
	if err != nil {
		return nil, apperr.Internal("could not read shipment").Wrap(err)
	}
	if live == nil {
		return nil, apperr.NotFound("Đơn này không có đơn THE nào đang sống")
	}
	if live.Status == models.ShipmentCreating {
		if time.Since(live.UpdatedAt) < creatingStaleAfter {
			return nil, apperr.Conflict("Đơn THE đang được tạo — đợi 2 phút rồi thử lại")
		}
		// Never answered: nothing paid. Release the claim; a pending package THE
		// may hold for it is never delivered and is re-used / replaced on resend.
		now := time.Now()
		_ = s.repo.Carrier.UpdateShipment(live.ID, map[string]any{"status": models.ShipmentCancelled, "cancelled_at": now, "error": "Huỷ: " + reason})
		live.Status = models.ShipmentCancelled
		return &ShipmentView{CarrierShipment: *live}, nil
	}
	if strings.HasPrefix(live.Error, unclearDeliveryPrefix) && time.Since(live.UpdatedAt) < unclearDeliveryWait {
		return nil, apperr.Conflict("Lần chốt đơn trước chưa rõ kết quả — đợi vài phút rồi huỷ, để không bỏ sót một đơn đã trả tiền")
	}
	c, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	cli, err := s.client(c)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, theReadTimeout)
	defer cancel()
	if err := cli.CancelPackages(cctx, []string{live.ExternalID}); err != nil {
		return nil, apperr.BadRequest("THE không cho huỷ: " + theErrorMessage(err))
	}
	wasPaid := live.Status == models.ShipmentLabeled
	now := time.Now()
	note := "Huỷ đơn THE"
	if strings.TrimSpace(reason) != "" {
		note += ": " + strings.TrimSpace(reason)
	}
	if err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		if err := txRepo.Carrier.UpdateShipment(live.ID, map[string]any{
			"status": models.ShipmentCancelled, "cancelled_at": now, "error": note,
		}); err != nil {
			return err
		}
		if !wasPaid {
			return nil
		}
		// The parcel is not leaving: back to "Chờ gửi hàng". Only from
		// HANDED_OFF — once THE has scanned it, THE would have refused above.
		o, err := txRepo.Order.FindByID(orderID)
		if err != nil {
			return err
		}
		if o.SellerStatus == models.SellerStatusHandedOff {
			fields := map[string]interface{}{"seller_status": models.SellerStatusProduction, "handed_over_at": nil}
			// The carrier's number belongs to the cancelled parcel.
			if live.LastMileTracking != "" && strings.TrimSpace(o.TrackingNumber) == live.LastMileTracking {
				fields["tracking_number"] = ""
				fields["tracking_status"] = models.TrackingNone
				fields["tracking_detail"] = ""
				fields["tracking_raw_status"] = ""
				fields["tracking_synced_at"] = nil
			}
			if err := tx.Model(&models.Order{}).Where("id = ?", o.ID).Updates(fields).Error; err != nil {
				return err
			}
			_ = recordStatus(txRepo, models.EntityOrder, o.ID, string(models.SellerStatusHandedOff),
				string(models.SellerStatusProduction), actor, note)
		}
		return nil
	}); err != nil {
		return nil, apperr.Internal("THE đã huỷ nhưng FFM chưa ghi được — báo quản trị").Wrap(err)
	}
	s.audit.Log(actor, "THE_SHIPMENT_CANCEL", "order", &orderID, note,
		models.JSONMap{"shipment_id": live.ID, "the_id": live.ExternalID, "tracking": live.TrackingCode, "paid": wasPaid})
	live.Status, live.CancelledAt, live.Error = models.ShipmentCancelled, &now, note
	return &ShipmentView{CarrierShipment: *live}, nil
}

// ---------- preflight ----------

// PreflightOrder is one order's readiness before sending.
type PreflightOrder struct {
	OrderID      uint    `json:"order_id"`
	InternalCode string  `json:"internal_code"`
	Ready        bool    `json:"ready"`
	AlreadyPaid  bool    `json:"already_paid,omitempty"`
	Reason       string  `json:"reason,omitempty"`
	Parcel       *Parcel `json:"parcel,omitempty"`
}

// Preflight is what the confirm dialog shows before money moves.
type Preflight struct {
	Enabled bool             `json:"enabled"`
	Problem string           `json:"problem,omitempty"`
	Balance *float64         `json:"balance,omitempty"`
	Debt    *float64         `json:"debt,omitempty"`
	Ready   int              `json:"ready"`
	Blocked int              `json:"blocked"`
	Orders  []PreflightOrder `json:"orders"`
}

// PreflightTHE checks the selected orders against FFM's data (no address check,
// no create) and reads the wallet. Nothing is sent to THE but the balance read.
func (s *CarrierService) PreflightTHE(ctx context.Context, actor Actor, orderIDs []uint) (*Preflight, error) {
	if !actor.Can(models.Manage(models.FeatShipQueue)) {
		return nil, apperr.Forbidden("Bạn không có quyền gửi hàng cho THE")
	}
	out := &Preflight{Orders: []PreflightOrder{}}
	c, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	if !c.Enabled {
		return out, nil
	}
	out.Enabled = true
	v := s.view(c)
	if !v.Ready {
		out.Problem = v.Problem
		return out, nil
	}
	orderIDs = dedupeIDs(orderIDs)
	if len(orderIDs) > MaxShipBatch {
		return nil, apperr.BadRequest(fmt.Sprintf("Chọn tối đa %d đơn mỗi lần", MaxShipBatch))
	}
	for _, id := range orderIDs {
		o, err := s.repo.Order.FindByID(id)
		if err != nil {
			out.Orders = append(out.Orders, PreflightOrder{OrderID: id, Reason: "Không tìm thấy đơn"})
			out.Blocked++
			continue
		}
		po := PreflightOrder{OrderID: id, InternalCode: o.InternalCode}
		if reason := shipBlockReason(o); reason != "" {
			po.Reason = reason
		} else if live, _ := s.repo.Carrier.LiveShipment(id); live != nil && live.Status == models.ShipmentLabeled {
			po.Ready, po.AlreadyPaid = true, true
		} else if p, problems := s.BuildParcel(c, o); len(problems) > 0 {
			po.Reason = strings.Join(problems, " · ")
		} else {
			po.Ready, po.Parcel = true, p
		}
		if po.Ready {
			out.Ready++
		} else {
			out.Blocked++
		}
		out.Orders = append(out.Orders, po)
	}
	if cli, err := s.client(c); err == nil {
		cctx, cancel := context.WithTimeout(ctx, theReadTimeout)
		defer cancel()
		if bal, err := cli.GetBalance(cctx); err == nil {
			b, d := float64(bal.Balance), float64(bal.Debt)
			out.Balance, out.Debt = &b, &d
		} else if !errors.Is(err, context.Canceled) {
			out.Problem = "Không đọc được số dư ví THE: " + theErrorMessage(err)
		}
	}
	return out, nil
}
