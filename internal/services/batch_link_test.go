package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

func TestSetBatchLink_AddAndReplace(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	material := &models.Material{Code: "WOOD-LINK", Name: "Wood"}
	if err := db.Create(material).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	batch := &models.Batch{Code: "#LINK-1", MaterialID: material.ID, Status: models.StatusPending}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch: %v", err)
	}

	first, err := svc.SetBatchLink(Actor{ID: 7}, batch.ID, SetBatchLinkInput{Kind: "print", URL: " https://files/print-v1 "})
	if err != nil {
		t.Fatalf("add link: %v", err)
	}
	if first.URL != "https://files/print-v1" {
		t.Fatalf("trimmed URL = %q", first.URL)
	}

	// Replacing with a DIFFERENT URL is a deliberate act and now carries a reason.
	replaced, err := svc.SetBatchLink(Actor{ID: 8}, batch.ID, SetBatchLinkInput{Kind: "PRINT", URL: "https://files/print-v2", Reason: "đổi file in"})
	if err != nil {
		t.Fatalf("replace link: %v", err)
	}
	if replaced.ID != first.ID {
		t.Fatalf("replace created a duplicate: first=%d replaced=%d", first.ID, replaced.ID)
	}
	if replaced.URL != "https://files/print-v2" {
		t.Fatalf("replaced URL = %q", replaced.URL)
	}

	var count int64
	db.Model(&models.BatchLink{}).Where("batch_id = ? AND kind = ?", batch.ID, models.BatchLinkPrint).Count(&count)
	if count != 1 {
		t.Fatalf("want one PRINT link, got %d", count)
	}
}

func TestSetBatchLink_RejectsInvalidURL(t *testing.T) {
	db := newSplitDB(t)
	svc := newBatchService(db)
	material := &models.Material{Code: "WOOD-URL", Name: "Wood"}
	if err := db.Create(material).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	batch := &models.Batch{Code: "#LINK-U", MaterialID: material.ID, Status: models.StatusPending}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	if _, err := svc.SetBatchLink(Actor{ID: 1}, batch.ID, SetBatchLinkInput{Kind: "CUT", URL: "javascript:alert(1)"}); err == nil {
		t.Fatal("non-http URL should be rejected")
	}
}
