package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

// TestSellerCancelAction verifies the cancellation rule engine: what a seller may
// do with an order depends on its review status, cancellation status and how far
// it has progressed (approved-but-waiting vs in-production vs packed+).
func TestSellerCancelAction(t *testing.T) {
	cases := []struct {
		name         string
		review       models.ReviewStatus
		cancellation models.CancellationStatus
		packed       bool
		inProduction bool
		want         SellerCancelAction
	}{
		{"pending review -> direct cancel", models.ReviewPending, models.CancellationNone, false, false, SellerActionCancel},
		{"needs correction -> direct cancel", models.ReviewNeedsFix, models.CancellationNone, false, false, SellerActionCancel},
		{"approved waiting for design -> request", models.ReviewApproved, models.CancellationNone, false, false, SellerActionRequest},
		{"approved in production -> ops only", models.ReviewApproved, models.CancellationNone, false, true, SellerActionOpsOnly},
		{"approved packed -> refund/claim", models.ReviewApproved, models.CancellationNone, true, true, SellerActionRefundClaim},
		{"approved packed (no prod flag) -> refund/claim", models.ReviewApproved, models.CancellationNone, true, false, SellerActionRefundClaim},
		{"request pending -> none", models.ReviewApproved, models.CancellationRequested, false, false, SellerActionNone},
		{"already cancelled -> none", models.ReviewCancelled, models.CancellationSeller, false, false, SellerActionNone},
		{"rejected -> none", models.ReviewRejected, models.CancellationNone, false, false, SellerActionNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sellerCancelAction(tc.review, tc.cancellation, tc.packed, tc.inProduction)
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
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
