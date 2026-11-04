package services

import (
	"fmt"
	"sort"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Bộ lọc "Bảo hành" + nút Bảo hành ở danh sách Orders/Items (#3194). Hai điều phải
// đúng: (1) SQL của bộ lọc "Có thể bảo hành" trùng WarrantyPolicy.CanPress, (2) cờ
// warranty_eligible trên từng dòng trùng CanPress — tức bấm nút không ra toast lỗi.
// Chạy trên sqlite ở TestWarrantyFilter và Postgres thật ở TestWarrantyFilterPostgres.

var (
	viewerTech  = models.RoleTechnician
	viewerOwner = models.RoleOwner
)

// listOrderIDs returns the order ids of the item list under f, every page, and
// (when role != "") checks each row's warranty_eligible flag equals CanPress.
func listOrderIDs(t *testing.T, svc *OrderService, role models.Role, f repositories.ItemFilter) []uint {
	t.Helper()
	f.WarrantyEligible = WarrantyPolicy.PressableScope(role) // itemFilterFrom (handler) does this in production
	ids := []uint{}
	for page := 1; ; page++ {
		f.Page = repositories.Page{Page: page, PageSize: 100}
		rows, total, err := svc.ListItemsAs(role, f)
		if err != nil {
			t.Fatalf("ListItemsAs(%+v): %v", f.Warranty, err)
		}
		for _, r := range rows {
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
	if got == nil {
		got = []uint{}
	}
	if want == nil {
		want = []uint{}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: got %v want %v", what, got, want)
	}
}

// pressable computes, order by order and in Go only, who CanPress says yes to.
func pressable(t *testing.T, db *gorm.DB, role models.Role) []uint {
	t.Helper()
	repo := repositories.New(db)
	var orders []models.Order
	if err := db.Find(&orders).Error; err != nil {
		t.Fatalf("load orders: %v", err)
	}
	var out []uint
	for i := range orders {
		o := &orders[i]
		open, err := repo.Warranty.OpenInChain(warrantyRoot(o), WarrantyPolicy.ReturnedTrackingStatuses)
		if err != nil {
			t.Fatalf("OpenInChain: %v", err)
		}
		if ok, _ := WarrantyPolicy.CanPress(role, o, open); ok {
			out = append(out, o.ID)
		}
	}
	return out
}

// flagged = rows whose warranty_eligible is true in the plain (unfiltered) list.
func flagged(t *testing.T, svc *OrderService, role models.Role) []uint {
	t.Helper()
	var out []uint
	for page := 1; ; page++ {
		rows, total, err := svc.ListItemsAs(role, repositories.ItemFilter{Page: repositories.Page{Page: page, PageSize: 100}, IncludeCancelled: true})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, r := range rows {
			if r.Order != nil && r.Order.WarrantyEligible {
				out = append(out, r.OrderID)
			}
		}
		if len(rows) == 0 || page*100 >= int(total) {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func checkWarrantyFilter(t *testing.T, db *gorm.DB) {
	t.Helper()
	svc := newOrderService(db)
	seller := &models.Seller{Code: "SF", Name: "SF"}
	if err := db.Create(seller).Error; err != nil {
		t.Fatalf("seller: %v", err)
	}

	// 1. Mọi tổ hợp trạng thái (seller status lấy từ models.AllSellerStatuses).
	reviews := []models.ReviewStatus{models.ReviewPending, models.ReviewNeedsFix, models.ReviewApproved, models.ReviewRejected, models.ReviewCancelled}
	cancels := []models.CancellationStatus{models.CancellationNone, models.CancellationSeller, models.CancellationRequested, models.CancellationApproved, models.CancellationRejected}
	trackings := []models.TrackingStatus{models.TrackingNone, models.TrackingPending, models.TrackingPreTransit, models.TrackingInTransit,
		models.TrackingOutForDelivery, models.TrackingPickUp, models.TrackingDelivered, models.TrackingUndelivered,
		models.TrackingException, models.TrackingExpired, models.TrackingCancelled}
	var wantPolicy []uint
	n := 0
	var cancelledItemOrder uint
	for _, rv := range reviews {
		for _, c := range cancels {
			for _, s := range models.AllSellerStatuses {
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
						wantPolicy = append(wantPolicy, o.ID)
						if cancelledItemOrder == 0 {
							cancelledItemOrder = o.ID
						}
					}
				}
			}
		}
	}
	if len(wantPolicy) == 0 || len(wantPolicy) == n {
		t.Fatalf("matrix is degenerate: %d of %d eligible", len(wantPolicy), n)
	}
	// Không có chuỗi nào: mọi vai trò được bấm thấy đúng tập Eligible.
	sameIDs(t, "eligible (tech) vs Eligible()", listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "eligible"}), append([]uint{}, wantPolicy...))
	sameIDs(t, "flag (tech) vs Eligible()", flagged(t, svc, viewerTech), append([]uint{}, wantPolicy...))
	// Vai trò không được bấm (CS) không thấy gì, không có nút.
	sameIDs(t, "eligible (CS)", listOrderIDs(t, svc, models.RoleCS, repositories.ItemFilter{Warranty: "eligible"}), nil)
	sameIDs(t, "flag (CS)", flagged(t, svc, models.RoleCS), nil)

	// Đường mặc định (IncludeCancelled=false): dòng item đã huỷ bị ẩn, còn lại thì như nhau.
	if err := db.Model(&models.OrderItem{}).Where("order_id = ?", cancelledItemOrder).
		Update("cancellation_status", models.CancellationApproved).Error; err != nil {
		t.Fatalf("cancel item: %v", err)
	}
	var hidden []uint
	for _, id := range wantPolicy {
		if id != cancelledItemOrder {
			hidden = append(hidden, id)
		}
	}
	sameIDs(t, "default path hides cancelled line", listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "eligible"}), hidden)
	sameIDs(t, "IncludeCancelled shows it", listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "eligible", IncludeCancelled: true}), append([]uint{}, wantPolicy...))
	db.Model(&models.OrderItem{}).Where("order_id = ?", cancelledItemOrder).Update("cancellation_status", models.CancellationNone)

	// 2. Chuỗi: gốc đã giao -> BH1 (đang chờ duyệt = chưa giao xong).
	root := seedDelivered(t, db, "CHAIN1", 1)
	bh1, err := svc.CreateWarranty(admin, root.ID, okInput())
	if err != nil {
		t.Fatalf("BH1: %v", err)
	}
	sameIDs(t, "child after BH1", listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "child"}), []uint{bh1.ID})
	sameIDs(t, "root after BH1", listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "root"}), []uint{root.ID})

	// Ca chuỗi đang có BH chưa giao: KTV không thấy nút / không nằm trong lọc, OWNER thì có.
	has := func(ids []uint, id uint) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}
	techFlag := flagged(t, svc, viewerTech)
	ownerFlag := flagged(t, svc, viewerOwner)
	techFilter := listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "eligible"})
	ownerFilter := listOrderIDs(t, svc, viewerOwner, repositories.ItemFilter{Warranty: "eligible"})
	if has(techFlag, root.ID) || has(techFilter, root.ID) {
		t.Fatalf("KTV must NOT see the button / filter hit for a chain with an unfinished BH")
	}
	if !has(ownerFlag, root.ID) || !has(ownerFilter, root.ID) {
		t.Fatalf("OWNER may override the guard: must see the button and the filter hit")
	}
	// ...và đúng điều WarrantyInfo nói: KTV không bấm được, OWNER bấm được.
	if info, _ := svc.WarrantyInfo(Actor{ID: 3, Role: viewerTech}, root.ID); info == nil || info.CanWarranty {
		t.Fatalf("WarrantyInfo(KTV).CanWarranty must be false: %+v", info)
	}
	if info, _ := svc.WarrantyInfo(Actor{ID: 1, Role: viewerOwner}, root.ID); info == nil || !info.CanWarranty {
		t.Fatalf("WarrantyInfo(OWNER).CanWarranty must be true: %+v", info)
	}

	// BH1 giao xong -> chặn hết, KTV bấm được lại; rồi BH2 từ BH1.
	if err := db.Model(&models.Order{}).Where("id = ?", bh1.ID).Updates(map[string]any{
		"review_status": models.ReviewApproved, "seller_status": models.SellerStatusDelivered, "tracking_status": models.TrackingDelivered}).Error; err != nil {
		t.Fatalf("deliver BH1: %v", err)
	}
	if !has(flagged(t, svc, viewerTech), root.ID) {
		t.Fatalf("once BH1 is delivered KTV must see the button on the root again")
	}
	bh2, err := svc.CreateWarranty(admin, bh1.ID, okInput())
	if err != nil {
		t.Fatalf("BH2: %v", err)
	}
	sameIDs(t, "child after BH2", listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "child"}), []uint{bh1.ID, bh2.ID})

	// "root" = đã từng bảo hành = Counts>0 (gốc VÀ BH1 vì BH1 có BH2: khớp huy hiệu "Đã BH ×N").
	counts := func() []uint {
		var all []uint
		db.Model(&models.Order{}).Pluck("id", &all)
		c, err := repositories.New(db).Warranty.Counts(all)
		if err != nil {
			t.Fatalf("counts: %v", err)
		}
		var out []uint
		for _, id := range all {
			if c[id] > 0 {
				out = append(out, id)
			}
		}
		return out
	}
	sameIDs(t, "root vs Counts>0", listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "root"}), counts())
	if len(counts()) != 2 {
		t.Fatalf("expected root and BH1 to count as warrantied, got %v", counts())
	}
	sid := seller.ID
	sameIDs(t, "root + seller", listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "root", SellerID: &sid}), []uint{root.ID, bh1.ID})

	// 3. Soft-delete: BH2 bị xoá -> BH1 hết "đã từng BH", child chỉ còn BH1; đơn bị xoá không lọt vào đâu.
	if err := db.Delete(&models.Order{}, bh2.ID).Error; err != nil {
		t.Fatalf("soft delete BH2: %v", err)
	}
	sameIDs(t, "child after BH2 deleted", listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "child"}), []uint{bh1.ID})
	sameIDs(t, "root after BH2 deleted", listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "root"}), []uint{root.ID})
	sameIDs(t, "root vs Counts>0 after delete", listOrderIDs(t, svc, viewerTech, repositories.ItemFilter{Warranty: "root"}), counts())
	// Đơn gốc bị xoá: BH1 vẫn là đơn con còn sống, đơn gốc biến khỏi mọi danh sách.
	if err := db.Delete(&models.Order{}, root.ID).Error; err != nil {
		t.Fatalf("soft delete root: %v", err)
	}
	for _, w := range []string{"", "eligible", "child", "root"} {
		if has(listOrderIDs(t, svc, viewerOwner, repositories.ItemFilter{Warranty: w}), root.ID) {
			t.Fatalf("soft-deleted root leaked into warranty=%q", w)
		}
	}
	// BH2 xoá mềm không còn là "đang bảo hành dở" chặn ai: BH1 (đã giao) bấm được.
	if !has(flagged(t, svc, viewerTech), bh1.ID) {
		t.Fatalf("BH1 delivered with no live open BH: KTV must see the button")
	}

	// 4. Toàn cục: bộ lọc SQL = cờ trên dòng = CanPress từng đơn, cho từng vai trò.
	for _, role := range []models.Role{viewerTech, viewerOwner, models.RoleCS} {
		want := pressable(t, db, role)
		// pressable() reads soft-deleted rows too; keep only live ones.
		var live []uint
		for _, id := range want {
			var c int64
			db.Model(&models.Order{}).Where("id = ?", id).Count(&c)
			if c > 0 {
				live = append(live, id)
			}
		}
		sameIDs(t, "filter vs CanPress "+string(role), listOrderIDs(t, svc, role, repositories.ItemFilter{Warranty: "eligible", IncludeCancelled: true}), append([]uint{}, live...))
		sameIDs(t, "flag vs CanPress "+string(role), flagged(t, svc, role), append([]uint{}, live...))
	}
}

func TestWarrantyFilter(t *testing.T) {
	checkWarrantyFilter(t, newWarrantyDB(t))
}
