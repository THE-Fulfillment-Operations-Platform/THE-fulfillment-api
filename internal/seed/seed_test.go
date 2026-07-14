package seed

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/models"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.RoleRecord{}, &models.User{}, &models.Seller{}, &models.Store{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func cfgWith(demoUsers bool, password string) *config.Config {
	return &config.Config{SeedOnStart: true, SeedDemoUsers: demoUsers, DemoPassword: password}
}

func demoUser(t *testing.T, db *gorm.DB, email string) models.User {
	t.Helper()
	var u models.User
	if err := db.Where("email = ?", email).First(&u).Error; err != nil {
		t.Fatalf("load %s: %v", email, err)
	}
	return u
}

// TestRun_RotatesExistingDemoPassword locks in the password-sync fix: demo
// accounts seeded by an earlier deploy (with the old/default password) must be
// re-hashed to the currently configured SEED_DEMO_PASSWORD, otherwise rotating
// the env var passes config.Validate but leaves the well-known credential live.
func TestRun_RotatesExistingDemoPassword(t *testing.T) {
	db := newTestDB(t)
	if err := Run(db, cfgWith(true, config.DefaultDemoPassword)); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	if err := Run(db, cfgWith(true, "str0ng-rotated-pw")); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	owner := demoUser(t, db, "owner@the.local")
	if auth.CheckPassword(owner.PasswordHash, config.DefaultDemoPassword) {
		t.Fatal("old default password still valid after rotation")
	}
	if !auth.CheckPassword(owner.PasswordHash, "str0ng-rotated-pw") {
		t.Fatal("rotated password does not work")
	}
}

// TestRun_DeactivatesDemoUsersWhenFlagOff: SEED_DEMO_USERS=false must revoke
// previously seeded demo logins, not just skip creating them.
func TestRun_DeactivatesDemoUsersWhenFlagOff(t *testing.T) {
	db := newTestDB(t)
	if err := Run(db, cfgWith(true, config.DefaultDemoPassword)); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	if err := Run(db, cfgWith(false, config.DefaultDemoPassword)); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	var active int64
	if err := db.Model(&models.User{}).
		Where("email IN ? AND is_active = ?", demoEmails(), true).
		Count(&active).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if active != 0 {
		t.Fatalf("%d demo account(s) still active with SEED_DEMO_USERS=false", active)
	}

	// Turning the flag back on re-activates them for dev convenience.
	if err := Run(db, cfgWith(true, config.DefaultDemoPassword)); err != nil {
		t.Fatalf("third seed: %v", err)
	}
	if u := demoUser(t, db, "qc@the.local"); !u.IsActive {
		t.Fatal("demo account not re-activated when flag turned back on")
	}
}

// TestRun_IdempotentNoDuplicates: repeated boots must not duplicate rows.
func TestRun_IdempotentNoDuplicates(t *testing.T) {
	db := newTestDB(t)
	for i := 0; i < 2; i++ {
		if err := Run(db, cfgWith(true, config.DefaultDemoPassword)); err != nil {
			t.Fatalf("seed #%d: %v", i+1, err)
		}
	}
	var users int64
	if err := db.Model(&models.User{}).Count(&users).Error; err != nil {
		t.Fatalf("count users: %v", err)
	}
	if want := int64(len(demoUserDefs)); users != want {
		t.Fatalf("got %d users, want %d", users, want)
	}
}
