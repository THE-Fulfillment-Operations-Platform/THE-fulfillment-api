package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

func newRoleFixture(t *testing.T) (*UserService, *models.User, *models.User) {
	t.Helper()
	db, svc := newUserFixture(t)
	if err := db.AutoMigrate(&models.RoleRecord{}); err != nil {
		t.Fatalf("migrate roles: %v", err)
	}
	// Role tick sets live in a process-wide registry: never leak into other tests.
	t.Cleanup(func() { models.SetRoleOverrides(nil) })
	owner := seedUser(t, db, "owner@t", models.RoleOwner)
	admin := seedUser(t, db, "admin@t", models.RoleAdmin)
	return svc, owner, admin
}

func roleView(t *testing.T, svc *UserService, actor Actor, role models.Role) RolePermissionView {
	t.Helper()
	views, err := svc.RolePermissions(actor)
	if err != nil {
		t.Fatalf("list role permissions: %v", err)
	}
	for _, v := range views {
		if v.Role == role {
			return v
		}
	}
	t.Fatalf("role %s missing from the list", role)
	return RolePermissionView{}
}

// TestRolePermissions_ChangeReachesFollowersOnly: đổi tick của một vai trò thì mọi
// tài khoản "theo vai trò" (chưa tick riêng) đổi theo ngay; tài khoản đã tick
// riêng giữ nguyên bộ tick của mình. "Về mặc định gốc" trả vai trò về bộ có sẵn.
func TestRolePermissions_ChangeReachesFollowersOnly(t *testing.T) {
	svc, owner, _ := newRoleFixture(t)
	follower := &models.User{Role: models.RoleOps}                                            // theo vai trò
	custom := &models.User{Role: models.RoleOps, Permissions: models.PermList{"orders.view"}} // tick riêng

	if !models.NewAccess(follower).Can("orders.manage") {
		t.Fatal("trước khi đổi: OPS mặc định phải có orders.manage")
	}
	in := PermsInput{Set: true, List: []string{"qc.manage"}}
	v, err := svc.SetRolePermissions(ownerActor(owner), models.RoleOps, in)
	if err != nil {
		t.Fatalf("owner đổi quyền OPS: %v", err)
	}
	if !v.Customized || len(v.Permissions) != 2 { // qc.manage + qc.view (manage kéo theo view)
		t.Fatalf("view sau khi lưu = %+v", v)
	}
	a := models.NewAccess(follower)
	if !a.Can("qc.manage") || !a.Can("qc.view") || a.Can("orders.manage") {
		t.Errorf("tài khoản theo vai trò phải đổi theo ngay: %v", a.Perms)
	}
	if b := models.NewAccess(custom); !b.Can("orders.view") || b.Can("qc.manage") {
		t.Errorf("tài khoản tick riêng phải giữ nguyên: %v", b.Perms)
	}

	if _, err := svc.SetRolePermissions(ownerActor(owner), models.RoleOps, PermsInput{Set: true}); err != nil {
		t.Fatalf("về mặc định gốc: %v", err)
	}
	if v := roleView(t, svc, ownerActor(owner), models.RoleOps); v.Customized {
		t.Errorf("về mặc định gốc mà vẫn đánh dấu đã tuỳ chỉnh: %+v", v)
	}
	if !models.NewAccess(follower).Can("orders.manage") {
		t.Error("về mặc định gốc: OPS phải có lại orders.manage")
	}

	// Lưu đúng bằng bộ có sẵn = coi như mặc định gốc (lưu NULL).
	builtin := PermsInput{Set: true, List: models.BuiltinRolePerms(models.RoleQC)}
	if v, err := svc.SetRolePermissions(ownerActor(owner), models.RoleQC, builtin); err != nil || v.Customized {
		t.Errorf("lưu trùng bộ có sẵn phải thành mặc định gốc: %+v err=%v", v, err)
	}
}

// TestRolePermissions_Guards: cùng ranh giới như sửa tài khoản.
func TestRolePermissions_Guards(t *testing.T) {
	svc, owner, admin := newRoleFixture(t)
	some := PermsInput{Set: true, List: []string{"orders.view"}}

	if _, err := svc.SetRolePermissions(adminActor(admin), models.RoleAdmin, some); err == nil {
		t.Error("ADMIN không được tự đổi quyền vai trò ADMIN của mình")
	}
	if _, err := svc.SetRolePermissions(adminActor(admin), models.RoleTechnician, some); err == nil {
		t.Error("ADMIN không được đổi quyền vai trò Kỹ thuật viên")
	}
	if _, err := svc.SetRolePermissions(adminActor(admin), models.RoleOps, some); err != nil {
		t.Errorf("ADMIN đổi quyền OPS: %v", err)
	}
	narrowed := Actor{ID: admin.ID, Role: models.RoleAdmin, Perms: map[string]bool{"orders.view": true}}
	if _, err := svc.SetRolePermissions(narrowed, models.RoleQC, PermsInput{Set: true, List: []string{"qc.manage"}}); err == nil {
		t.Error("không được cấp cho vai trò quyền mà chính mình không có")
	}
	for _, r := range []models.Role{models.RoleOwner, models.RoleSeller} {
		if _, err := svc.SetRolePermissions(ownerActor(owner), r, some); err == nil {
			t.Errorf("%s không phải bộ tick, không được lưu", r)
		}
	}
	if _, err := svc.SetRolePermissions(ownerActor(owner), models.RoleOps, PermsInput{Set: true, List: []string{"qc.mange"}}); err == nil {
		t.Error("quyền gõ sai phải bị từ chối, không được lặng lẽ bỏ qua")
	}
	if _, err := svc.SetRolePermissions(ownerActor(owner), models.RoleTechnician, some); err != nil {
		t.Errorf("OWNER đổi quyền Kỹ thuật viên: %v", err)
	}
	if v := roleView(t, svc, adminActor(admin), models.RoleAdmin); v.Editable || v.ReadOnlyReason == "" {
		t.Errorf("ADMIN xem vai trò ADMIN phải thấy khoá kèm lý do: %+v", v)
	}
}
