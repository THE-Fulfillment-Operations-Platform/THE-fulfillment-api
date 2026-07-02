package repositories

import (
	"errors"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// ---------- Users ----------

type UserRepository struct{ db *gorm.DB }

func (r *UserRepository) Create(u *models.User) error { return r.db.Create(u).Error }
func (r *UserRepository) Update(u *models.User) error { return r.db.Save(u).Error }

func (r *UserRepository) FindByID(id uint) (*models.User, error) {
	var u models.User
	if err := r.db.Preload("Seller").First(&u, id).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *UserRepository) FindByEmail(email string) (*models.User, error) {
	var u models.User
	if err := r.db.Preload("Seller").Where("email = ?", email).First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *UserRepository) List(p Page) ([]models.User, int64, error) {
	var users []models.User
	var total int64
	r.db.Model(&models.User{}).Count(&total)
	err := r.db.Preload("Seller").Order("id asc").
		Limit(p.PageSize).Offset(p.Offset()).Find(&users).Error
	return users, total, err
}

func (r *UserRepository) Delete(id uint) error {
	return r.db.Delete(&models.User{}, id).Error
}

func (r *UserRepository) ExistsByEmail(email string) (bool, error) {
	var count int64
	err := r.db.Model(&models.User{}).Where("email = ?", email).Count(&count).Error
	return count > 0, err
}

// ---------- Sellers ----------

type SellerRepository struct{ db *gorm.DB }

func (r *SellerRepository) Create(s *models.Seller) error { return r.db.Create(s).Error }
func (r *SellerRepository) Update(s *models.Seller) error { return r.db.Save(s).Error }
func (r *SellerRepository) Delete(id uint) error          { return r.db.Delete(&models.Seller{}, id).Error }

func (r *SellerRepository) FindByID(id uint) (*models.Seller, error) {
	var s models.Seller
	if err := r.db.Preload("Stores").First(&s, id).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SellerRepository) FindByCode(code string) (*models.Seller, error) {
	var s models.Seller
	if err := r.db.Where("code = ?", code).First(&s).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SellerRepository) List(p Page) ([]models.Seller, int64, error) {
	var rows []models.Seller
	var total int64
	r.db.Model(&models.Seller{}).Count(&total)
	err := r.db.Order("id asc").Limit(p.PageSize).Offset(p.Offset()).Find(&rows).Error
	return rows, total, err
}

// ---------- Stores ----------

type StoreRepository struct{ db *gorm.DB }

func (r *StoreRepository) Create(s *models.Store) error { return r.db.Create(s).Error }
func (r *StoreRepository) Update(s *models.Store) error { return r.db.Save(s).Error }
func (r *StoreRepository) Delete(id uint) error         { return r.db.Delete(&models.Store{}, id).Error }

func (r *StoreRepository) FindByID(id uint) (*models.Store, error) {
	var s models.Store
	if err := r.db.First(&s, id).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *StoreRepository) List(p Page, sellerID *uint) ([]models.Store, int64, error) {
	var rows []models.Store
	var total int64
	q := r.db.Model(&models.Store{})
	if sellerID != nil {
		q = q.Where("seller_id = ?", *sellerID)
	}
	q.Count(&total)
	err := q.Order("id asc").Limit(p.PageSize).Offset(p.Offset()).Find(&rows).Error
	return rows, total, err
}

func (r *StoreRepository) FindByNameAndSeller(sellerID uint, name string) (*models.Store, error) {
	var s models.Store
	err := r.db.Where("seller_id = ? AND name = ?", sellerID, name).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ---------- Materials ----------

type MaterialRepository struct{ db *gorm.DB }

func (r *MaterialRepository) Create(m *models.Material) error { return r.db.Create(m).Error }
func (r *MaterialRepository) Update(m *models.Material) error { return r.db.Save(m).Error }
func (r *MaterialRepository) Delete(id uint) error            { return r.db.Delete(&models.Material{}, id).Error }

func (r *MaterialRepository) FindByID(id uint) (*models.Material, error) {
	var m models.Material
	if err := r.db.First(&m, id).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *MaterialRepository) FindByCode(code string) (*models.Material, error) {
	var m models.Material
	if err := r.db.Where("code = ?", code).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// FindByNameInsensitive matches a material on a case-insensitive, trimmed name.
// Used by the legacy master-data import so the same raw "Loại VL" string is not
// created twice. Returns (nil, nil) when nothing matches.
func (r *MaterialRepository) FindByNameInsensitive(name string) (*models.Material, error) {
	var m models.Material
	err := r.db.Where("LOWER(TRIM(name)) = ?", strings.ToLower(strings.TrimSpace(name))).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *MaterialRepository) List(p Page) ([]models.Material, int64, error) {
	var rows []models.Material
	var total int64
	r.db.Model(&models.Material{}).Count(&total)
	err := r.db.Order("id asc").Limit(p.PageSize).Offset(p.Offset()).Find(&rows).Error
	return rows, total, err
}

// ---------- SKUs ----------

type SKURepository struct{ db *gorm.DB }

func (r *SKURepository) Create(s *models.SKU) error { return r.db.Create(s).Error }
func (r *SKURepository) Delete(id uint) error       { return r.db.Delete(&models.SKU{}, id).Error }

// Save persists a SKU together with its material set inside a transaction.
func (r *SKURepository) Save(s *models.SKU) error {
	return r.db.Session(&gorm.Session{FullSaveAssociations: true}).Save(s).Error
}

func (r *SKURepository) ReplaceMaterials(skuID uint, mats []models.SKUMaterial) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("sku_id = ?", skuID).Delete(&models.SKUMaterial{}).Error; err != nil {
			return err
		}
		for i := range mats {
			mats[i].ID = 0
			mats[i].SKUID = skuID
		}
		if len(mats) > 0 {
			return tx.Create(&mats).Error
		}
		return nil
	})
}

