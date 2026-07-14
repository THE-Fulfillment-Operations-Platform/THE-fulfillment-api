// Package seed populates the minimum data the system needs to run: the role
// catalog (required for RBAC), a demo seller (so the SELLER demo login has a
// seller to belong to) and — gated behind SEED_DEMO_USERS — one demo login per
// role. It deliberately does NOT seed any master catalog (materials, SKUs) or
// orders: that operational data is created by the real import/setup flows, so
// nothing junk reappears on restart.
package seed

import (
	"fmt"
	"log"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/models"
)

// Run seeds only what the system needs to run: the role catalog, a demo seller,
// and (gated) the demo login accounts. It is idempotent (find-or-create) and
// never seeds master catalog or orders — that data comes from the real
// import/setup flows, so nothing reappears on restart.
func Run(db *gorm.DB, cfg *config.Config) error {
	if err := seedRoles(db); err != nil {
		return err
	}
	seller, err := seedSeller(db)
	if err != nil {
		return err
	}
	// Demo login accounts (owner@the.local … with a shared password) are a
	// convenience for dev/testing. They are real, working credentials, so seeding
	// them is gated: SEED_DEMO_USERS must be on, and in production config.Validate
	// has already refused to boot if the password is still the default. Turning
	// the flag off must also revoke accounts seeded by earlier deploys, not just
	// stop creating new ones — otherwise the well-known credentials stay live.
	if cfg.SeedDemoUsers {
		if err := seedUsers(db, cfg, seller.ID); err != nil {
			return err
		}
	} else {
		if err := deactivateDemoUsers(db); err != nil {
			return err
		}
	}
	log.Println("seed: completed")
	return nil
}

func seedRoles(db *gorm.DB) error {
	descriptions := map[models.Role]string{
		models.RoleOwner:      "Chủ xưởng / ban điều hành",
		models.RoleAdmin:      "Quản trị hệ thống",
		models.RoleOps:        "Order ops / vận hành đơn",
		models.RoleDesigner:   "Designer: file in/cắt, tạo batch",
		models.RoleProduction: "Thợ in/cắt theo batch",
		models.RoleQC:         "Kiểm QC đối chiếu mockup",
		models.RolePacking:    "Đóng gói",
		models.RoleShipping:   "Bàn giao THE / shipping",
		models.RoleSeller:     "Seller: import & theo dõi trạng thái tổng",
	}
	for _, role := range models.AllRoles {
		rec := models.RoleRecord{Name: string(role), Description: descriptions[role]}
		if err := db.Where("name = ?", rec.Name).FirstOrCreate(&rec).Error; err != nil {
			return fmt.Errorf("seed roles: %w", err)
		}
	}
	return nil
}

func seedSeller(db *gorm.DB) (*models.Seller, error) {
	seller := models.Seller{Code: "SELLER01", Name: "Etsy Demo Seller", ContactEmail: "seller@the.local", Status: "active"}
	if err := db.Where("code = ?", seller.Code).FirstOrCreate(&seller).Error; err != nil {
		return nil, fmt.Errorf("seed seller: %w", err)
	}
	store := models.Store{SellerID: seller.ID, Name: "Etsy-Demo", Platform: "Etsy"}
	var existing models.Store
	if err := db.Where("seller_id = ? AND name = ?", seller.ID, store.Name).First(&existing).Error; err != nil {
		if err := db.Create(&store).Error; err != nil {
			return nil, fmt.Errorf("seed store: %w", err)
		}
	}
	return &seller, nil
}

// demoUserDefs is the fixed set of demo logins. Seeding, password sync and the
// SEED_DEMO_USERS=false deactivation path all key off these emails.
type demoUserDef struct {
	email, name string
	role        models.Role
	needsSeller bool
}

var demoUserDefs = []demoUserDef{
	{"owner@the.local", "Owner Demo", models.RoleOwner, false},
	{"admin@the.local", "Admin Demo", models.RoleAdmin, false},
	{"ops@the.local", "Ops Demo", models.RoleOps, false},
	{"designer@the.local", "Designer Demo", models.RoleDesigner, false},
	{"production@the.local", "Production Demo", models.RoleProduction, false},
	{"qc@the.local", "QC Demo", models.RoleQC, false},
	{"packing@the.local", "Packing Demo", models.RolePacking, false},
	{"shipping@the.local", "Shipping Demo", models.RoleShipping, false},
	{"seller@the.local", "Seller Demo", models.RoleSeller, true},
}

func demoEmails() []string {
	emails := make([]string, len(demoUserDefs))
	for i, d := range demoUserDefs {
		emails[i] = d.email
	}
	return emails
}

func seedUsers(db *gorm.DB, cfg *config.Config, sellerID uint) error {
	hash, err := auth.HashPassword(cfg.DemoPassword)
	if err != nil {
		return fmt.Errorf("seed users: hash: %w", err)
	}
	for _, d := range demoUserDefs {
		var existing models.User
		if err := db.Where("email = ?", d.email).First(&existing).Error; err == nil {
			// Keep pre-existing demo accounts in line with the configured
			// password: find-or-create alone would leave accounts from earlier
			// deploys on their old (possibly default) password, so rotating
			// SEED_DEMO_PASSWORD would silently change nothing.
			updates := map[string]any{}
			if !auth.CheckPassword(existing.PasswordHash, cfg.DemoPassword) {
				updates["password_hash"] = hash
			}
			if !existing.IsActive {
				updates["is_active"] = true
			}
			if len(updates) > 0 {
				if err := db.Model(&existing).Updates(updates).Error; err != nil {
					return fmt.Errorf("seed users: sync %s: %w", d.email, err)
				}
			}
			continue
		}
		var sellerRef *uint
		if d.needsSeller {
			sellerRef = &sellerID
		}
		u := models.User{
			Email: d.email, PasswordHash: hash, FullName: d.name,
			Role: d.role, SellerID: sellerRef, IsActive: true,
		}
		if err := db.Create(&u).Error; err != nil {
			return fmt.Errorf("seed users: %w", err)
		}
	}
	// Do not log the demo password — it would leak a valid credential into logs.
	log.Println("seed: demo users ready")
	return nil
}

// deactivateDemoUsers disables previously seeded demo logins when
// SEED_DEMO_USERS=false, so the flag revokes the shared credentials already in
// the database instead of merely skipping the insert.
func deactivateDemoUsers(db *gorm.DB) error {
	res := db.Model(&models.User{}).
		Where("email IN ? AND is_active = ?", demoEmails(), true).
		Update("is_active", false)
	if res.Error != nil {
		return fmt.Errorf("seed: deactivate demo users: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		log.Printf("seed: demo users disabled (%d account(s) deactivated, SEED_DEMO_USERS=false)", res.RowsAffected)
	} else {
		log.Println("seed: demo users skipped (SEED_DEMO_USERS=false)")
	}
	return nil
}

