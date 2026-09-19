package services

import (
	"encoding/json"
	"testing"

	"the-fulfillment/backend/internal/models"
)

func ownerActor(u *models.User) Actor { return Actor{ID: u.ID, Role: models.RoleOwner} }
func adminActor(u *models.User) Actor { return Actor{ID: u.ID, Role: models.RoleAdmin} }

// The three states of the permissions field: absent, null, a list.
func TestPermsInput_AbsentNullList(t *testing.T) {
	var in UpdateUserInput
	if err := json.Unmarshal([]byte(`{"full_name":"x"}`), &in); err != nil || in.Permissions.Set {
		t.Fatalf("absent: set=%v err=%v", in.Permissions.Set, err)
	}
	in = UpdateUserInput{}
	if err := json.Unmarshal([]byte(`{"permissions":null}`), &in); err != nil || !in.Permissions.Set || in.Permissions.List != nil {
		t.Fatalf("null: %+v err=%v", in.Permissions, err)
	}
	in = UpdateUserInput{}
	if err := json.Unmarshal([]byte(`{"permissions":[]}`), &in); err != nil || !in.Permissions.Set || in.Permissions.List == nil {
		t.Fatalf("empty list must stay non-nil: %+v err=%v", in.Permissions, err)
	}
}

func TestCreateUser_StoresNormalisedTicks(t *testing.T) {
	db, svc := newUserFixture(t)
	owner := seedUser(t, db, "owner@t", models.RoleOwner)

	in := CreateUserInput{Email: "qc@t", Password: "secret1", FullName: "QC", Role: models.RoleQC}
	in.Permissions = PermsInput{Set: true, List: []string{"qc.manage", "ship_queue.manage"}}
	u, err := svc.Create(ownerActor(owner), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	want := []string{"qc.manage", "qc.view", "ship_queue.manage", "ship_queue.view"}
	var got models.User
	db.First(&got, u.ID)
	if len(got.Permissions) != len(want) {
		t.Fatalf("stored = %v, want %v", got.Permissions, want)
	}
	for i := range want {
		if got.Permissions[i] != want[i] {
			t.Fatalf("stored = %v, want %v", got.Permissions, want)
		}
	}

	// No permissions sent → NULL → role defaults.
	u2, err := svc.Create(ownerActor(owner), CreateUserInput{Email: "p@t", Password: "secret1", FullName: "P", Role: models.RoleProduction})
	if err != nil {
		t.Fatalf("create default: %v", err)
	}
	var got2 models.User
	db.First(&got2, u2.ID)
	if got2.Permissions != nil {
		t.Fatalf("default user stored %v, want NULL", got2.Permissions)
	}

	bad := CreateUserInput{Email: "b@t", Password: "secret1", FullName: "B", Role: models.RoleQC}
	bad.Permissions = PermsInput{Set: true, List: []string{"qc.mange"}}
	if _, err := svc.Create(ownerActor(owner), bad); err == nil {
		t.Fatal("a typo'd permission must be rejected, not dropped")
	}
}

func TestUpdateUser_Guards(t *testing.T) {
	db, svc := newUserFixture(t)
	owner := seedUser(t, db, "owner@t", models.RoleOwner)
	admin := seedUser(t, db, "admin@t", models.RoleAdmin)
	ops := seedUser(t, db, "ops@t", models.RoleOps)

	ownerRole := models.RoleOwner
	if _, err := svc.Update(adminActor(admin), ops.ID, UpdateUserInput{Role: &ownerRole}); err == nil {
		t.Error("ADMIN must not promote anyone to OWNER")
	}
	name := "renamed"
	if _, err := svc.Update(adminActor(admin), owner.ID, UpdateUserInput{FullName: &name}); err == nil {
		t.Error("ADMIN must not edit an OWNER account")
	}
	self := UpdateUserInput{Permissions: PermsInput{Set: true, List: []string{"orders.view"}}}
	if _, err := svc.Update(adminActor(admin), admin.ID, self); err == nil {
		t.Error("ADMIN must not change their own ticks")
	}
	if _, err := svc.Update(adminActor(admin), admin.ID, UpdateUserInput{FullName: &name}); err != nil {
		t.Errorf("ADMIN editing their own name: %v", err)
	}

	// An ADMIN whose own ticks were narrowed cannot hand out what they lack.
	narrowed := Actor{ID: admin.ID, Role: models.RoleAdmin, Perms: map[string]bool{"orders.view": true}}
	grant := UpdateUserInput{Permissions: PermsInput{Set: true, List: []string{"qc.manage"}}}
	if _, err := svc.Update(narrowed, ops.ID, grant); err == nil {
		t.Error("granting a tick the actor lacks must be refused")
	}

	// Changing the role without sending ticks resets to the new role's defaults.
	custom := UpdateUserInput{Permissions: PermsInput{Set: true, List: []string{"notes.view"}}}
	if _, err := svc.Update(ownerActor(owner), ops.ID, custom); err != nil {
		t.Fatalf("set custom: %v", err)
	}
	qcRole := models.RoleQC
	u, err := svc.Update(ownerActor(owner), ops.ID, UpdateUserInput{Role: &qcRole})
	if err != nil {
		t.Fatalf("role change: %v", err)
	}
	if u.Permissions != nil {
		t.Errorf("role change kept old ticks %v, want role defaults", u.Permissions)
	}
	if !contains(u.EffectivePermissions, "qc.manage") {
		t.Errorf("effective = %v, want QC defaults", u.EffectivePermissions)
	}
}

// OWNER cannot be narrowed: whatever is stored, they keep everything.
func TestOwnerAlwaysHasEverything(t *testing.T) {
	a := models.NewAccess(&models.User{Role: models.RoleOwner, Permissions: models.PermList{}})
	if !a.Can(models.Manage(models.FeatQC)) || !a.Can(models.View(models.FeatAudit)) {
		t.Fatal("OWNER must hold every permission")
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
