package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

func TestLabelScan_ReadOnlyAndReportsLabelState(t *testing.T) {
	db := newHandoffDB(t)
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))

	withLabel := seedOrderForShipping(t, db, "100001", models.StatusQCPassed)
	db.Model(withLabel).Update("external_label_url", "https://pdfobject.com/pdf/sample.pdf")
	noLabel := seedOrderForShipping(t, db, "100002", models.StatusQCPassed, models.StatusCut)

	// Order QR, padded the way scanners pad it.
	res, err := svc.LabelScan(opsActor(), " 100001 ")
	if err != nil {
		t.Fatalf("LabelScan(order code): %v", err)
	}
	if res.OrderID != withLabel.ID || !res.HasExternalLabel || !res.QCDone {
		t.Fatalf("res = %+v, want order %d with external label and QC done", res, withLabel.ID)
	}

	// Product tem of an order that is not QC-finished and has no label: still resolves.
	res, err = svc.LabelScan(opsActor(), "100002_2")
	if err != nil {
		t.Fatalf("LabelScan(item code): %v", err)
	}
	if res.OrderID != noLabel.ID || res.HasExternalLabel || res.QCDone || res.UnfinishedItems != 1 || res.TotalItems != 2 {
		t.Fatalf("res = %+v, want order %d without label, 1/2 unfinished", res, noLabel.ID)
	}

	// Read-only: nothing moved, no handoff made.
	for _, o := range []*models.Order{withLabel, noLabel} {
		var got models.Order
		db.First(&got, o.ID)
		if got.SellerStatus != models.SellerStatusProduction {
			t.Errorf("order %d status = %s, scan must not change it", o.ID, got.SellerStatus)
		}
	}
	var handoffs int64
	db.Model(&models.Handoff{}).Count(&handoffs)
	if handoffs != 0 {
		t.Errorf("%d handoffs after label scans, want 0", handoffs)
	}
}

func TestLabelScan_RefusesUnknownBlankAndWrongRole(t *testing.T) {
	db := newHandoffDB(t)
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))
	seedOrderForShipping(t, db, "100001", models.StatusQCPassed)

	if _, err := svc.LabelScan(opsActor(), "999999"); err == nil {
		t.Error("unknown code accepted")
	}
	if _, err := svc.LabelScan(opsActor(), "  "); err == nil {
		t.Error("blank code accepted")
	}
	if _, err := svc.LabelScan(Actor{ID: 2, Role: models.RoleSeller}, "100001"); err == nil {
		t.Error("seller allowed to scan for labels")
	}
	// Packing/Shipping staff hold no QC tick but must be able to use the box.
	if _, err := svc.LabelScan(Actor{ID: 3, Role: models.RolePacking}, "100001"); err != nil {
		t.Errorf("packing role refused: %v", err)
	}
}

func TestLabelScan_CancelledOrderRefused(t *testing.T) {
	db := newHandoffDB(t)
	svc := newPackingService(db, newSync(t, db, newFakeProvider()))
	o := seedOrderForShipping(t, db, "100003", models.StatusQCPassed)
	db.Model(o).Update("cancellation_status", models.CancellationApproved)
	if _, err := svc.LabelScan(opsActor(), "100003"); err == nil {
		t.Error("cancelled order accepted")
	}
}
