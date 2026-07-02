package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

// TestDeriveItemStatusFromBatchItems verifies the item status is the
// least-advanced status across its production parts, and QC_PASSED only when all
// parts pass — the core rule behind combo (multi-material) items.
func TestDeriveItemStatusFromBatchItems(t *testing.T) {
	cases := []struct {
		name  string
		parts []models.InternalStatus
		want  models.InternalStatus
	}{
		{"no parts -> pending", nil, models.StatusPending},
		{"single printed", []models.InternalStatus{models.StatusPrinted}, models.StatusPrinted},
		{"combo: wood QC'd, mica printed -> printed", []models.InternalStatus{models.StatusQCPassed, models.StatusPrinted}, models.StatusPrinted},
		{"combo: both cut -> cut", []models.InternalStatus{models.StatusCut, models.StatusCut}, models.StatusCut},
		{"combo: all qc passed -> qc passed", []models.InternalStatus{models.StatusQCPassed, models.StatusQCPassed}, models.StatusQCPassed},
		{"mixed pending dominates", []models.InternalStatus{models.StatusQCPassed, models.StatusPending}, models.StatusPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var parts []models.BatchItem
			for _, s := range tc.parts {
				parts = append(parts, models.BatchItem{Status: s})
			}
			if got := deriveItemStatusFromBatchItems(parts); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