func (r *SKURepository) FindByID(id uint) (*models.SKU, error) {
	var s models.SKU
	if err := r.db.Preload("Materials.Material").First(&s, id).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SKURepository) FindByCode(code string) (*models.SKU, error) {
	var s models.SKU
	err := r.db.Preload("Materials.Material").Where("code = ?", code).First(&s).Error
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SKURepository) List(p Page) ([]models.SKU, int64, error) {
	var rows []models.SKU
	var total int64
	r.db.Model(&models.SKU{}).Count(&total)
	err := r.db.Preload("Materials.Material").Order("id asc").
		Limit(p.PageSize).Offset(p.Offset()).Find(&rows).Error
	return rows, total, err
}

// CountMaterials returns how many materials a SKU is mapped to. Used by the
// order-import validator to distinguish "SKU exists but has no material" from a
// fully set-up SKU.
func (r *SKURepository) CountMaterials(skuID uint) (int64, error) {
	var n int64
	err := r.db.Model(&models.SKUMaterial{}).Where("sku_id = ?", skuID).Count(&n).Error
	return n, err
}

// MappingExists reports whether a (sku, material) mapping already exists.
func (r *SKURepository) MappingExists(skuID, materialID uint) (bool, error) {
	var n int64
	err := r.db.Model(&models.SKUMaterial{}).
		Where("sku_id = ? AND material_id = ?", skuID, materialID).Count(&n).Error
	return n > 0, err
}

// AddMaterial appends a single material to a SKU (idempotent per unique index).
// It never removes existing materials, so it is safe for additive legacy imports.
func (r *SKURepository) AddMaterial(skuID, materialID uint, qty int, note string) error {
	if qty < 1 {
		qty = 1
	}
	return r.db.Create(&models.SKUMaterial{
		SKUID: skuID, MaterialID: materialID, QuantityPerUnit: qty, Note: note,
	}).Error
}

// ---------- Master-data import jobs ----------

type MasterImportRepository struct{ db *gorm.DB }

func (r *MasterImportRepository) Create(j *models.MasterImportJob) error { return r.db.Create(j).Error }
func (r *MasterImportRepository) Update(j *models.MasterImportJob) error { return r.db.Save(j).Error }

func (r *MasterImportRepository) FindByID(id uint) (*models.MasterImportJob, error) {
	var j models.MasterImportJob
	if err := r.db.First(&j, id).Error; err != nil {
		return nil, err
	}
	return &j, nil
}

func (r *MasterImportRepository) List(p Page) ([]models.MasterImportJob, int64, error) {
	var rows []models.MasterImportJob
	var total int64
	r.db.Model(&models.MasterImportJob{}).Count(&total)
	err := r.db.Order("id desc").Limit(p.PageSize).Offset(p.Offset()).Find(&rows).Error
	return rows, total, err
}
