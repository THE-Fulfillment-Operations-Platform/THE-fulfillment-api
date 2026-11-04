package services

import (
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/database"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Chạy trên Postgres THẬT khi có FFM_PG_TEST_DSN, ví dụ:
//
//	FFM_PG_TEST_DSN='postgres://ffm@127.0.0.1:55432/ffm_test?sslmode=disable' go test ./internal/services -run TestWarrantyPostgres -v
//
// Bỏ qua (skip) nếu không đặt. Test XOÁ SẠCH schema public của DB đó, nên chỉ
// chạy khi host là máy này và tên DB kết thúc bằng "_test" — không bao giờ chạy
// trên DB dev chung hay prod.
func openWarrantyPG(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("FFM_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("FFM_PG_TEST_DSN không đặt — bỏ qua test Postgres")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	host := u.Hostname()
	if (host != "127.0.0.1" && host != "localhost" && host != "::1") || !strings.HasSuffix(strings.TrimPrefix(u.Path, "/"), "_test") {
		t.Fatalf("từ chối chạy: chỉ DB local có tên *_test (host=%q db=%q)", host, u.Path)
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public").Error; err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	return db
}

func TestWarrantyPostgres(t *testing.T) {
	db := openWarrantyPG(t)

	// 1. Một DB "cũ": schema trước tính năng bảo hành, đã có vài đơn.
	if err := database.AutoMigrate(db); err != nil {
		t.Fatalf("migrate (fresh): %v", err)
	}
	if err := db.Exec(`DROP INDEX IF EXISTS uniq_orders_warranty_seq;
		DROP TABLE order_warranties;
		ALTER TABLE orders DROP COLUMN parent_order_id, DROP COLUMN root_order_id, DROP COLUMN warranty_seq`).Error; err != nil {
		t.Fatalf("roll back to pre-warranty schema: %v", err)
	}
	seller := &models.Seller{Code: "S1", Name: "S1"}
	if err := db.Create(seller).Error; err != nil {
		t.Fatalf("seller: %v", err)
	}
	var olds []uint
	for _, code := range []string{"OLD1", "OLD2", "OLD3"} {
		o := &models.Order{InternalCode: "I-" + code, StoreOrderID: code, StoreOrderRef: code, SellerID: seller.ID,
			ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusDelivered, TrackingStatus: models.TrackingDelivered}
		// The pre-warranty schema has no warranty columns: leave them out of the INSERT.
		if err := db.Omit("ParentOrderID", "RootOrderID", "WarrantySeq").Create(o).Error; err != nil {
			t.Fatalf("old order: %v", err)
		}
		olds = append(olds, o.ID)
		if err := db.Create(&models.OrderItem{OrderID: o.ID, InternalCode: "IT-" + code, LineNo: 1, SKUCode: "SKU", Quantity: 1,
			DesignURL: "https://x/d.png", MockupURL: "https://x/m.png", InternalStatus: models.StatusQCPassed, DesignStatus: models.DesignReady}).Error; err != nil {
			t.Fatalf("old item: %v", err)
		}
	}

	// 2. AutoMigrate lại trên DB có đơn cũ: thêm cột, bảng, index; đơn cũ còn nguyên.
	if err := database.AutoMigrate(db); err != nil {
		t.Fatalf("migrate over existing orders: %v", err)
	}
	var seq0 int64
	db.Model(&models.Order{}).Where("warranty_seq = 0 AND parent_order_id IS NULL AND root_order_id IS NULL").Count(&seq0)
	if seq0 != 3 {
		t.Fatalf("old orders must survive with warranty_seq=0, got %d", seq0)
	}
	var idxDef string
	if err := db.Raw(`SELECT indexdef FROM pg_indexes WHERE indexname = 'uniq_orders_warranty_seq'`).Scan(&idxDef).Error; err != nil || idxDef == "" {
		t.Fatalf("partial unique index missing (err=%v)", err)
	}
	t.Logf("index: %s", idxDef)
	// Nó thật sự duy nhất: hai đơn sống cùng (root, seq) bị từ chối.
	root := olds[0]
	mk := func(code string) error {
		return db.Create(&models.Order{InternalCode: code, StoreOrderID: code, SellerID: seller.ID, RootOrderID: &root, WarrantySeq: 7}).Error
	}
	if err := mk("DUP-A"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := mk("DUP-B"); err == nil {
		t.Fatalf("second order with the same (root, seq) must violate the unique index")
	}
	db.Unscoped().Where("warranty_seq = 7").Delete(&models.Order{})

	repo := repositories.New(db)
	// 3. LockRoot chạy FOR UPDATE thật (không lỗi cú pháp trên Postgres).
	if err := db.Transaction(func(tx *gorm.DB) error { return repositories.New(tx).Warranty.LockRoot(root) }); err != nil {
		t.Fatalf("LockRoot: %v", err)
	}

	// 4. Hai request cùng lúc trên cùng một đơn: đúng 1 thành công, 1 bị chặn 409, không 500.
	svc := &OrderService{repo: repo, audit: &AuditService{repo: repo}}
	for round, id := range olds {
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = svc.CreateWarranty(admin, id, okInput())
			}(i)
		}
		close(start)
		wg.Wait()
		ok, blocked := 0, 0
		for _, e := range errs {
			switch {
			case e == nil:
				ok++
			case status(e) == 409:
				blocked++
			default:
				t.Fatalf("round %d: unexpected error (want nil or 409): %v", round, e)
			}
		}
		if ok != 1 || blocked != 1 {
			t.Fatalf("round %d: want 1 success + 1 blocked, got %d + %d (%v)", round, ok, blocked, errs)
		}
		var n int64
		db.Model(&models.Order{}).Where("root_order_id = ?", id).Count(&n)
		if n != 1 {
			t.Fatalf("round %d: exactly one warranty order expected, got %d", round, n)
		}
	}

	// 5. Hai Owner (được vượt chặn bấm đúp) cùng lúc: vẫn được xếp hàng, số khác nhau, không lỗi.
	var wg sync.WaitGroup
	start := make(chan struct{})
	seqs := make([]int, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			o, err := svc.CreateWarranty(owner, olds[0], okInput())
			errs[i] = err
			if err == nil {
				seqs[i] = o.WarrantySeq
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || seqs[0] == seqs[1] || seqs[0] == 0 || seqs[1] == 0 {
		t.Fatalf("two owners: want distinct seqs and no error, got %v / %v", seqs, errs)
	}
	t.Logf("owners got seq %v", seqs)
}

// Bộ lọc "Bảo hành" trên Postgres thật (cùng kiểm tra với sqlite).
func TestWarrantyFilterPostgres(t *testing.T) {
	db := openWarrantyPG(t)
	if err := database.AutoMigrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	checkWarrantyFilter(t, db)
}
