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
	// has already refused to boot if the password is still the default.
	if cfg.SeedDemoUsers {
		if err := seedUsers(db, cfg, seller.ID); err != nil {
			return err
		}
	} else {
		log.Println("seed: demo users skipped (SEED_DEMO_USERS=false)")
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

func seedUsers(db *gorm.DB, cfg *config.Config, sellerID uint) error {
	hash, err := auth.HashPassword(cfg.DemoPassword)
	if err != nil {
		return fmt.Errorf("seed users: hash: %w", err)
	}
	type userDef struct {
		email, name string
		role        models.Role
		seller      *uint
	}
	defs := []userDef{
		{"owner@the.local", "Owner Demo", models.RoleOwner, nil},
		{"admin@the.local", "Admin Demo", models.RoleAdmin, nil},
		{"ops@the.local", "Ops Demo", models.RoleOps, nil},
		{"designer@the.local", "Designer Demo", models.RoleDesigner, nil},
		{"production@the.local", "Production Demo", models.RoleProduction, nil},
		{"qc@the.local", "QC Demo", models.RoleQC, nil},
		{"packing@the.local", "Packing Demo", models.RolePacking, nil},
		{"shipping@the.local", "Shipping Demo", models.RoleShipping, nil},
		{"seller@the.local", "Seller Demo", models.RoleSeller, &sellerID},
	}
	for _, d := range defs {
		var existing models.User
		if err := db.Where("email = ?", d.email).First(&existing).Error; err == nil {
			continue
		}
		u := models.User{
			Email: d.email, PasswordHash: hash, FullName: d.name,
			Role: d.role, SellerID: d.seller, IsActive: true,
		}
		if err := db.Create(&u).Error; err != nil {
			return fmt.Errorf("seed users: %w", err)
		}
	}
	// Do not log the demo password — it would leak a valid credential into logs.
	log.Println("seed: demo users ready")
	return nil
}

