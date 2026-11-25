package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

// Đổi sku_code của dòng đơn ở màn Sửa đơn phải kéo theo sku_id. Bản cũ: đơn nạp
// kèm Preload("Items.SKU") rồi Save(item) ghi đè sku_id mới bằng id của SKU cũ
// đang nằm trong item.SKU → sku_code mới, sku_id cũ (47 dòng lệch trên prod).
func TestEditOrder_ChangingSKUCodeMovesSKUID(t *testing.T) {
	db := newImportDB(t)
	imp := importSvc(db)
	svc := directOrderSvc(db)
	actor := Actor{ID: 1, Role: models.RoleOwner}

	var oldSKU models.SKU
	db.Where("code = ?", "TESTSKU").First(&oldSKU) // seeded by newImportDB; row() imports with it
	newSKU := &models.SKU{Code: "SKU-NEW", Name: "Mới"}
	db.Create(newSKU)

	prev, err := imp.Preview(actor, 1, "XLSX", "f.xlsx", []ImportRow{row("EDIT-SKU-1", "A")}, HeaderReport{})
	if err != nil || prev.ValidRows != 1 {
		t.Fatalf("preview: %v (%+v)", err, prev)
	}
	if _, err := imp.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	order := loadOrders(t, db, 1)[0]
	itemID := order.Items[0].ID
	if order.Items[0].SKUID == nil || *order.Items[0].SKUID != oldSKU.ID {
		t.Fatalf("setup: sku_id should start at the old SKU, got %v (old=%d)", *order.Items[0].SKUID, oldSKU.ID)
	}

	code := "sku-new"
	if _, err := svc.UpdateOrder(actor, order.ID, UpdateOrderInput{Items: []EditOrderItemInput{{ID: itemID, SKUCode: &code}}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	var got models.OrderItem
	db.First(&got, itemID)
	if got.SKUCode != "SKU-NEW" {
		t.Fatalf("sku_code = %q", got.SKUCode)
	}
	if got.SKUID == nil || *got.SKUID != newSKU.ID {
		t.Fatalf("sku_id must follow the new code (want %d), got %v", newSKU.ID, got.SKUID)
	}
}
