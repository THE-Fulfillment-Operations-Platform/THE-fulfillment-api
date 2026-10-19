package database

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// TestCarryExportTicks: tài khoản / vai trò đã tick riêng mà xem được Kết quả QC
// hoặc Nhật ký thì được thêm quyền xuất (trước 19/10 xem là xuất được); tài khoản
// theo mặc định không bị đụng; và chạy lại KHÔNG tick lại cho người đã bị bỏ tick
// xuất sau đó.
func TestCarryExportTicks(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.RoleRecord{}, &models.DataMigration{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	custom := models.User{Email: "c@t", PasswordHash: "x", Role: models.RoleQC, IsActive: true,
		Permissions: models.PermList{"audit.view", "orders.view", "qc_results.view"}}
	follower := models.User{Email: "f@t", PasswordHash: "x", Role: models.RoleQC, IsActive: true}
	db.Create(&custom)
	db.Create(&follower)
	db.Create(&models.RoleRecord{Name: "PACKING", Permissions: models.PermList{"qc_results.view"}})

	if err := carryExportTicks(db); err != nil {
		t.Fatalf("carry: %v", err)
	}
	var got models.User
	db.First(&got, custom.ID)
	want := []string{"audit.manage", "audit.view", "orders.view", "qc_results.manage", "qc_results.view"}
	if len(got.Permissions) != len(want) {
		t.Fatalf("tick riêng sau migrate = %v, want %v", got.Permissions, want)
	}
	for i := range want {
		if got.Permissions[i] != want[i] {
			t.Fatalf("tick riêng sau migrate = %v, want %v", got.Permissions, want)
		}
	}
	// Biến mới: GORM không xoá giá trị cũ của struct khi cột là NULL.
	var fol models.User
	db.First(&fol, follower.ID)
	if fol.Permissions != nil {
		t.Errorf("tài khoản theo mặc định bị ghi đè: %v", fol.Permissions)
	}
	var role models.RoleRecord
	db.Where("name = ?", "PACKING").First(&role)
	if len(role.Permissions) != 2 {
		t.Errorf("vai trò đã cấu hình phải được thêm quyền xuất: %v", role.Permissions)
	}

	// Admin cố ý bỏ tick xuất → lần khởi động sau không được tick lại.
	db.Model(&models.User{}).Where("id = ?", custom.ID).Update("permissions", models.PermList{"qc_results.view"})
	if err := carryExportTicks(db); err != nil {
		t.Fatalf("carry lần 2: %v", err)
	}
	var again models.User
	db.First(&again, custom.ID)
	if len(again.Permissions) != 1 {
		t.Errorf("chạy lại đã tick lại quyền xuất người ta vừa bỏ: %v", again.Permissions)
	}
}
