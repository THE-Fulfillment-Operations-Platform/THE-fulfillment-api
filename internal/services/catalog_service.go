package services

import (
	"errors"
	"fmt"

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
	// ProductsPerUnit is the production quota (max products per unit of this
	// material). Only OWNER may set it; other roles' values are ignored. nil or ≤0
	// means unlimited (batches for this material are never split).
	ProductsPerUnit *int `json:"products_per_unit"`
}

// MaterialUpdateInput is a partial-update payload for a material. Unlike
// MaterialInput, code/name are NOT required: the Materials screen edits name /
// description / quota and never resends the (immutable) code — mirroring the
// SKUUpdateInput convention so a code-less update isn't rejected by validation.
type MaterialUpdateInput struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	ProductsPerUnit *int   `json:"products_per_unit"`
}

// normalizeQuota coerces a raw quota into the stored form: a positive quota is
// kept, anything ≤0 or absent becomes nil (unlimited).
func normalizeQuota(v *int) *int {
	if v == nil || *v <= 0 {
		return nil
	}
	return v
}

func (s *CatalogService) CreateMaterial(actor Actor, in MaterialInput) (*models.Material, error) {
	in.Code = models.NormalizeCode(in.Code)
	if _, err := s.repo.Material.FindByCode(in.Code); err == nil {
		return nil, apperr.Conflict("A material with this code already exists")
	}
	m := &models.Material{Code: in.Code, Name: in.Name, Description: in.Description}
	// The production quota is an OWNER-only lever; ignore it for any other role so a
	// non-owner client can't set/clear it by sending the field.
	if actor.Role == models.RoleOwner {
		m.ProductsPerUnit = normalizeQuota(in.ProductsPerUnit)
	}
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

func (s *CatalogService) UpdateMaterial(actor Actor, id uint, in MaterialUpdateInput) (*models.Material, error) {
	m, err := s.GetMaterial(id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		m.Name = in.Name
	}
	m.Description = in.Description
	// Only OWNER may change the production quota. For other roles it is left as-is,
	// so an ADMIN/OPS name/description edit can't wipe a quota the owner set.
	if actor.Role == models.RoleOwner {
		m.ProductsPerUnit = normalizeQuota(in.ProductsPerUnit)
	}
	if err := s.repo.Material.Update(m); err != nil {
		return nil, apperr.Internal("could not update material").Wrap(err)
	}
	s.audit.Log(actor, "MATERIAL_UPDATE", "material", &m.ID, "Updated material "+m.Code, nil)
	return m, nil
}

// MaterialDeleteSkip is one material a delete left alone, and why.
type MaterialDeleteSkip struct {
	ID     uint   `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// MaterialDeleteResult reports what a (bulk) delete actually did.
type MaterialDeleteResult struct {
	DeletedIDs []uint               `json:"deleted_ids"`
	Skipped    []MaterialDeleteSkip `json:"skipped"`
}

// maxDeleteIDs bounds one bulk delete request. Well above any real selection, low
// enough that a runaway client can't ask for an unbounded statement.
const maxDeleteIDs = 5000

// DeleteMaterials removes many materials in one go: a handful of statements for
// the whole set instead of the three-per-material an id-at-a-time API costs.
// Materials still referenced by a SKU or a batch are skipped with a reason rather
// than deleted — those rows point at the material by id, and soft-deleting it
// would leave them pointing at something that no longer lists.
func (s *CatalogService) DeleteMaterials(actor Actor, ids []uint) (*MaterialDeleteResult, error) {
	clean := dedupeIDs(ids)
	if len(clean) == 0 {
		return nil, apperr.BadRequest("Chưa chọn nguyên vật liệu nào để xoá")
	}
	if len(clean) > maxDeleteIDs {
		return nil, apperr.BadRequest(fmt.Sprintf("Chỉ xoá tối đa %d nguyên vật liệu mỗi lần", maxDeleteIDs))
	}

	found, err := s.repo.Material.ListByIDs(clean)
	if err != nil {
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	byID := make(map[uint]*models.Material, len(found))
	for i := range found {
		byID[found[i].ID] = &found[i]
	}
	inUse, err := s.repo.Material.InUseIDs(clean)
	if err != nil {
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}

	res := &MaterialDeleteResult{DeletedIDs: []uint{}, Skipped: []MaterialDeleteSkip{}}
	deletable := make([]uint, 0, len(clean))
	for _, id := range clean {
		m := byID[id]
		if m == nil {
			res.Skipped = append(res.Skipped, MaterialDeleteSkip{ID: id, Reason: "không còn tồn tại"})
			continue
		}
		if reason, used := inUse[id]; used {
			res.Skipped = append(res.Skipped, MaterialDeleteSkip{
				ID: id, Code: m.Code, Name: m.Name, Reason: reason,
			})
			continue
		}
		deletable = append(deletable, id)
	}

	if len(deletable) > 0 {
		if _, err := s.repo.Material.DeleteMany(deletable); err != nil {
			return nil, apperr.Internal("could not delete materials").Wrap(err)
		}
		res.DeletedIDs = deletable
	}

	// One audit entry for the action, not one per row — a bulk cleanup should read
	// as a single event in the log.
	if len(res.DeletedIDs) == 1 {
		id := res.DeletedIDs[0]
		s.audit.Log(actor, "MATERIAL_DELETE", "material", &id, "Deleted material "+byID[id].Code, nil)
	} else if len(res.DeletedIDs) > 1 {
		s.audit.Log(actor, "MATERIAL_DELETE_BULK", "material", nil,
			fmt.Sprintf("Deleted %d materials (skipped %d)", len(res.DeletedIDs), len(res.Skipped)), nil)
	}
	return res, nil
}

func (s *CatalogService) DeleteMaterial(actor Actor, id uint) error {
	res, err := s.DeleteMaterials(actor, []uint{id})
	if err != nil {
		return err
	}
	if len(res.DeletedIDs) == 0 {
		skip := res.Skipped[0]
		if skip.Reason == "không còn tồn tại" {
			return apperr.NotFound("Material not found")
		}
		return apperr.Conflict("Không xoá được: nguyên vật liệu " + skip.Reason)
	}
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

// SKUDeleteSkip is one SKU a delete left alone, and why.
type SKUDeleteSkip struct {
	ID     uint   `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// SKUDeleteResult reports what a (bulk) SKU delete actually did.
type SKUDeleteResult struct {
	DeletedIDs []uint          `json:"deleted_ids"`
	Skipped    []SKUDeleteSkip `json:"skipped"`
}

// dedupeIDs drops zeros and repeats while keeping the caller's order, so a bulk
// response reads like the selection that produced it.
func dedupeIDs(ids []uint) []uint {
	seen := make(map[uint]bool, len(ids))
	out := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// DeleteSKUs removes many SKUs in one go — a handful of statements for the whole
// set instead of the three-per-SKU an id-at-a-time API costs. A SKU an order line
// points at is skipped with a reason: deleting it would leave the order pointing
// at a SKU that no longer lists.
func (s *CatalogService) DeleteSKUs(actor Actor, ids []uint) (*SKUDeleteResult, error) {
	clean := dedupeIDs(ids)
	if len(clean) == 0 {
		return nil, apperr.BadRequest("Chưa chọn SKU nào để xoá")
	}
	if len(clean) > maxDeleteIDs {
		return nil, apperr.BadRequest(fmt.Sprintf("Chỉ xoá tối đa %d SKU mỗi lần", maxDeleteIDs))
	}

	found, err := s.repo.SKU.ListByIDs(clean)
	if err != nil {
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	byID := make(map[uint]*models.SKU, len(found))
	for i := range found {
		byID[found[i].ID] = &found[i]
	}
	inUse, err := s.repo.SKU.InUseIDs(clean)
	if err != nil {
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}

	res := &SKUDeleteResult{DeletedIDs: []uint{}, Skipped: []SKUDeleteSkip{}}
	deletable := make([]uint, 0, len(clean))
	for _, id := range clean {
		sku := byID[id]
		if sku == nil {
			res.Skipped = append(res.Skipped, SKUDeleteSkip{ID: id, Reason: "không còn tồn tại"})
			continue
		}
		if reason, used := inUse[id]; used {
			res.Skipped = append(res.Skipped, SKUDeleteSkip{
				ID: id, Code: sku.Code, Name: sku.Name, Reason: reason,
			})
			continue
		}
		deletable = append(deletable, id)
	}

	if len(deletable) > 0 {
		if _, err := s.repo.SKU.DeleteMany(deletable); err != nil {
			return nil, apperr.Internal("could not delete SKUs").Wrap(err)
		}
		res.DeletedIDs = deletable
	}

	if len(res.DeletedIDs) == 1 {
		id := res.DeletedIDs[0]
		s.audit.Log(actor, "SKU_DELETE", "sku", &id, "Deleted SKU "+byID[id].Code, nil)
	} else if len(res.DeletedIDs) > 1 {
		s.audit.Log(actor, "SKU_DELETE_BULK", "sku", nil,
			fmt.Sprintf("Deleted %d SKUs (skipped %d)", len(res.DeletedIDs), len(res.Skipped)), nil)
	}
	return res, nil
}

func (s *CatalogService) DeleteSKU(actor Actor, id uint) error {
	res, err := s.DeleteSKUs(actor, []uint{id})
	if err != nil {
		return err
	}
	if len(res.DeletedIDs) == 0 {
		skip := res.Skipped[0]
		if skip.Reason == "không còn tồn tại" {
			return apperr.NotFound("SKU not found")
		}
		return apperr.Conflict("Không xoá được: SKU " + skip.Reason)
	}
	return nil
}

// SetSKUsActive turns many SKUs on/off in one statement per chunk. Hiding a
// catalogue's worth of SKUs used to be one PUT (and a full SKU save) per row.
func (s *CatalogService) SetSKUsActive(actor Actor, ids []uint, active bool) (int64, error) {
	clean := dedupeIDs(ids)
	if len(clean) == 0 {
		return 0, apperr.BadRequest("Chưa chọn SKU nào")
	}
	if len(clean) > maxDeleteIDs {
		return 0, apperr.BadRequest(fmt.Sprintf("Chỉ đổi tối đa %d SKU mỗi lần", maxDeleteIDs))
	}
	n, err := s.repo.SKU.SetActiveMany(clean, active)
	if err != nil {
		return 0, apperr.Internal("could not update SKUs").Wrap(err)
	}
	state := "hidden"
	if active {
		state = "active"
	}
	s.audit.Log(actor, "SKU_SET_ACTIVE_BULK", "sku", nil,
		fmt.Sprintf("Set %d SKUs %s", n, state), nil)
	return n, nil
}
