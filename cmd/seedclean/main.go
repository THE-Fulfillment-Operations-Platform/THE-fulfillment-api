// Command seedclean removes the legacy demo *catalog* rows that the old startup
// seeder used to create (materials + SKUs like WOOD-01, COMBO-01, …). It is a
// one-off cleanup for systems that were booted before that seeding was removed.
//
// It is reference-safe: a SKU that is already used by an order, or a material
// still referenced by any (real) SKU mapping or batch, is SKIPPED and logged —
// never deleted. It never touches roles, users or the demo seller (those are
// intentionally kept as the login path).
//
// Usage:
//
//	go run ./cmd/seedclean            # dry run: print what WOULD be deleted
//	go run ./cmd/seedclean --apply    # actually delete
package main

import (
	"flag"
	"log"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/database"
	"the-fulfillment/backend/internal/models"
)

// Raw codes the old seeder created. They are normalised the same way the models
// do on save, so we match whatever ended up stored (e.g. "BR A 1.6 KEP" →
// "BR-A-1.6-KEP").
var seedSKUCodes = []string{
	"WOOD-01", "WOOD-03", "MICA-02", "ACR-05", "COMBO-01",
	"BR A 1.6 KEP", "BR SHC 2 KEP", "3LWD 12IN", "BR HC 2.5 KEP", "BR A 1.6 GAI", "NL C 5.5",
}

var seedMaterialCodes = []string{
	"WOOD", "MICA", "ACRYLIC", "METAL",
	"MICA-TRONG-3-LY", "MICA-START-HOLOGRAM", "GO-5-LY-3-LAYER", "MDF-3LY-80X120",
}

func main() {
	apply := flag.Bool("apply", false, "actually delete (default: dry run)")
	flag.Parse()

	cfg := config.Load()
	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}

	if !*apply {
		log.Println("seedclean: DRY RUN (pass --apply to delete). Nothing will be changed.")
	}

	var skusDeleted, skusSkipped, matsDeleted, matsSkipped int

	// 1) Demo SKUs — skip any already used by an order item.
	for _, raw := range seedSKUCodes {
		code := models.NormalizeCode(raw)
		var sku models.SKU
		if err := db.Where("code = ?", code).First(&sku).Error; err != nil {
			continue // not present — already clean
		}
		var refs int64
		db.Model(&models.OrderItem{}).Where("sku_id = ? OR sku_code = ?", sku.ID, code).Count(&refs)
		if refs > 0 {
			log.Printf("SKIP sku %s — đang được %d order item dùng", code, refs)
			skusSkipped++
			continue
		}
		if *apply {
			if err := db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Unscoped().Where("sku_id = ?", sku.ID).Delete(&models.SKUMaterial{}).Error; err != nil {
					return err
				}
				return tx.Unscoped().Delete(&models.SKU{}, sku.ID).Error
			}); err != nil {
				log.Printf("ERR delete sku %s: %v", code, err)
				continue
			}
		}
		log.Printf("DEL  sku %s", code)
		skusDeleted++
	}

	// 2) Demo materials — skip any still referenced by a SKU mapping or a batch item.
	for _, raw := range seedMaterialCodes {
		code := models.NormalizeCode(raw)
		var mat models.Material
		if err := db.Where("code = ?", code).First(&mat).Error; err != nil {
			continue
		}
		var mapRefs, batchRefs int64
		db.Model(&models.SKUMaterial{}).Where("material_id = ?", mat.ID).Count(&mapRefs)
		db.Model(&models.BatchItem{}).Where("material_id = ?", mat.ID).Count(&batchRefs)
		if mapRefs > 0 || batchRefs > 0 {
			log.Printf("SKIP material %s — còn %d mapping SKU + %d batch item tham chiếu", code, mapRefs, batchRefs)
			matsSkipped++
			continue
		}
		if *apply {
			if err := db.Unscoped().Delete(&models.Material{}, mat.ID).Error; err != nil {
				log.Printf("ERR delete material %s: %v", code, err)
				continue
			}
		}
		log.Printf("DEL  material %s", code)
		matsDeleted++
	}

	verb := "would delete"
	if *apply {
		verb = "deleted"
	}
	log.Printf("seedclean done: %s %d SKU (skipped %d), %d material (skipped %d)",
		verb, skusDeleted, skusSkipped, matsDeleted, matsSkipped)
}
