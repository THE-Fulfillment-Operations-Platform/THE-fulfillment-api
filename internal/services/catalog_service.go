package services

import (
	"errors"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// CatalogService manages materials and SKUs (SKU ↔ material setup).
type CatalogService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// ---------- Materials ----------

type MaterialInput struct {
	Code        string `json:"code" binding:"required"`
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
}

func (s *CatalogService) CreateMaterial(actor Actor, in MaterialInput) (*models.Material, error) {
	in.Code = models.NormalizeCode(in.Code)
	if _, err := s.repo.Material.FindByCode(in.Code); err == nil {
		return nil, apperr.Conflict("A material with this code already exists")
	}
	m := &models.Material{Code: in.Code, Name: in.Name, Description: in.Description}
	if err := s.repo.Material.Create(m); err != nil {
		return nil, apperr.Internal("could not create material").Wrap(err)
	}
	s.audit.Log(actor, "MATERIAL_CREATE", "material", &m.ID, "Created material "+m.Code, nil)
	return m, nil
}

func (s *CatalogService) GetMaterial(id uint) (*models.Material, error) {
	m, err := s.repo.Material.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Material not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return m, nil
}

func (s *CatalogService) ListMaterials(page repositories.Page) ([]models.Material, int64, error) {
	return s.repo.Material.List(page.Normalize())
}

func (s *CatalogService) UpdateMaterial(actor Actor, id uint, in MaterialInput) (*models.Material, error) {
	m, err := s.GetMaterial(id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		m.Name = in.Name
	}
	m.Description = in.Description
	if err := s.repo.Material.Update(m); err != nil {
		return nil, apperr.Internal("could not update material").Wrap(err)
	}
	s.audit.Log(actor, "MATERIAL_UPDATE", "material", &m.ID, "Updated material "+m.Code, nil)
	return m, nil
}

func (s *CatalogService) DeleteMaterial(actor Actor, id uint) error {
	if _, err := s.GetMaterial(id); err != nil {
		return err
	}
	if err := s.repo.Material.Delete(id); err != nil {
		return apperr.Internal("could not delete material").Wrap(err)
	}
	s.audit.Log(actor, "MATERIAL_DELETE", "material", &id, "Deleted material", nil)
	return nil
}

// ---------- SKUs ----------

// SKUMaterialInput links a SKU to a material with a per-unit quantity.
type SKUMaterialInput struct {
	MaterialID      uint   `json:"material_id" binding:"required"`
	QuantityPerUnit int    `json:"quantity_per_unit"`
	Note            string `json:"note"`
}

type SKUInput struct {
	Code        string `json:"code" binding:"required"`
	Name        string `json:"name" binding:"required"`
	ProductName string `json:"product_name"`
	Description string `json:"description"`
	IsActive    *bool  `json:"is_active"`
	// Materials is optional: a SKU may be created "unmapped" and have its material(s)
	// assigned later from the Master Data → Mapping screen. On update, an empty/omitted
	// list leaves the existing material set unchanged (see UpdateSKU).
	Materials []SKUMaterialInput `json:"materials" binding:"omitempty,dive"`
}

// SKUUpdateInput is a partial-update payload for an existing SKU. Unlike SKUInput,
// code/name are NOT required: the mapping screen only submits `materials`, and
// UpdateSKU ignores code and treats name as optional. Requiring them here would
// reject a materials-only update with a spurious validation error.
type SKUUpdateInput struct {
	Name        string             `json:"name"`
	ProductName string             `json:"product_name"`
	Description string             `json:"description"`
	IsActive    *bool              `json:"is_active"`
	Materials   []SKUMaterialInput `json:"materials" binding:"omitempty,dive"`
}

func (s *CatalogService) buildMaterials(in []SKUMaterialInput) ([]models.SKUMaterial, error) {
	seen := map[uint]bool{}
	out := make([]models.SKUMaterial, 0, len(in))
	for _, m := range in {
		if _, err := s.repo.Material.FindByID(m.MaterialID); err != nil {
			return nil, apperr.BadRequest("material_id does not reference an existing material")
		}
		if seen[m.MaterialID] {
			return nil, apperr.BadRequest("duplicate material in SKU material list")
		}
		seen[m.MaterialID] = true
		qty := m.QuantityPerUnit
		if qty < 1 {
			qty = 1
		}
		out = append(out, models.SKUMaterial{MaterialID: m.MaterialID, QuantityPerUnit: qty, Note: m.Note})
	}
	return out, nil
}

// CreateSKU creates a SKU and its material set. A SKU with more than one material
// is automatically marked as a combo.
func (s *CatalogService) CreateSKU(actor Actor, in SKUInput) (*models.SKU, error) {
	in.Code = models.NormalizeCode(in.Code)
	if _, err := s.repo.SKU.FindByCode(in.Code); err == nil {
		return nil, apperr.Conflict("A SKU with this code already exists")
	}
	mats, err := s.buildMaterials(in.Materials)
	if err != nil {
		return nil, err
	}
	active := true
	if in.IsActive != nil {
		active = *in.IsActive
	}
	sku := &models.SKU{
		Code:        in.Code,
		Name:        in.Name,
		ProductName: in.ProductName,
		Description: in.Description,
		IsCombo:     len(mats) > 1,
		IsActive:    active,
		Materials:   mats,
	}
	if err := s.repo.SKU.Create(sku); err != nil {
		return nil, apperr.Internal("could not create SKU").Wrap(err)
	}
	full, _ := s.repo.SKU.FindByID(sku.ID)
	s.audit.Log(actor, "SKU_CREATE", "sku", &sku.ID, "Created SKU "+sku.Code, nil)
	return full, nil
}

func (s *CatalogService) GetSKU(id uint) (*models.SKU, error) {
	sku, err := s.repo.SKU.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("SKU not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return sku, nil
}

func (s *CatalogService) ListSKUs(page repositories.Page) ([]models.SKU, int64, error) {
	return s.repo.SKU.List(page.Normalize())
}

// UpdateSKU updates SKU attributes and (if provided) replaces its material set.
func (s *CatalogService) UpdateSKU(actor Actor, id uint, in SKUUpdateInput) (*models.SKU, error) {
	sku, err := s.GetSKU(id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		sku.Name = in.Name
	}
	sku.ProductName = in.ProductName
	sku.Description = in.Description
	if in.IsActive != nil {
		sku.IsActive = *in.IsActive
	}
	if len(in.Materials) > 0 {
		mats, err := s.buildMaterials(in.Materials)
		if err != nil {
			return nil, err
		}
		if err := s.repo.SKU.ReplaceMaterials(sku.ID, mats); err != nil {
			return nil, apperr.Internal("could not update SKU materials").Wrap(err)
		}
		sku.IsCombo = len(mats) > 1
	}
	sku.Materials = nil // avoid double-write of the association on Save
	if err := s.repo.SKU.Save(sku); err != nil {
		return nil, apperr.Internal("could not update SKU").Wrap(err)
	}
	full, _ := s.repo.SKU.FindByID(sku.ID)
	s.audit.Log(actor, "SKU_UPDATE", "sku", &sku.ID, "Updated SKU "+sku.Code, nil)
	return full, nil
}

func (s *CatalogService) DeleteSKU(actor Actor, id uint) error {
	if _, err := s.GetSKU(id); err != nil {
		return err
	}
	if err := s.repo.SKU.Delete(id); err != nil {
		return apperr.Internal("could not delete SKU").Wrap(err)
	}
	s.audit.Log(actor, "SKU_DELETE", "sku", &id, "Deleted SKU", nil)
	return nil
}
