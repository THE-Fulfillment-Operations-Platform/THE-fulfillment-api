package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

// TestSellerCancelAction verifies the cancellation rule engine: nothing produced
// yet → the seller cancels outright and free; anything already produced → they
// may only request it, ops/admin decide, and the order stays billable.
func TestSellerCancelAction(t *testing.T) {
	cases := []struct {
		name         string
		review       models.ReviewStatus
		cancellation models.CancellationStatus
		stage        models.CancelStage
		want         SellerCancelAction
	}{
		{"pending review -> direct cancel", models.ReviewPending, models.CancellationNone, models.CancelStagePreProduction, SellerActionCancel},
		{"needs correction -> direct cancel", models.ReviewNeedsFix, models.CancellationNone, models.CancelStagePreProduction, SellerActionCancel},
		{"approved waiting for design -> direct cancel", models.ReviewApproved, models.CancellationNone, models.CancelStagePreProduction, SellerActionCancel},
		{"approved in production -> request", models.ReviewApproved, models.CancellationNone, models.CancelStageInProduction, SellerActionRequest},
		{"approved packed -> request", models.ReviewApproved, models.CancellationNone, models.CancelStagePacked, SellerActionRequest},
		{"approved shipped -> request", models.ReviewApproved, models.CancellationNone, models.CancelStageShipped, SellerActionRequest},
		{"request pending -> none", models.ReviewApproved, models.CancellationRequested, models.CancelStageInProduction, SellerActionNone},
		{"already cancelled -> none", models.ReviewCancelled, models.CancellationSeller, models.CancelStagePreProduction, SellerActionNone},
		{"rejected -> none", models.ReviewRejected, models.CancellationNone, models.CancelStagePreProduction, SellerActionNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sellerCancelAction(tc.review, tc.cancellation, tc.stage)
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestOrderCancelStage verifies the stage — and therefore the billing decision —
// derived from how far an order has got.
func TestOrderCancelStage(t *testing.T) {
	cases := []struct {
		name         string
		seller       models.SellerStatus
		inProduction bool
		want         models.CancelStage
		wantBillable bool
	}{
		{"nothing started", models.SellerStatusProduction, false, models.CancelStagePreProduction, false},
		{"work in flight", models.SellerStatusProduction, true, models.CancelStageInProduction, true},
		{"packed", models.SellerStatusPacked, true, models.CancelStagePacked, true},
		{"handed off", models.SellerStatusHandedOff, true, models.CancelStagePacked, true},
		{"shipped", models.SellerStatusShipped, true, models.CancelStageShipped, true},
		// Packed without any per-item production trace still counts as packed: the
		// goods physically exist whatever the item rows say.
		{"packed, no item trace", models.SellerStatusPacked, false, models.CancelStagePacked, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orderCancelStage(tc.seller, tc.inProduction)
			if got != tc.want {
				t.Fatalf("stage: got %s, want %s", got, tc.want)
			}
			if got.Billable() != tc.wantBillable {
				t.Fatalf("billable: got %v, want %v", got.Billable(), tc.wantBillable)
			}
		})
	}
}

// TestOrderInProduction verifies production is detected from either an advanced
// internal status or the presence of batch parts.
func TestOrderInProduction(t *testing.T) {
	cases := []struct {
		name  string
		items []models.OrderItem
		want  bool
	}{
		{"no items", nil, false},
		{"all pending, no batch", []models.OrderItem{{InternalStatus: models.StatusPending}}, false},
		{"printed item", []models.OrderItem{{InternalStatus: models.StatusPrinted}}, true},
		{"pending but batched", []models.OrderItem{{InternalStatus: models.StatusPending, BatchItems: []models.BatchItem{{}}}}, true},
		{"mixed", []models.OrderItem{{InternalStatus: models.StatusPending}, {InternalStatus: models.StatusQCPassed}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &models.Order{Items: tc.items}
			if got := orderInProduction(o); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsReviewable verifies only pending/needs-correction orders are reviewable.
func TestIsReviewable(t *testing.T) {
	yes := []models.ReviewStatus{models.ReviewPending, models.ReviewNeedsFix}
	no := []models.ReviewStatus{models.ReviewApproved, models.ReviewRejected, models.ReviewCancelled}
	for _, s := range yes {
		if !isReviewable(s) {
			t.Fatalf("%s should be reviewable", s)
		}
	}
	for _, s := range no {
		if isReviewable(s) {
			t.Fatalf("%s should not be reviewable", s)
		}
	}
}
