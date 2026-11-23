package services

import (
	"strings"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// LabelScanResult is what the "Quét in label" box needs to know about the order a
// scanner just read: which order it is, whether it has a label to print and
// whether QC is finished. It is READ-ONLY — scanning to print a label never moves
// the order, never hands it off and never calls THE.
type LabelScanResult struct {
	OrderID      uint   `json:"order_id"`
	InternalCode string `json:"internal_code"`
	StoreOrderID string `json:"store_order_id"`
	// HasExternalLabel: orders.external_label_url is set (the link itself is not
	// exposed here; the print goes through GET /orders/:id/label/external).
	HasExternalLabel bool `json:"has_external_label"`
	// THETracking is the label already made on THE for this order (empty = none).
	THETracking string `json:"the_tracking,omitempty"`
	// QCDone is false while any live product has not passed QC; the box still
	// prints but tells the operator the order is not finished.
	QCDone          bool `json:"qc_done"`
	UnfinishedItems int  `json:"unfinished_items,omitempty"`
	TotalItems      int  `json:"total_items,omitempty"`
}

// LabelScan resolves whatever the scanner read (order QR "100048" or product tem
// "100048_1/3" — the same resolver as ship-scan) into the order and its label state.
func (s *PackingService) LabelScan(actor Actor, code string) (*LabelScanResult, error) {
	if !canSeeShipments(actor) {
		return nil, apperr.Forbidden("Bạn không có quyền in label")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, apperr.BadRequest("Hãy quét hoặc nhập mã nội bộ của đơn")
	}
	order, err := s.orderByScanCode(code)
	if err != nil {
		return nil, err
	}
	if order.CancellationStatus == models.CancellationApproved || order.CancellationStatus == models.CancellationSeller {
		return nil, apperr.Conflict("Đơn đã huỷ")
	}
	live := activeOrderItems(order.Items)
	waiting := 0
	for _, it := range live {
		if it.InternalStatus != models.StatusQCPassed {
			waiting++
		}
	}
	res := &LabelScanResult{
		OrderID: order.ID, InternalCode: order.InternalCode, StoreOrderID: order.StoreOrderID,
		HasExternalLabel: strings.TrimSpace(order.ExternalLabelURL) != "",
		QCDone:           len(live) > 0 && waiting == 0,
		UnfinishedItems:  waiting, TotalItems: len(live),
	}
	// Display-only: a failure to read shipments must not block printing.
	if shipments, _ := s.repo.Carrier.LatestShipmentsByOrder([]uint{order.ID}); shipments != nil {
		if sh, ok := shipments[order.ID]; ok && sh.Status == models.ShipmentLabeled {
			res.THETracking = sh.TrackingCode
		}
	}
	return res, nil
}
