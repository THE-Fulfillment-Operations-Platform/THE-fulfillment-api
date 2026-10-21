package models

import "testing"

// TECHNICIAN is the developer who tests the live system: it holds every tick,
// never narrowed by a stored list, same as OWNER.
func TestTechnicianHoldsEveryPermission(t *testing.T) {
	if !RoleTechnician.IsSuper() || !RoleOwner.IsSuper() || RoleAdmin.IsSuper() {
		t.Fatal("IsSuper must be exactly OWNER + TECHNICIAN")
	}
	got := EffectivePerms(RoleTechnician, PermList{"orders.view"})
	if len(got) != len(allPerms()) {
		t.Fatalf("a stored list must not narrow TECHNICIAN: %d of %d", len(got), len(allPerms()))
	}
	if !(&Access{Role: RoleTechnician}).Can(Manage(FeatOrders)) {
		t.Fatal("TECHNICIAN must pass every Can()")
	}
}
