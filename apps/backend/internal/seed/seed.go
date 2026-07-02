// Package seed populates demo data so the frontend can exercise the full flow:
// roles, one demo user per role, materials, SKUs (single + combo), a seller with
// a store, and a handful of orders/items in different states.
package seed

import (
	"fmt"
	"log"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/models"
)

// Run seeds the database. It is idempotent: master data uses find-or-create and
// demo orders are only created once (when no orders exist yet).
func Run(db *gorm.DB, cfg *config.Config) error {
	if err := seedRoles(db); err != nil {
		return err
	}
	materials, err := seedMaterials(db)
	if err != nil {
		return err
	}
	skus, err := seedSKUs(db, materials)
	if err != nil {
		return err
	}
	seller, err := seedSeller(db)
	if err != nil {
		return err
	}
	if err := seedUsers(db, cfg, seller.ID); err != nil {
		return err
	}
	if err := seedDemoOrders(db, seller, skus, materials); err != nil {
		return err
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

func seedMaterials(db *gorm.DB) (map[string]models.Material, error) {
	defs := []models.Material{
		{Code: "WOOD", Name: "Gỗ", Description: "Gỗ tự nhiên / plywood"},
		{Code: "MICA", Name: "Mica", Description: "Mica / acrylic mỏng"},
		{Code: "ACRYLIC", Name: "Acrylic", Description: "Acrylic dày"},
		{Code: "METAL", Name: "Metal", Description: "Kim loại"},
		// Real materials taken from the factory's operational spreadsheet ("Loại VL").
		// Codes match the legacy-import code generator so a re-import detects them.
		{Code: "MICA-TRONG-3-LY", Name: "Mica trong 3 ly", Description: "Mica trong dày 3 ly"},
		{Code: "MICA-START-HOLOGRAM", Name: "Mica start Hologram", Description: "Mica hiệu ứng hologram"},
		{Code: "GO-5-LY-3-LAYER", Name: "Gỗ 5 ly 3 layer", Description: "Gỗ dán 5 ly, 3 lớp"},
		{Code: "MDF-3LY-80X120", Name: "MDF 3ly 80x120", Description: "Ván MDF 3 ly khổ 80x120"},
	}
	out := map[string]models.Material{}
	for _, m := range defs {
		rec := m
		if err := db.Where("code = ?", m.Code).FirstOrCreate(&rec).Error; err != nil {
			return nil, fmt.Errorf("seed materials: %w", err)
		}
		out[rec.Code] = rec
	}
	return out, nil
}

func seedSKUs(db *gorm.DB, materials map[string]models.Material) (map[string]models.SKU, error) {
	type skuDef struct {
		code, name, product string
		mats                []string
	}
	defs := []skuDef{
		{"WOOD-01", "Wood Sign 01", "Personalized Wood Sign", []string{"WOOD"}},
		{"WOOD-03", "Wood Ornament 03", "Wood Ornament", []string{"WOOD"}},
		{"MICA-02", "Mica Plaque 02", "Mica Plaque", []string{"MICA"}},
		{"ACR-05", "Acrylic Stand 05", "Acrylic Stand", []string{"ACRYLIC"}},
		{"COMBO-01", "Combo Wood+Mica 01", "Combo Wood & Mica Sign", []string{"WOOD", "MICA"}},
		// Real SKUs from the factory spreadsheet. Codes are uppercased so an order
		// file SKU ("BR A 1.6 kep") matches after the importer uppercases it.
		// Mappings are set ONLY where the source data made them unambiguous — the
		// rest are intentionally left unmapped so the SKU_UNMAPPED / Master Data
		// flow can resolve them (we never guess a material).
		{"BR A 1.6 KEP", "BR A 1.6 kep", "Bracelet A 1.6 kẹp", []string{"MICA-TRONG-3-LY"}},
		{"BR SHC 2 KEP", "BR SHC 2 kep", "Bracelet SHC 2 kẹp", []string{"MICA-START-HOLOGRAM"}},
		{"3LWD 12IN", "3LWD 12in", "3-Layer Wood 12 inch", []string{"GO-5-LY-3-LAYER"}},
		{"BR HC 2.5 KEP", "BR HC 2.5 kep", "Bracelet HC 2.5 kẹp", nil}, // material chưa rõ
		{"BR A 1.6 GAI", "BR A 1.6 Gai", "Bracelet A 1.6 gai", nil},    // material chưa rõ
		{"NL C 5.5", "NL C 5.5", "NL C 5.5", nil},                      // material chưa rõ
	}
	out := map[string]models.SKU{}
	for _, d := range defs {
		var existing models.SKU
		if err := db.Where("code = ?", d.code).First(&existing).Error; err == nil {
			out[existing.Code] = existing
			continue
		}
		sku := models.SKU{
			Code: d.code, Name: d.name, ProductName: d.product,
			IsCombo: len(d.mats) > 1, IsActive: true,
		}
		for _, mc := range d.mats {
			sku.Materials = append(sku.Materials, models.SKUMaterial{
				MaterialID: materials[mc].ID, QuantityPerUnit: 1,
			})
		}
		if err := db.Create(&sku).Error; err != nil {
			return nil, fmt.Errorf("seed skus: %w", err)
		}
		out[sku.Code] = sku
	}
	return out, nil
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
	log.Printf("seed: demo users ready (password: %s)", cfg.DemoPassword)
	return nil
}

// seedDemoOrders creates a few orders/items in varied states (only once).
func seedDemoOrders(db *gorm.DB, seller *models.Seller, skus map[string]models.SKU, materials map[string]models.Material) error {
	var count int64
	db.Model(&models.Order{}).Count(&count)
	if count > 0 {
		return nil
	}

	type itemDef struct {
		skuCode     string
		qty         int
		mockup      string
		designReady bool
	}
	type orderDef struct {
		storeOrderID string
		items        []itemDef
	}
	defs := []orderDef{
		{"Etsy-7821", []itemDef{{"WOOD-01", 1, "https://mockups.example.com/etsy-7821-1.png", true}}},
		{"Etsy-7822", []itemDef{{"MICA-02", 1, "https://mockups.example.com/etsy-7822-1.png", false}}},
		{"Etsy-7823", []itemDef{{"COMBO-01", 1, "https://mockups.example.com/etsy-7823-1.png", true}}},
		{"Etsy-7824", []itemDef{{"WOOD-01", 2, "", false}}}, // missing mockup -> required attention
	}

	return db.Transaction(func(tx *gorm.DB) error {
		var woodReadyItemIDs []uint
		for _, od := range defs {
			order := models.Order{
				StoreOrderID: od.storeOrderID, StoreOrderRef: od.storeOrderID, SellerID: seller.ID,
				StoreName: "Etsy-Demo", ShippingMethod: "Standard", ShippingName: "John Doe",
				ShippingAddress1: "123 Demo St", ShippingCity: "Austin", ShippingProvince: "TX",
				ShippingZip: "78701", ShippingCountry: "US", ShippingEmail: "john@example.com",
				SellerStatus: models.SellerStatusProduction,
			}
			if err := tx.Create(&order).Error; err != nil {
				return err
			}
			order.InternalCode = fmt.Sprintf("ORD-%06d", order.ID)
			if err := tx.Save(&order).Error; err != nil {
				return err
			}

			for i, it := range od.items {
				sku := skus[it.skuCode]
				ds := models.DesignPending
				if it.mockup == "" {
					ds = models.DesignMissing
				} else if it.designReady {
					ds = models.DesignReady
				}
				item := models.OrderItem{
					OrderID: order.ID, LineNo: i + 1,
					InternalCode: fmt.Sprintf("ORD-%06d_%d", order.ID, i+1),
					SKUID:        &sku.ID, SKUCode: sku.Code, ProductName: sku.ProductName,
					Quantity: it.qty, MockupURL: it.mockup,
					InternalStatus: models.StatusPending, DesignStatus: ds,
				}
				if err := tx.Create(&item).Error; err != nil {
					return err
				}
				if it.mockup != "" {
					if err := tx.Create(&models.ItemAsset{
						OrderItemID: item.ID, AssetType: "MOCKUP", URL: it.mockup, Version: 1,
					}).Error; err != nil {
						return err
					}
				} else {
					if err := tx.Create(&models.Note{
						Title: "Thiếu Mockup URL", Body: "Item " + item.InternalCode + " chưa có mockup để QC.",
						ReasonCode: "ART_MISSING", Severity: models.SeverityHigh, Status: models.NoteOpen,
						IsRequiredAttention: true, EntityType: models.EntityOrderItem, EntityID: &item.ID,
						OwnerRole: models.RoleDesigner,
					}).Error; err != nil {
						return err
					}
				}
				// Track design-ready wood items for a demo batch.
				if it.designReady && strings.HasPrefix(it.skuCode, "WOOD") {
					woodReadyItemIDs = append(woodReadyItemIDs, item.ID)
				}
			}
		}

		// Demo batch: wood material, containing the design-ready wood item(s), at PRINTED.
		if len(woodReadyItemIDs) > 0 {
			wood := materials["WOOD"]
			batch := models.Batch{MaterialID: wood.ID, Status: models.StatusPrinted, Priority: models.PriorityNormal}
			if err := tx.Create(&batch).Error; err != nil {
				return err
			}
			batch.Code = fmt.Sprintf("#%d", 101000+batch.ID)
			if err := tx.Save(&batch).Error; err != nil {
				return err
			}
			for _, itemID := range woodReadyItemIDs {
				bi := models.BatchItem{
					BatchID: batch.ID, OrderItemID: itemID, MaterialID: wood.ID, Status: models.StatusPrinted,
				}
				if err := tx.Create(&bi).Error; err != nil {
					return err
				}
				// Item internal status follows its (single) wood part here.
				if err := tx.Model(&models.OrderItem{}).Where("id = ?", itemID).
					Update("internal_status", models.StatusPrinted).Error; err != nil {
					return err
				}
			}
			log.Printf("seed: demo batch %s created (PRINTED) with %d item(s)", batch.Code, len(woodReadyItemIDs))
		}

		log.Printf("seed: %d demo orders created", len(defs))
		return nil
	})
}
