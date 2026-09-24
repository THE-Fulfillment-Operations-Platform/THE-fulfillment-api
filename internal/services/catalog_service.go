package services

import (
	"errors"
	"fmt"
	"math"

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
	// LengthMM × WidthMM: the size of one sheet, in millimetres — what the
	// production quota of every SKU on this material is derived from. Both or
	// neither; omitted/≤0 = not declared.
	LengthMM *float64 `json:"length_mm"`
	WidthMM  *float64 `json:"width_mm"`
}

// MaterialUpdateInput is a partial-update payload for a material. Unlike
// MaterialInput, code/name are NOT required: the Materials screen edits name /
// description / size and never resends the (immutable) code — mirroring the
// SKUUpdateInput convention so a code-less update isn't rejected by validation.
// Pointer fields: omitted = leave as is; a size sent as ≤0 clears the pair.
type MaterialUpdateInput struct {
	Name        string   `json:"name"`
	Description *string  `json:"description"`
	LengthMM    *float64 `json:"length_mm"`
	WidthMM     *float64 `json:"width_mm"`
}

// maxDimMM caps a declared D/R at 100 m — far past any product or sheet, low
// enough to catch a unit slip (a size typed in µm, or a price pasted into the
// column).
const maxDimMM = 100000

// normalizeSize validates a D × R pair as stored: each side ≤0/absent means
// "not declared", otherwise it is rounded to 0.01 mm and capped. The two sides
// go together — one without the other is refused, because a half size can't
// yield an area and would silently mean "no quota" while looking filled in.
func normalizeSize(length, width *float64) (*float64, *float64, error) {
	l, err := normalizeDimMM(length)
	if err != nil {
		return nil, nil, err
	}
	w, err := normalizeDimMM(width)
	if err != nil {
		return nil, nil, err
	}
	if (l == nil) != (w == nil) {
		return nil, nil, apperr.BadRequest("Nhập cả D lẫn R (mm), hoặc bỏ trống cả hai")
	}
	return l, w, nil
}

// normalizeDimMM turns one D/R input into what is stored: ≤0 → nil (not
// declared), otherwise the value rounded to 0.01 mm.
func normalizeDimMM(v *float64) (*float64, error) {
	if v == nil || *v <= 0 {
		return nil, nil
	}
	if *v > maxDimMM || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil, apperr.BadRequest(fmt.Sprintf("Kích thước phải nhỏ hơn %d mm", maxDimMM))
	}
	r := math.Round(*v*100) / 100
	return &r, nil
}

