package services

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// newImportDB spins up an in-memory sqlite with every table the import flow
// touches, then seeds a seller and a material-mapped SKU so rows validate.
func newImportDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Store{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.ImportJob{}, &models.ImportError{},
		&models.Order{}, &models.OrderItem{}, &models.ItemAsset{}, &models.Note{},
		&models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&models.Seller{Code: "S1", Name: "Seller One"}).Error; err != nil {
		t.Fatalf("seed seller: %v", err)
	}
	mat := &models.Material{Code: "MICA", Name: "Mica trong 3 ly"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatalf("seed material: %v", err)
	}
	sku := &models.SKU{Code: "TESTSKU", Name: "Test SKU", ProductName: "Test SKU"}
	if err := db.Create(sku).Error; err != nil {
		t.Fatalf("seed sku: %v", err)
	}
	if err := db.Create(&models.SKUMaterial{SKUID: sku.ID, MaterialID: mat.ID, QuantityPerUnit: 1}).Error; err != nil {
		t.Fatalf("seed sku-material: %v", err)
	}
	return db
}

func importSvc(db *gorm.DB) *ImportService {
	repo := repositories.New(db)
	return &ImportService{repo: repo, audit: &AuditService{repo: repo}}
}

// row builds a minimal valid ImportRow for the given store order id + image code.
func row(storeOrderID, imageCode string) ImportRow {
	return ImportRow{
		StoreOrderID: storeOrderID, SKU: "TESTSKU", Quantity: 1, ImageCode: imageCode,
		Mockup:       "https://example.com/" + imageCode + ".png",
		ShippingName: "Jane Doe", ShippingAddress1: "1 Main St", ShippingCountry: "US",
	}
}

// TestImport_DuplicateStoreOrderID_StillGeneratesCodes is the core guarantee: a
// StoreOrderID that repeats never blocks the import; every row becomes its own
// item with a unique "100xxx_pos/total" internal code, and re-importing the same
// StoreOrderID makes a brand-new independent order (with a warning, not an error).
func TestImport_DuplicateStoreOrderID_StillGeneratesCodes(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}
	const sellerID = 1

	// One order with 5 items (same StoreOrderID) + one single-item order.
	rows := []ImportRow{
		row("US-DUP-1", "IMG-A"), row("US-DUP-1", "IMG-B"), row("US-DUP-1", "IMG-C"),
		row("US-DUP-1", "IMG-D"), row("US-DUP-1", "IMG-E"),
		row("US-SINGLE-2", "IMG-F"),
	}

	prev, err := svc.Preview(actor, sellerID, "XLSX", "seller.xlsx", rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.ErrorRows != 0 || len(prev.Errors) != 0 {
		t.Fatalf("expected no blocking errors on first import, got %d: %+v", prev.ErrorRows, prev.Errors)
	}
	if len(prev.Warnings) != 0 {
		t.Fatalf("expected no warnings on a first-ever import, got %+v", prev.Warnings)
	}
	if prev.ValidRows != 6 {
		t.Fatalf("expected 6 valid rows, got %d", prev.ValidRows)
	}

	if _, err := svc.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	orders := loadOrders(t, db, sellerID)
	if len(orders) != 2 {
		t.Fatalf("expected 2 orders, got %d", len(orders))
	}

	dup := orderByStoreID(t, orders, "US-DUP-1")
	if len(dup.Items) != 5 {
		t.Fatalf("expected 5 items on the duplicated order, got %d", len(dup.Items))
	}
	// Order base code is the workshop 6-digit code 100000+id.
	wantBase := strconv.Itoa(100000 + int(dup.ID))
	if dup.InternalCode != wantBase {
		t.Fatalf("order base code = %q, want %q", dup.InternalCode, wantBase)
	}
	// Each item is "<base>_<pos>/5" with distinct positions 1..5.
	re := regexp.MustCompile(`^` + wantBase + `_[1-5]/5$`)
	seen := map[string]bool{}
	for _, it := range dup.Items {
		if !re.MatchString(it.InternalCode) {
			t.Fatalf("item code %q does not match %s_<1-5>/5", it.InternalCode, wantBase)
		}
		if seen[it.InternalCode] {
			t.Fatalf("duplicate item internal code %q", it.InternalCode)
		}
		seen[it.InternalCode] = true
	}
	if len(seen) != 5 {
		t.Fatalf("expected 5 distinct item codes, got %d", len(seen))
	}

	single := orderByStoreID(t, orders, "US-SINGLE-2")
	singleBase := strconv.Itoa(100000 + int(single.ID))
	if single.Items[0].InternalCode != singleBase+"_1/1" {
		t.Fatalf("single item code = %q, want %s_1/1", single.Items[0].InternalCode, singleBase)
	}

	// --- Re-import the SAME StoreOrderID: must NOT block, warns, new order. ---
	prev2, err := svc.Preview(actor, sellerID, "XLSX", "seller2.xlsx", []ImportRow{row("US-DUP-1", "IMG-Z")})
	if err != nil {
		t.Fatalf("preview 2: %v", err)
	}
	if prev2.ErrorRows != 0 {
		t.Fatalf("re-import must not be blocked, got errors: %+v", prev2.Errors)
	}
	if len(prev2.Warnings) != 1 || prev2.Warnings[0].ErrorCode != "ORD_DUPLICATE" {
		t.Fatalf("expected 1 ORD_DUPLICATE warning, got %+v", prev2.Warnings)
	}
	if _, err := svc.Commit(actor, prev2.ImportJobID); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	orders = loadOrders(t, db, sellerID)
	if len(orders) != 3 {
		t.Fatalf("re-import should create a new independent order (want 3 total), got %d", len(orders))
	}
	// The new order shares the StoreOrderID but has its own distinct base code.
	var dupCount, newBaseSeen int
	for _, o := range orders {
		if o.StoreOrderID == "US-DUP-1" {
			dupCount++
			if o.InternalCode != wantBase {
				newBaseSeen++
			}
		}
	}
	if dupCount != 2 || newBaseSeen != 1 {
		t.Fatalf("expected 2 orders sharing US-DUP-1 with 1 distinct new base, got dup=%d newBase=%d", dupCount, newBaseSeen)
	}

	// Duplicate flag for the list screens: after the re-import US-DUP-1 spans 2
	// orders -> flagged; US-SINGLE-2 has a single order -> not flagged.
	repo := repositories.New(db)
	dupSet, err := repo.Order.DuplicateStoreOrderIDs([]string{"US-DUP-1", "US-SINGLE-2"})
	if err != nil {
		t.Fatalf("dup query: %v", err)
	}
	if !dupSet[repositories.StoreOrderDupKey(sellerID, "US-DUP-1")] {
		t.Fatalf("US-DUP-1 should be flagged as duplicate (spans 2 orders)")
	}
	if dupSet[repositories.StoreOrderDupKey(sellerID, "US-SINGLE-2")] {
		t.Fatalf("US-SINGLE-2 should NOT be flagged (single order)")
	}
}

func loadOrders(t *testing.T, db *gorm.DB, sellerID uint) []models.Order {
	t.Helper()
	var orders []models.Order
	if err := db.Preload("Items").Where("seller_id = ?", sellerID).Order("id asc").Find(&orders).Error; err != nil {
		t.Fatalf("load orders: %v", err)
	}
	return orders
}

func orderByStoreID(t *testing.T, orders []models.Order, storeID string) models.Order {
	t.Helper()
	for _, o := range orders {
		if strings.EqualFold(o.StoreOrderID, storeID) {
			return o
		}
	}
	t.Fatalf("no order with store order id %q", storeID)
	return models.Order{}
}
