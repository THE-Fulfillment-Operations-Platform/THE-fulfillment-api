package services

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newDownloadableDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{}, &models.AuditLog{},
		&models.Batch{}, &models.BatchItem{}, &models.BatchLink{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedDownloadable creates an approved (or pending) order with one item in a given
// design state and design URL, so the pick-list scoping can be exercised.
func seedDownloadable(t *testing.T, db *gorm.DB, code, sku string, status models.DesignStatus, designURL string, approved bool) *models.OrderItem {
	t.Helper()
	review := models.ReviewApproved
	if !approved {
		review = models.ReviewPending
	}
	order := &models.Order{
		InternalCode: "ORD-" + code,
		StoreOrderID: "SO-" + code,
		SellerID:     1,
		ReviewStatus: review,
		SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("seed order %s: %v", code, err)
	}
	item := &models.OrderItem{
		OrderID:      order.ID,
		InternalCode: code,
		SKUCode:      sku,
		Quantity:     1,
		DesignStatus: status,
		DesignURL:    designURL,
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("seed item %s: %v", code, err)
	}
	return item
}

func downloadableCodes(t *testing.T, svc *OrderService, f repositories.ItemFilter) map[string]bool {
	t.Helper()
	rows, err := svc.DesignDownloadableItems(f)
	if err != nil {
		t.Fatalf("DesignDownloadableItems: %v", err)
	}
	got := make(map[string]bool, len(rows))
	for _, it := range rows {
		got[it.InternalCode] = true
	}
	return got
}

// TestDesignDownloadableItems_Scoping locks the pick-list scope: only approved,
// still-needing-design items that already have a design file are downloadable.
func TestDesignDownloadableItems_Scoping(t *testing.T) {
	db := newDownloadableDB(t)
	svc := newOrderService(db)

	seedDownloadable(t, db, "100001_1/1", "SKU-A", models.DesignPending, "https://x/a.png", true)  // ✓
	seedDownloadable(t, db, "100002_1/1", "SKU-A", models.DesignPending, "", true)                 // ✗ no file
	seedDownloadable(t, db, "100003_1/1", "SKU-B", models.DesignReady, "https://x/c.png", true)    // ✗ not needing design
	seedDownloadable(t, db, "100004_1/1", "SKU-B", models.DesignPending, "https://x/d.png", false) // ✗ not approved

	got := downloadableCodes(t, svc, repositories.ItemFilter{})
	if !got["100001_1/1"] {
		t.Error("item with a design file in the queue must be downloadable")
	}
	for _, bad := range []string{"100002_1/1", "100003_1/1", "100004_1/1"} {
		if got[bad] {
			t.Errorf("%s must not be downloadable", bad)
		}
	}
}

// TestDesignDownloadableItems_Search covers the single search box matching EITHER
// internal_code OR sku_code (case-insensitive, partial).
func TestDesignDownloadableItems_Search(t *testing.T) {
	db := newDownloadableDB(t)
	svc := newOrderService(db)

	seedDownloadable(t, db, "100001_1/1", "BR-SH-2-KEP", models.DesignPending, "https://x/a.png", true)
	seedDownloadable(t, db, "100002_1/1", "MICA-HOLO", models.DesignPending, "https://x/b.png", true)

	// Match by SKU fragment.
	bySKU := downloadableCodes(t, svc, repositories.ItemFilter{Search: "mica"})
	if !bySKU["100002_1/1"] || bySKU["100001_1/1"] {
		t.Errorf("search 'mica' should match only the MICA sku item, got %v", bySKU)
	}

	// Match by internal-code fragment.
	byCode := downloadableCodes(t, svc, repositories.ItemFilter{Search: "100001"})
	if !byCode["100001_1/1"] || byCode["100002_1/1"] {
		t.Errorf("search '100001' should match only that internal code, got %v", byCode)
	}
}
