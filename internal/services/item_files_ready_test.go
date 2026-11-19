package services

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Màn Đơn hàng/Sản phẩm xin ?with_files=1: mỗi dòng mang cờ production_files_ready
// ("Đã có file SX") tính bằng CÙNG điều kiện với bộ lọc ?files=ready (Nghĩa #6360).
// Không xin thì không tốn câu lệnh và cờ không có trong JSON.
func TestListItems_WithFilesReadyFlag(t *testing.T) {
	db := newWarrantyDB(t)
	if err := db.AutoMigrate(&models.BatchLink{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&models.Seller{Code: "S1", Name: "S1"}).Error; err != nil {
		t.Fatal(err)
	}
	mat := &models.Material{Code: "M1", Name: "M1"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatal(err)
	}
	mkItem := func(code string) *models.OrderItem {
		o := &models.Order{InternalCode: "O" + code, StoreOrderID: "S" + code, SellerID: 1,
			ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction}
		if err := db.Create(o).Error; err != nil {
			t.Fatal(err)
		}
		it := &models.OrderItem{OrderID: o.ID, InternalCode: code, SKUCode: "SKU-1", Quantity: 1}
		if err := db.Create(it).Error; err != nil {
			t.Fatal(err)
		}
		return it
	}
	mkBatch := func(code string, it *models.OrderItem, links ...models.BatchLinkKind) {
		b := &models.Batch{Code: code, MaterialID: mat.ID, Status: models.StatusPending}
		if err := db.Create(b).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&models.BatchItem{BatchID: b.ID, OrderItemID: it.ID, MaterialID: mat.ID, Status: models.StatusPending, Attempt: 1}).Error; err != nil {
			t.Fatal(err)
		}
		for _, k := range links {
			if err := db.Create(&models.BatchLink{BatchID: b.ID, Kind: k, URL: "https://f/" + code + string(k), LinkUpdatedAt: time.Now()}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	ready := mkItem("A_1/1")
	mkBatch("#A", ready, models.BatchLinkPrint, models.BatchLinkCut)
	cutMissing := mkItem("B_1/1")
	mkBatch("#B", cutMissing, models.BatchLinkPrint)
	mkItem("C_1/1") // chưa vào batch

	svc := newOrderService(db)
	page := repositories.Page{Page: 1, PageSize: 20}

	rows, _, err := svc.ListItems(repositories.ItemFilter{Page: page, WithFilesReady: true})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	for _, r := range rows {
		if want := r.ID == ready.ID; r.ProductionFilesReady != want {
			t.Fatalf("%s: production_files_ready = %v, want %v", r.InternalCode, r.ProductionFilesReady, want)
		}
	}
	raw, _ := json.Marshal(rows)
	if strings.Count(string(raw), `"production_files_ready":true`) != 1 {
		t.Fatalf("JSON phải có đúng 1 dòng production_files_ready:true: %s", raw)
	}

	// Không xin: không điền, JSON không có trường này.
	rows, _, err = svc.ListItems(repositories.ItemFilter{Page: page})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, r := range rows {
		if r.ProductionFilesReady {
			t.Fatalf("%s: cờ file SX bật khi không xin with_files", r.InternalCode)
		}
	}
	raw, _ = json.Marshal(rows)
	if strings.Contains(string(raw), "production_files_ready") {
		t.Fatalf("không xin with_files mà JSON vẫn có production_files_ready")
	}

	// Bộ lọc và cờ là một: lọc "ready" ra đúng dòng có cờ.
	rows, total, err := svc.ListItems(repositories.ItemFilter{Page: page, Files: "ready", WithFilesReady: true})
	if err != nil {
		t.Fatalf("ListItems ready: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].ID != ready.ID || !rows[0].ProductionFilesReady {
		t.Fatalf("ready: total=%d rows=%v", total, rows)
	}
}