func (s *CatalogService) CreateMaterial(actor Actor, in MaterialInput) (*models.Material, error) {
	in.Code = models.NormalizeCode(in.Code)
	if _, err := s.repo.Material.FindByCode(in.Code); err == nil {
		return nil, apperr.Conflict("A material with this code already exists")
	}
	length, width, err := normalizeSize(in.LengthMM, in.WidthMM)
	if err != nil {
		return nil, err
	}
	m := &models.Material{Code: in.Code, Name: in.Name, Description: in.Description, LengthMM: length, WidthMM: width}
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
	if in.Description != nil {
		m.Description = *in.Description
	}
	// A side not sent keeps its stored value; the pair is validated as a whole so
	// a partial payload can't leave the sheet half-sized.
	length, width := m.LengthMM, m.WidthMM
	if in.LengthMM != nil {
		length = in.LengthMM
	}
	if in.WidthMM != nil {
		width = in.WidthMM
	}
	if m.LengthMM, m.WidthMM, err = normalizeSize(length, width); err != nil {
		return nil, err
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

// SKUMaterialInput links a SKU to a material with a per-unit quantity and,
// optionally, the pair's declared production quota.
type SKUMaterialInput struct {
	MaterialID uint `json:"material_id" binding:"required"`
	// QuantityPerUnit: how much of the material ONE product consumes.
	QuantityPerUnit int `json:"quantity_per_unit"`
	// ProductsPerUnit: how many products of this SKU one sheet of the material
	// yields, as the factory declares it. Omitted / ≤0 = not declared → the
	// batch splitter estimates from the sizes (models.ProductionQuotaSource).
	ProductsPerUnit *int   `json:"products_per_unit"`
	Note            string `json:"note"`
}

type SKUInput struct {
	Code        string `json:"code" binding:"required"`
	Name        string `json:"name" binding:"required"`
	ProductName string `json:"product_name"`
	Description string `json:"description"`
	IsActive    *bool  `json:"is_active"`
	// ParentID files the new SKU under a parent SKU (0/omitted = top level).
	ParentID *uint `json:"parent_id"`
	// LengthMM / WidthMM: the product's "D x R" in millimetres — with the
	// material's sheet size, what its production quota is derived from. Both or
	// neither; omitted/≤0 = not declared.
	LengthMM *float64 `json:"length_mm"`
	WidthMM  *float64 `json:"width_mm"`
	// Materials is optional: a SKU may be created "unmapped" and have its material(s)
	// assigned later from the Master Data → Mapping screen. On update, an empty/omitted
	// list leaves the existing material set unchanged (see UpdateSKU).
	Materials []SKUMaterialInput `json:"materials" binding:"omitempty,dive"`
}

// SKUUpdateInput is a partial-update payload for an existing SKU. Unlike SKUInput,
// code/name are NOT required: the mapping screen only submits `materials`, and
// UpdateSKU ignores code and treats name as optional. Requiring them here would
// reject a materials-only update with a spurious validation error.
//
// Every other field is a pointer for the same reason: omitted = leave as is. The
// mapping screen (materials only) and the hide/show toggle (is_active only) used
// to blank the product name and description on every save. ParentID, LengthMM and
// WidthMM: omitted keeps the stored value, ≤0 clears.
type SKUUpdateInput struct {
	Name        string             `json:"name"`
	ProductName *string            `json:"product_name"`
	Description *string            `json:"description"`
	IsActive    *bool              `json:"is_active"`
	ParentID    *uint              `json:"parent_id"`
	LengthMM    *float64           `json:"length_mm"`
	WidthMM     *float64           `json:"width_mm"`
	Materials   []SKUMaterialInput `json:"materials" binding:"omitempty,dive"`
}

// checkSKUParent enforces the two-level parent → child tree for SKU `selfID`
// (0 when the SKU is being created) getting `parentID` as its parent: the parent
// must exist, must itself be top level, and a SKU that already has children
// can't become somebody's child.
func (s *CatalogService) checkSKUParent(selfID, parentID uint) error {
	if selfID != 0 && selfID == parentID {
		return apperr.BadRequest("SKU không thể là SKU cha của chính nó")
	}
	parent, err := s.repo.SKU.FindByID(parentID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperr.BadRequest("SKU cha không tồn tại")
		}
		return apperr.Internal("lookup failed").Wrap(err)
	}
	if parent.ParentID != nil {
		return apperr.BadRequest("SKU " + parent.Code + " đang là SKU con — chỉ hỗ trợ 2 tầng cha → con")
	}
	if selfID != 0 {
		children, err := s.repo.SKU.ChildrenOf([]uint{selfID})
		if err != nil {
			return apperr.Internal("lookup failed").Wrap(err)
		}
		if n := len(children[selfID]); n > 0 {
			return apperr.BadRequest(fmt.Sprintf("SKU này đang là SKU cha của %d SKU con — không gán SKU cha cho nó được", n))
		}
	}
	return nil
}

// buildMaterials validates a SKU's material set (replaced wholesale on update).
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
		quota := m.ProductsPerUnit
		if quota != nil && *quota <= 0 {
			quota = nil
		}
		out = append(out, models.SKUMaterial{MaterialID: m.MaterialID, QuantityPerUnit: qty, ProductsPerUnit: quota, Note: m.Note})
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
	var parentID *uint
	if in.ParentID != nil && *in.ParentID != 0 {
		if err := s.checkSKUParent(0, *in.ParentID); err != nil {
			return nil, err
		}
		parentID = in.ParentID
	}
	length, width, err := normalizeSize(in.LengthMM, in.WidthMM)
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
		ParentID:    parentID,
		LengthMM:    length,
		WidthMM:     width,
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
	if in.ProductName != nil {
		sku.ProductName = *in.ProductName
	}
	if in.Description != nil {
		sku.Description = *in.Description
	}
	if in.IsActive != nil {
		sku.IsActive = *in.IsActive
	}
	if in.ParentID != nil {
		if *in.ParentID == 0 {
			sku.ParentID = nil
		} else if sku.ParentID == nil || *sku.ParentID != *in.ParentID {
			if err := s.checkSKUParent(sku.ID, *in.ParentID); err != nil {
				return nil, err
			}
			sku.ParentID = in.ParentID
		}
	}
	// A side not sent keeps its stored value; the pair is validated as a whole.
	length, width := sku.LengthMM, sku.WidthMM
	if in.LengthMM != nil {
		length = in.LengthMM
	}
	if in.WidthMM != nil {
		width = in.WidthMM
	}
	if sku.LengthMM, sku.WidthMM, err = normalizeSize(length, width); err != nil {
		return nil, err
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

	// A parent goes only together with every one of its children: deleting it
	// alone would leave SKUs pointing at a parent nobody can see. Children never
	// have children of their own, so one pass settles it.
	if len(deletable) > 0 {
		children, err := s.repo.SKU.ChildrenOf(deletable)
		if err != nil {
			return nil, apperr.Internal("lookup failed").Wrap(err)
		}
		if len(children) > 0 {
			going := make(map[uint]bool, len(deletable))
			for _, id := range deletable {
				going[id] = true
			}
			kept := make([]uint, 0, len(deletable))
			for _, id := range deletable {
				left := 0
				for _, c := range children[id] {
					if !going[c] {
						left++
					}
				}
				if left > 0 {
					sku := byID[id]
					res.Skipped = append(res.Skipped, SKUDeleteSkip{
						ID: id, Code: sku.Code, Name: sku.Name,
						Reason: fmt.Sprintf("còn %d SKU con — xoá hoặc chuyển SKU con trước", left),
					})
					continue
				}
				kept = append(kept, id)
			}
			deletable = kept
		}
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
