package services

import (
	"errors"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Nút "Bảo hành" (#3092): tạo đơn MỚI nối với đơn cũ, không đụng đơn cũ.

func newWarrantyDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{}, &models.ItemAsset{}, &models.Note{},
		&models.DailyCounter{}, &models.OrderWarranty{}, &models.AuditLog{},
		&models.Batch{}, &models.BatchItem{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedDelivered creates a delivered order with the given number of lines (qty each).
func seedDelivered(t *testing.T, db *gorm.DB, store string, qtys ...int) *models.Order {
	t.Helper()
	o := &models.Order{
		InternalCode: "I-" + store, StoreOrderID: store, StoreOrderRef: store, SellerID: 1,
		ShippingName: "An", ShippingAddress1: "1 Main St", ShippingCountry: "US", ShippingPhone: "1",
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusDelivered,
		TrackingStatus: models.TrackingDelivered, TrackingNumber: "TRK1",
	}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	for i, q := range qtys {
		it := &models.OrderItem{
			OrderID: o.ID, InternalCode: store + "_" + string(rune('a'+i)), LineNo: i + 1, SKUCode: "SKU" + string(rune('A'+i)),
			Quantity: q, DesignURL: "https://x/d" + string(rune('a'+i)) + ".png", MockupURL: "https://x/m.png",
			InternalStatus: models.StatusQCPassed, DesignStatus: models.DesignReady,
		}
		if err := db.Create(it).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}
	return o
}

var (
	owner = Actor{ID: 1, Role: models.RoleOwner}
	admin = Actor{ID: 2, Role: models.RoleAdmin}
	tech  = Actor{ID: 3, Role: models.RoleTechnician}
)

func okInput() CreateWarrantyInput {
	return CreateWarrantyInput{Reason: models.WarrantyReasonShippingDamage}
}

func status(err error) int {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

func TestCreateWarranty_NewLinkedOrderOldOneUntouched(t *testing.T) {
	db := newWarrantyDB(t)
	svc := newOrderService(db)
	old := seedDelivered(t, db, "SON20721", 1, 2)

	got, err := svc.CreateWarranty(admin, old.ID, okInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.ID == old.ID || got.StoreOrderID != "SON20721-BH1" || got.WarrantySeq != 1 {
		t.Fatalf("new order: id=%d store=%q seq=%d", got.ID, got.StoreOrderID, got.WarrantySeq)
	}
	if got.ParentOrderID == nil || *got.ParentOrderID != old.ID || got.RootOrderID == nil || *got.RootOrderID != old.ID {
		t.Fatalf("parent/root must point at the old order: %v %v", got.ParentOrderID, got.RootOrderID)
	}
	if got.ReviewStatus != models.ReviewPending || got.SellerStatus != models.SellerStatusProduction {
		t.Fatalf("new order must wait for review: %s / %s", got.ReviewStatus, got.SellerStatus)
	}
	if got.ShippingName != "An" || got.ShippingAddress1 != "1 Main St" || got.SellerID != 1 {
		t.Fatalf("recipient must be copied: %+v", got)
	}
	if got.InternalCode == old.InternalCode || strings.HasPrefix(got.InternalCode, "TMP-") {
		t.Fatalf("internal code must be stamped fresh, got %q", got.InternalCode)
	}
	if len(got.Items) != 2 || got.Items[0].DesignURL != "https://x/da.png" || got.Items[1].Quantity != 2 ||
		got.Items[0].InternalStatus != models.StatusPending {
		t.Fatalf("items must be copied afresh: %+v", got.Items)
	}
	var assets int64
	db.Model(&models.ItemAsset{}).Where("order_item_id = ?", got.Items[0].ID).Count(&assets)
	if assets == 0 {
		t.Fatalf("design assets must be copied for the new item")
	}

	// The old order is exactly as it was.
	var after models.Order
	db.First(&after, old.ID)
	if after.SellerStatus != models.SellerStatusDelivered || after.ReviewStatus != models.ReviewApproved ||
		after.TrackingNumber != "TRK1" || after.WarrantySeq != 0 {
		t.Fatalf("old order changed: %+v", after)
	}
	// ...and now shows "Đã bảo hành ×1".
	again, _ := svc.GetOrder(old.ID)
	if again.WarrantyCount != 1 {
		t.Fatalf("warranty_count: %d", again.WarrantyCount)
	}
	// The form's answers are stored for the carrier report.
	var rec models.OrderWarranty
	if err := db.Where("order_id = ?", got.ID).First(&rec).Error; err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Reason != models.WarrantyReasonShippingDamage || rec.Scope != models.WarrantyScopeFull || rec.SourceTrackingNumber != "TRK1" {
		t.Fatalf("record: %+v", rec)
	}
}

func TestCreateWarranty_SeqIncreasesAndCodeUsesRoot(t *testing.T) {
	db := newWarrantyDB(t)
	svc := newOrderService(db)
	root := seedDelivered(t, db, "SON1", 1)

	b1, err := svc.CreateWarranty(admin, root.ID, okInput())
	if err != nil {
		t.Fatal(err)
	}
	// BH1 gets delivered, then breaks again: BH2 hangs off BH1, code stays root-based.
	db.Model(&models.Order{}).Where("id = ?", b1.ID).Updates(map[string]any{
		"review_status": models.ReviewApproved, "seller_status": models.SellerStatusDelivered,
	})
	b2, err := svc.CreateWarranty(admin, b1.ID, okInput())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if b2.StoreOrderID != "SON1-BH2" || b2.WarrantySeq != 2 {
		t.Fatalf("want SON1-BH2 / seq 2, got %q / %d", b2.StoreOrderID, b2.WarrantySeq)
	}
	if *b2.ParentOrderID != b1.ID || *b2.RootOrderID != root.ID {
		t.Fatalf("parent=%d root=%d", *b2.ParentOrderID, *b2.RootOrderID)
	}
	// Root says ×2 (whole chain); BH1 says ×1 (its own child).
	r, _ := svc.GetOrder(root.ID)
	c, _ := svc.GetOrder(b1.ID)
	if r.WarrantyCount != 2 || c.WarrantyCount != 1 {
		t.Fatalf("counts root=%d child=%d", r.WarrantyCount, c.WarrantyCount)
	}
	list, err := svc.ListWarranties(root.ID)
	if err != nil || len(list) != 2 || list[0].Order.WarrantySeq != 1 || list[1].Order.WarrantySeq != 2 || list[1].Warranty == nil {
		t.Fatalf("chain list: %+v err=%v", list, err)
	}
}

func TestCreateWarranty_SeqNotReusedAfterDelete(t *testing.T) {
	db := newWarrantyDB(t)
	svc := newOrderService(db)
	root := seedDelivered(t, db, "SON2", 1)
	b1, _ := svc.CreateWarranty(admin, root.ID, okInput())
	db.Delete(&models.Order{}, b1.ID) // soft delete frees the open slot, not the number
	b2, err := svc.CreateWarranty(admin, root.ID, okInput())
	if err != nil || b2.WarrantySeq != 2 {
		t.Fatalf("want seq 2, got %v err=%v", b2, err)
	}
}

func TestCreateWarranty_DoubleClickBlockedExceptOwner(t *testing.T) {
	db := newWarrantyDB(t)
	svc := newOrderService(db)
	root := seedDelivered(t, db, "SON3", 1)
	if _, err := svc.CreateWarranty(admin, root.ID, okInput()); err != nil {
		t.Fatal(err)
	}
	// BH1 is still in review: no second one, from the root or anywhere in the chain.
	if _, err := svc.CreateWarranty(admin, root.ID, okInput()); status(err) != 409 {
		t.Fatalf("double click must be 409, got %v", err)
	}
	if _, err := svc.CreateWarranty(tech, root.ID, okInput()); status(err) != 409 {
		t.Fatalf("technician is not an override role, got %v", err)
	}
	info, _ := svc.WarrantyInfo(admin, root.ID)
	if info.CanWarranty || info.Blocking == nil || info.Blocking.StoreOrderID != "SON3-BH1" {
		t.Fatalf("info must say blocked by BH1: %+v", info)
	}
	// Owner may.
	b2, err := svc.CreateWarranty(owner, root.ID, okInput())
	if err != nil || b2.StoreOrderID != "SON3-BH2" {
		t.Fatalf("owner override: %v err=%v", b2, err)
	}
}

func TestCreateWarranty_ReturnedWarrantyOrderDoesNotBlock(t *testing.T) {
	db := newWarrantyDB(t)
	svc := newOrderService(db)
	root := seedDelivered(t, db, "SON4", 1)
	b1, _ := svc.CreateWarranty(admin, root.ID, okInput())
	// BH1 shipped and the carrier sent it back: finished, so the chain can go on.
	db.Model(&models.Order{}).Where("id = ?", b1.ID).Updates(map[string]any{
		"review_status": models.ReviewApproved, "seller_status": models.SellerStatusShipped,
		"tracking_status": models.TrackingUndelivered,
	})
	if _, err := svc.CreateWarranty(admin, b1.ID, okInput()); err != nil {
		t.Fatalf("returned BH1 must be warrantiable: %v", err)
	}
}

func TestCreateWarranty_Permissions(t *testing.T) {
	db := newWarrantyDB(t)
	svc := newOrderService(db)
	root := seedDelivered(t, db, "SON5", 1)
	for _, role := range []models.Role{models.RoleDesigner, models.RolePacking, models.RoleShipping, models.RoleCS, models.RoleSeller} {
		if _, err := svc.CreateWarranty(Actor{ID: 9, Role: role}, root.ID, okInput()); status(err) != 403 {
			t.Fatalf("%s must be 403, got %v", role, err)
		}
	}
	for _, a := range []Actor{owner, admin, tech, {ID: 6, Role: models.RoleOps}, {ID: 7, Role: models.RoleProduction}, {ID: 8, Role: models.RoleQC}} {
		o := seedDelivered(t, db, "OK-"+string(a.Role), 1)
		if _, err := svc.CreateWarranty(a, o.ID, okInput()); err != nil {
			t.Fatalf("%s must be allowed: %v", a.Role, err)
		}
	}
}

func TestCreateWarranty_WhichOrdersGetTheButton(t *testing.T) {
	db := newWarrantyDB(t)
	svc := newOrderService(db)
	mk := func(code string, seller models.SellerStatus, tr models.TrackingStatus, review models.ReviewStatus) uint {
		o := seedDelivered(t, db, code, 1)
		db.Model(o).Updates(map[string]any{"seller_status": seller, "tracking_status": tr, "review_status": review})
		return o.ID
	}
	cases := []struct {
		name string
		id   uint
		want bool
	}{
		{"delivered", mk("E1", models.SellerStatusDelivered, models.TrackingDelivered, models.ReviewApproved), true},
		{"carrier undelivered", mk("E2", models.SellerStatusShipped, models.TrackingUndelivered, models.ReviewApproved), true},
		{"carrier exception", mk("E3", models.SellerStatusShipped, models.TrackingException, models.ReviewApproved), true},
		{"in transit", mk("E4", models.SellerStatusShipped, models.TrackingInTransit, models.ReviewApproved), false},
		{"in production", mk("E5", models.SellerStatusProduction, models.TrackingNone, models.ReviewApproved), false},
		{"pending review", mk("E6", models.SellerStatusProduction, models.TrackingNone, models.ReviewPending), false},
	}
	for _, c := range cases {
		info, err := svc.WarrantyInfo(admin, c.id)
		if err != nil || info.CanWarranty != c.want {
			t.Errorf("%s: can=%v want %v (%s) err=%v", c.name, info.CanWarranty, c.want, info.Reason, err)
		}
		if !c.want {
			if _, err := svc.CreateWarranty(admin, c.id, okInput()); status(err) != 409 {
				t.Errorf("%s: create must be 409, got %v", c.name, err)
			}
		}
	}
}

func TestCreateWarranty_PartialAndAddress(t *testing.T) {
	db := newWarrantyDB(t)
	svc := newOrderService(db)
	old := seedDelivered(t, db, "SON6", 3, 1)
	var items []models.OrderItem
	db.Where("order_id = ?", old.ID).Order("line_no").Find(&items)

	in := okInput()
	in.Items = []WarrantyItemInput{{ItemID: items[0].ID, Quantity: 2}}
	in.Address = &WarrantyAddressInput{ShippingAddress1: "9 New Rd"}
	got, err := svc.CreateWarranty(admin, old.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Quantity != 2 || got.ShippingAddress1 != "9 New Rd" || got.ShippingName != "An" {
		t.Fatalf("partial/address: %+v", got)
	}
	var rec models.OrderWarranty
	db.Where("order_id = ?", got.ID).First(&rec)
	if rec.Scope != models.WarrantyScopePartial || !rec.AddressChanged {
		t.Fatalf("record: %+v", rec)
	}
	// Too many / foreign item / unknown reason are refused.
	bad := okInput()
	bad.Items = []WarrantyItemInput{{ItemID: items[1].ID, Quantity: 5}}
	if _, err := svc.CreateWarranty(owner, old.ID, bad); status(err) != 422 {
		t.Fatalf("qty too high: %v", err)
	}
	bad = okInput()
	bad.Items = []WarrantyItemInput{{ItemID: 99999}}
	if _, err := svc.CreateWarranty(owner, old.ID, bad); status(err) != 400 {
		t.Fatalf("foreign item: %v", err)
	}
	bad = okInput()
	bad.Reason = "WHATEVER"
	if _, err := svc.CreateWarranty(owner, old.ID, bad); status(err) != 422 {
		t.Fatalf("bad reason: %v", err)
	}
	bad = okInput()
	bad.Reason = models.WarrantyReasonOther
	if _, err := svc.CreateWarranty(owner, old.ID, bad); status(err) != 422 {
		t.Fatalf("other needs a note: %v", err)
	}
}

func TestCreateWarranty_DefaultIsFreeAndPolicyIsOneSwitch(t *testing.T) {
	db := newWarrantyDB(t)
	svc := newOrderService(db)
	a := seedDelivered(t, db, "SON7", 1)
	got, err := svc.CreateWarranty(admin, a.ID, okInput())
	if err != nil {
		t.Fatal(err)
	}
	var rec models.OrderWarranty
	db.Where("order_id = ?", got.ID).First(&rec)
	if rec.BillSeller {
		t.Fatalf("default must be free for the seller")
	}

	// One place to change the four choices.
	saved := WarrantyPolicy
	defer func() { WarrantyPolicy = saved }()
	WarrantyPolicy.BillSeller = true
	WarrantyPolicy.SuffixPrefix = " BH"
	WarrantyPolicy.AllowedRoles = append(append([]models.Role{}, WarrantyPolicy.AllowedRoles...), models.RoleCS)
	b := seedDelivered(t, db, "SON8", 1)
	got, err = svc.CreateWarranty(Actor{ID: 5, Role: models.RoleCS}, b.ID, okInput())
	if err != nil {
		t.Fatalf("CS added to policy must be allowed: %v", err)
	}
	rec = models.OrderWarranty{}
	db.Where("order_id = ?", got.ID).First(&rec)
	if got.StoreOrderID != "SON8 BH1" || !rec.BillSeller {
		t.Fatalf("policy: store=%q bill=%v", got.StoreOrderID, rec.BillSeller)
	}
}

// Tìm theo mã gốc ra cả chuỗi: bộ lọc store_order_id là "chứa" không phân biệt hoa thường.
func TestCreateWarranty_SearchByRootCodeFindsChain(t *testing.T) {
	db := newWarrantyDB(t)
	svc := newOrderService(db)
	root := seedDelivered(t, db, "SON20721", 1)
	if _, err := svc.CreateWarranty(admin, root.ID, okInput()); err != nil {
		t.Fatal(err)
	}
	seedDelivered(t, db, "OTHER99", 1)
	rows, total, err := repositories.New(db).Order.List(repositories.OrderFilter{Page: repositories.Page{Page: 1, PageSize: 50}, StoreOrderID: "son20721"})
	if err != nil || total != 2 {
		t.Fatalf("want root + BH1, got %d err=%v", total, err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.StoreOrderID] = true
	}
	if !got["SON20721"] || !got["SON20721-BH1"] {
		t.Fatalf("got %v", got)
	}
}
