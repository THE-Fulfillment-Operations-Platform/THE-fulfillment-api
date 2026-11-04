package services

import (
	"fmt"
	"sort"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Bộ lọc "Bảo hành" của màn Orders/Items (#3194). Điều kiện "có thể bảo hành"
// phải CÙNG MỘT luật với WarrantyPolicy.Eligible: test này dựng đủ tổ hợp trạng
// thái, hỏi Eligible (Go) cho từng đơn rồi so với danh sách SQL trả về. Lệch một
// đơn là fail. Chạy trên sqlite ở đây, và trên Postgres thật ở TestWarrantyFilterPostgres.

func itemOrderIDs(t *testing.T, svc *OrderService, warranty string) []uint {
	return itemOrderIDsFor(t, svc, repositories.ItemFilter{Warranty: warranty})
}

func itemOrderIDsFor(t *testing.T, svc *OrderService, f repositories.ItemFilter) []uint {
	t.Helper()
	f.IncludeCancelled = true
	f.WarrantyEligible = WarrantyPolicy.EligibleScope // itemFilterFrom (handler) sets this in production
	ids := []uint{}
	for page := 1; ; page++ {
		f.Page = repositories.Page{Page: page, PageSize: 100}
		rows, total, err := svc.ListItems(f)
		if err != nil {
			t.Fatalf("ListItems(%+v): %v", f.Warranty, err)
		}
		for _, r := range rows {
			if r.Order != nil {
				if ok, _ := WarrantyPolicy.Eligible(r.Order); r.Order.WarrantyEligible != ok {
					t.Fatalf("order %d: warranty_eligible flag disagrees with Eligible", r.Order.ID)
				}
			}
			ids = append(ids, r.OrderID)
		}
		if len(rows) == 0 || len(ids) >= int(total) {
			break
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func sameIDs(t *testing.T, what string, got, want []uint) {
	t.Helper()
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: got %v want %v", what, got, want)
	}
}

func checkWarrantyFilter(t *testing.T, db *gorm.DB) {
	t.Helper()
	svc := newOrderService(db)
	seller := &models.Seller{Code: "SF", Name: "SF"}
	if err := db.Create(seller).Error; err != nil {
		t.Fatalf("seller: %v", err)
	}

	// 1. Mọi tổ hợp trạng thái: SQL phải trùng Eligible.
	reviews := []models.ReviewStatus{models.ReviewPending, models.ReviewNeedsFix, models.ReviewApproved, models.ReviewRejected, models.ReviewCancelled}
	cancels := []models.CancellationStatus{models.CancellationNone, models.CancellationSeller, models.CancellationRequested, models.CancellationApproved, models.CancellationRejected}
	sellers := []models.SellerStatus{models.SellerStatusProduction, models.SellerStatusPacked, models.SellerStatusHandedOff, models.SellerStatusShipped, models.SellerStatusDelivered}
	trackings := []models.TrackingStatus{models.TrackingNone, models.TrackingPending, models.TrackingPreTransit, models.TrackingInTransit,
		models.TrackingOutForDelivery, models.TrackingPickUp, models.TrackingDelivered, models.TrackingUndelivered,
		models.TrackingException, models.TrackingExpired, models.TrackingCancelled}
	var want []uint
	n := 0
	for _, rv := range reviews {
		for _, c := range cancels {
			for _, s := range sellers {
				for _, tr := range trackings {
					n++
					code := fmt.Sprintf("M%04d", n)
					o := &models.Order{InternalCode: "I-" + code, StoreOrderID: code, StoreOrderRef: code, SellerID: seller.ID,
						ReviewStatus: rv, CancellationStatus: c, SellerStatus: s, TrackingStatus: tr}
					if err := db.Create(o).Error; err != nil {
						t.Fatalf("seed %s: %v", code, err)
					}
					if err := db.Create(&models.OrderItem{OrderID: o.ID, InternalCode: "IT-" + code, LineNo: 1, SKUCode: "SKU", Quantity: 1,
						CancellationStatus: models.CancellationNone}).Error; err != nil {
						t.Fatalf("seed item %s: %v", code, err)
					}
					if ok, _ := WarrantyPolicy.Eligible(o); ok {
						want = append(want, o.ID)
					}
				}
			}
		}
	}
	if len(want) == 0 || len(want) == n {
		t.Fatalf("matrix is degenerate: %d of %d eligible", len(want), n)
	}
	sameIDs(t, "eligible vs Eligible()", itemOrderIDs(t, svc, "eligible"), want)

	// 2. Chuỗi bảo hành: gốc -> BH1 -> (BH1 giao xong) -> BH2 (cha = BH1, gốc = gốc).
	root := seedDelivered(t, db, "CHAIN1", 1)
	bh1, err := svc.CreateWarranty(admin, root.ID, okInput())
	if err != nil {
		t.Fatalf("BH1: %v", err)
	}
	sameIDs(t, "child after BH1", itemOrderIDs(t, svc, "child"), []uint{bh1.ID})
	sameIDs(t, "root after BH1", itemOrderIDs(t, svc, "root"), []uint{root.ID})
	if err := db.Model(&models.Order{}).Where("id = ?", bh1.ID).Updates(map[string]any{
		"review_status": models.ReviewApproved, "seller_status": models.SellerStatusDelivered, "tracking_status": models.TrackingDelivered}).Error; err != nil {
		t.Fatalf("deliver BH1: %v", err)
	}
	bh2, err := svc.CreateWarranty(admin, bh1.ID, okInput())
	if err != nil {
		t.Fatalf("BH2: %v", err)
	}
	sameIDs(t, "child after BH2", itemOrderIDs(t, svc, "child"), []uint{bh1.ID, bh2.ID})
	// "Đã từng bảo hành" = đúng những đơn mà huy hiệu "Đã BH ×N" hiện (Counts > 0).
	var all []uint
	db.Model(&models.Order{}).Pluck("id", &all)
	counts, err := repositories.New(db).Warranty.Counts(all)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	var wantRoot []uint
	for _, id := range all {
		if counts[id] > 0 {
			wantRoot = append(wantRoot, id)
		}
	}
	sameIDs(t, "root vs Counts>0", itemOrderIDs(t, svc, "root"), wantRoot)
	if len(wantRoot) != 2 { // gốc và BH1
		t.Fatalf("expected root+BH1 to have warranties, got %v", wantRoot)
	}
	// Bộ lọc đứng cùng bộ lọc khác: thêm seller không phá ngoặc OR của "root".
	sid := seller.ID
	sameIDs(t, "root + seller", itemOrderIDsFor(t, svc, repositories.ItemFilter{Warranty: "root", SellerID: &sid}), []uint{root.ID, bh1.ID})
	// Không lọc: thấy tất cả.
	if got := itemOrderIDs(t, svc, ""); len(got) != n+3 {
		t.Fatalf("no filter: got %d orders want %d", len(got), n+3)
	}
}

func TestWarrantyFilter(t *testing.T) {
	checkWarrantyFilter(t, newWarrantyDB(t))
}
