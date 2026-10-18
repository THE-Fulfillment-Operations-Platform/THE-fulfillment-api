package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

// TestTechnician_HasAdminReach: kỹ thuật viên test toàn hệ thống nên mặc định
// thấy và thao tác được mọi màn, và qua được các cửa "Admin/Owner" (xoá đơn test
// của chính mình, cấu hình THE…). Cần gạt riêng của OWNER thì không có.
func TestTechnician_HasAdminReach(t *testing.T) {
	a := models.NewAccess(&models.User{Role: models.RoleTechnician})
	for _, p := range []string{models.Manage(models.FeatImport), models.Manage(models.FeatQC), models.View(models.FeatAudit)} {
		if !a.Can(p) {
			t.Errorf("TECHNICIAN thiếu quyền mặc định %s", p)
		}
	}
	if !models.RoleTechnician.HasAdminRights() {
		t.Error("TECHNICIAN phải qua được các cửa Admin/Owner")
	}
	for _, r := range []models.Role{models.RoleOps, models.RoleQC, models.RoleCS, models.RoleSeller} {
		if r.HasAdminRights() {
			t.Errorf("%s không được có quyền Admin", r)
		}
	}
}

// TestTechnician_OnlyOwnerOrTechnicianManagesTheAccount: role này mang công cụ
// test (tạo đơn lẻ), nên ADMIN không được tự cấp nó cho mình hay người khác,
// cũng không được sửa / khoá / xoá một tài khoản kỹ thuật viên.
func TestTechnician_OnlyOwnerOrTechnicianManagesTheAccount(t *testing.T) {
	db, svc := newUserFixture(t)
	owner := seedUser(t, db, "owner@t", models.RoleOwner)
	admin := seedUser(t, db, "admin@t", models.RoleAdmin)
	ops := seedUser(t, db, "ops@t", models.RoleOps)

	in := CreateUserInput{Email: "tech@t", Password: "secret1", FullName: "Tech", Role: models.RoleTechnician}
	if _, err := svc.Create(adminActor(admin), in); err == nil {
		t.Fatal("ADMIN không được tạo tài khoản kỹ thuật viên")
	}
	tech, err := svc.Create(ownerActor(owner), in)
	if err != nil {
		t.Fatalf("OWNER tạo kỹ thuật viên: %v", err)
	}
	techActor := Actor{ID: tech.ID, Role: models.RoleTechnician}

	techRole := models.RoleTechnician
	if _, err := svc.Update(adminActor(admin), ops.ID, UpdateUserInput{Role: &techRole}); err == nil {
		t.Error("ADMIN không được nâng ai lên kỹ thuật viên")
	}
	name := "renamed"
	if _, err := svc.Update(adminActor(admin), tech.ID, UpdateUserInput{FullName: &name}); err == nil {
		t.Error("ADMIN không được sửa tài khoản kỹ thuật viên")
	}
	if err := svc.Delete(adminActor(admin), tech.ID); err == nil {
		t.Error("ADMIN không được xoá tài khoản kỹ thuật viên")
	}

	// Kỹ thuật viên tự quản lý được tài khoản kỹ thuật viên khác.
	in2 := CreateUserInput{Email: "tech2@t", Password: "secret1", FullName: "Tech 2", Role: models.RoleTechnician}
	tech2, err := svc.Create(techActor, in2)
	if err != nil {
		t.Fatalf("kỹ thuật viên tạo kỹ thuật viên khác: %v", err)
	}
	if err := svc.Delete(techActor, tech2.ID); err != nil {
		t.Errorf("kỹ thuật viên xoá kỹ thuật viên khác: %v", err)
	}
	// …nhưng vẫn không đụng được OWNER.
	if _, err := svc.Update(techActor, owner.ID, UpdateUserInput{FullName: &name}); err == nil {
		t.Error("kỹ thuật viên không được sửa tài khoản OWNER")
	}
}
