package repositories

import (
	"net/url"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// Chạy bộ lọc "Đã bàn giao" trên Postgres THẬT (prod dùng Postgres). Chỉ chạy khi
// có FFM_PG_TEST_DSN, ví dụ:
//
//	FFM_PG_TEST_DSN='postgres://ffm@127.0.0.1:55433/ffm_test?sslmode=disable' go test ./internal/repositories -run Postgres -v
//
// XOÁ SẠCH schema public của DB đó, nên chỉ chạy khi host là máy này và tên DB
// kết thúc bằng "_test".
func openHandoverPG(t *testing.T) *gorm.DB {
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

func TestBatchList_HandoverFilterPostgres(t *testing.T) {
	db := openHandoverPG(t)
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{},
		&models.Batch{}, &models.BatchItem{}, &models.BatchLink{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Khoá ngoại thật: seed seller 1 và material 1 mà các helper giả định.
	if err := db.Create(&models.Seller{Code: "S1", Name: "S1"}).Error; err != nil {
		t.Fatalf("seller: %v", err)
	}
	if err := db.Create(&models.Material{Code: "M1", Name: "M1"}).Error; err != nil {
		t.Fatalf("material: %v", err)
	}
	checkBatchHandoverFilter(t, db)
}

func TestItemList_QCFilterExcludesHandedOverPostgres(t *testing.T) {
	db := openHandoverPG(t)
	if err := db.AutoMigrate(
		&models.Seller{}, &models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Order{}, &models.OrderItem{},
		&models.Batch{}, &models.BatchItem{}, &models.BatchLink{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&models.Seller{Code: "S1", Name: "S1"}).Error; err != nil {
		t.Fatalf("seller: %v", err)
	}
	checkItemListHandoverFilter(t, db)
}
