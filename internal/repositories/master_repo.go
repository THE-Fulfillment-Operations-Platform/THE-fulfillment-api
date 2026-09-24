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

// FindByIDs bulk-loads users for attributing a list of records to their actor.
// One query instead of a FindByID per row, and no Seller preload: callers that
// resolve authorship only need role + seller_id off the user itself.
func (r *UserRepository) FindByIDs(ids []uint) ([]models.User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []models.User
	err := r.db.Where("id IN ?", ids).Find(&rows).Error
	return rows, err
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

// FindDeletedByEmail looks up a SOFT-DELETED user by email. users.email is a
// plain unique index (no deleted_at predicate), so a deleted row still occupies
// its email: creating "the same person" again has to find that row and restore
// it rather than insert a second one and hit the constraint.
func (r *UserRepository) FindDeletedByEmail(email string) (*models.User, error) {
	var u models.User
	err := r.db.Unscoped().Where("email = ? AND deleted_at IS NOT NULL", email).First(&u).Error
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// RestoreWith un-deletes a user row and overwrites it with the details of the
// account being (re-)created. Keeping the row — and therefore the id — means
// every audit entry, batch and QC record that points at this person still
// resolves to them instead of dangling.
func (r *UserRepository) RestoreWith(u *models.User) error {
	return r.db.Unscoped().Model(&models.User{}).Where("id = ?", u.ID).
		Updates(map[string]any{
			"deleted_at":    nil,
			"password_hash": u.PasswordHash,
			"full_name":     u.FullName,
			"role":          u.Role,
			"seller_id":     u.SellerID,
			"is_active":     u.IsActive,
			"permissions":   u.Permissions,
		}).Error
}

// CountByRole counts the live (not soft-deleted) users holding a role. The
// user-delete guard uses it to refuse removing the last OWNER.
func (r *UserRepository) CountByRole(role models.Role) (int64, error) {
	var n int64
	err := r.db.Model(&models.User{}).Where("role = ?", role).Count(&n).Error
	return n, err
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

// Exists reports whether the seller id is real, without loading the seller or
// its stores. Callers that only need the guard (import, seller-scoped writes)
// used to run FindByID, whose Preload("Stores") costs a second query — one extra
// round-trip to a remote database for data nobody reads.
func (r *SellerRepository) Exists(id uint) (bool, error) {
	var n int64
	err := r.db.Model(&models.Seller{}).Where("id = ?", id).Count(&n).Error
	return n > 0, err
}

// SellerIdentity is the minimum an import needs to cross-check the "Seller ID"
// column against the seller it is importing for: the id and the code, nothing
// else. FindByID would drag the seller's stores along for a string comparison.
type SellerIdentity struct {
	ID   uint
	Code string
	Name string
}

// IdentityByID loads just id/code/name. found=false means no such seller.
func (r *SellerRepository) IdentityByID(id uint) (SellerIdentity, bool, error) {
	var out []SellerIdentity
	err := r.db.Model(&models.Seller{}).
		Select("id", "code", "name").
		Where("id = ?", id).
		Limit(1).
		Scan(&out).Error
	if err != nil || len(out) == 0 {
		return SellerIdentity{}, false, err
	}
	return out[0], true, nil
}

// Identities loads id/code/name of every seller: the directory a multi-seller
// import resolves its "Seller ID" column against.
func (r *SellerRepository) Identities() ([]SellerIdentity, error) {
	var out []SellerIdentity
	err := r.db.Model(&models.Seller{}).
		Select("id", "code", "name").
		Order("id asc").
		Scan(&out).Error
	return out, err
}

func (r *SellerRepository) FindByCode(code string) (*models.Seller, error) {
	code = models.NormalizeCode(code)
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
	code = models.NormalizeCode(code)
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

// ListByNameInsensitive returns EVERY material carrying the name, oldest first.
// Material names are not unique (only the code is), and the quota import can
// deliberately create several materials sharing a name when their định mức/mô tả
// differ — so anything that has to pick the right one must see them all rather
// than take FindByNameInsensitive's arbitrary first row.
func (r *MaterialRepository) ListByNameInsensitive(name string) ([]models.Material, error) {
	return r.ListByNamesInsensitive([]string{name})
}

// nameChunk caps how many names ride in one IN (...) list. Postgres allows far
// more parameters; this just keeps statements a sane size.
const nameChunk = 500

// ListByNamesInsensitive returns every material matching ANY of the given names
// (case- and space-insensitive), oldest first. This is what an importer uses to
// look up a whole spreadsheet in one round-trip instead of one query per row —
// against a hosted database that difference is minutes, not milliseconds.
func (r *MaterialRepository) ListByNamesInsensitive(names []string) ([]models.Material, error) {
	keys := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		k := strings.ToLower(strings.TrimSpace(n))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	var out []models.Material
	for start := 0; start < len(keys); start += nameChunk {
		end := start + nameChunk
		if end > len(keys) {
			end = len(keys)
		}
		var rows []models.Material
		if err := r.db.Where("LOWER(TRIM(name)) IN ?", keys[start:end]).
			Order("id asc").Find(&rows).Error; err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// idChunk caps how many ids ride in one IN (...) list.
const idChunk = 500

// ListByIDs returns the materials with these ids, in one query per chunk.
func (r *MaterialRepository) ListByIDs(ids []uint) ([]models.Material, error) {
	var out []models.Material
	for start := 0; start < len(ids); start += idChunk {
		end := start + idChunk
		if end > len(ids) {
			end = len(ids)
		}
		var rows []models.Material
		if err := r.db.Where("id IN ?", ids[start:end]).Order("id asc").Find(&rows).Error; err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// InUseIDs reports which of the given materials are still referenced — by a SKU's
// material list, by a batch, or by a batch line — mapped to a human reason. A
// referenced material must not be deleted: rows point at it by id, and a soft
// delete would leave a SKU or batch pointing at a material that no longer lists.
// One query per referencing table, not one per material.
func (r *MaterialRepository) InUseIDs(ids []uint) (map[uint]string, error) {
	inUse := map[uint]string{}
	sources := []struct {
		table  string
		reason string
	}{
		{"sku_materials", "đang được SKU sử dụng"},
		{"batches", "đang thuộc batch sản xuất"},
		{"batch_items", "đang thuộc batch sản xuất"},
	}
	for start := 0; start < len(ids); start += idChunk {
		end := start + idChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		for _, src := range sources {
			var used []uint
			if err := r.db.Table(src.table).
				Where("material_id IN ? AND deleted_at IS NULL", chunk).
				Distinct().Pluck("material_id", &used).Error; err != nil {
				return nil, err
			}
			for _, id := range used {
				if _, seen := inUse[id]; !seen {
					inUse[id] = src.reason
				}
			}
		}
	}
	return inUse, nil
}

// DeleteMany soft-deletes materials by id — one statement per chunk instead of
// one request per material.
func (r *MaterialRepository) DeleteMany(ids []uint) (int64, error) {
	var affected int64
	for start := 0; start < len(ids); start += idChunk {
		end := start + idChunk
		if end > len(ids) {
			end = len(ids)
		}
		res := r.db.Where("id IN ?", ids[start:end]).Delete(&models.Material{})
		if res.Error != nil {
			return affected, res.Error
		}
		affected += res.RowsAffected
	}
	return affected, nil
}

// AllCodes returns every material code, so an importer can mint unique codes in
// memory instead of probing the database once per new material.
func (r *MaterialRepository) AllCodes() ([]string, error) {
	var codes []string
	err := r.db.Model(&models.Material{}).Pluck("code", &codes).Error
	return codes, err
}

// CreateMany inserts materials in batches — one statement per batch instead of
// one per row, which is the whole cost of a few hundred new materials.
func (r *MaterialRepository) CreateMany(rows []models.Material, batchSize int) error {
	if len(rows) == 0 {
		return nil
	}
	return r.db.CreateInBatches(rows, batchSize).Error
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
		// Hard delete (Unscoped): sku_materials is a pure mapping table with a
		// unique index on (sku_id, material_id). A soft delete would leave the old
		// rows physically present, so re-inserting the same pair collides with the
		// unique constraint. We never need soft-delete history for a mapping row.
		if err := tx.Unscoped().Where("sku_id = ?", skuID).Delete(&models.SKUMaterial{}).Error; err != nil {
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

// ListByIDs returns the SKUs with these ids (no material preload — bulk actions
// only need code/name), one query per chunk.
func (r *SKURepository) ListByIDs(ids []uint) ([]models.SKU, error) {
	var out []models.SKU
	for start := 0; start < len(ids); start += idChunk {
		end := start + idChunk
		if end > len(ids) {
			end = len(ids)
		}
		var rows []models.SKU
		if err := r.db.Where("id IN ?", ids[start:end]).Order("id asc").Find(&rows).Error; err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// InUseIDs reports which of the given SKUs are referenced by an order line, with
// a reason. An order item points at its SKU by id; deleting the SKU underneath it
// would leave the order pointing at nothing. One query per chunk.
func (r *SKURepository) InUseIDs(ids []uint) (map[uint]string, error) {
	inUse := map[uint]string{}
	for start := 0; start < len(ids); start += idChunk {
		end := start + idChunk
		if end > len(ids) {
			end = len(ids)
		}
		var used []uint
		if err := r.db.Table("order_items").
			Where("sku_id IN ? AND deleted_at IS NULL", ids[start:end]).
			Distinct().Pluck("sku_id", &used).Error; err != nil {
			return nil, err
		}
		for _, id := range used {
			inUse[id] = "đang có đơn hàng dùng"
		}
	}
	return inUse, nil
}

// DeleteMany soft-deletes SKUs and hard-deletes their material mappings — the
// mapping table is pure join data owned by the SKU, and leaving rows behind would
// collide with its unique (sku_id, material_id) index if the SKU is recreated.
func (r *SKURepository) DeleteMany(ids []uint) (int64, error) {
	var affected int64
	for start := 0; start < len(ids); start += idChunk {
		end := start + idChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		if err := r.db.Unscoped().Where("sku_id IN ?", chunk).Delete(&models.SKUMaterial{}).Error; err != nil {
			return affected, err
		}
		res := r.db.Where("id IN ?", chunk).Delete(&models.SKU{})
		if res.Error != nil {
			return affected, res.Error
		}
		affected += res.RowsAffected
	}
	return affected, nil
}

// SetActiveMany flips is_active on many SKUs in one statement per chunk.
func (r *SKURepository) SetActiveMany(ids []uint, active bool) (int64, error) {
	var affected int64
	for start := 0; start < len(ids); start += idChunk {
		end := start + idChunk
		if end > len(ids) {
			end = len(ids)
		}
		res := r.db.Model(&models.SKU{}).Where("id IN ?", ids[start:end]).
			Update("is_active", active)
		if res.Error != nil {
			return affected, res.Error
		}
		affected += res.RowsAffected
	}
	return affected, nil
}

func (r *SKURepository) FindByID(id uint) (*models.SKU, error) {
	var s models.SKU
	if err := r.db.Preload("Materials.Material").First(&s, id).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SKURepository) FindByCode(code string) (*models.SKU, error) {
	code = models.NormalizeCode(code)
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

// SKUInfo is the minimal SKU fact set the import validator and review checks
// need: does the code exist, what's its id, how many materials are mapped, and
// the master-data product name (the single source of truth for what a line is
// called, now that order items no longer carry their own copy).
type SKUInfo struct {
	ID            uint
	MaterialCount int64
	ProductName   string
}

// InfoByCodes returns SKUInfo for every existing code in one query (LEFT JOIN
// on the mapping table), replacing a FindByCode + CountMaterials pair per row.
// Codes are expected pre-normalized. Missing codes are simply absent.
func (r *SKURepository) InfoByCodes(codes []string) (map[string]SKUInfo, error) {
	out := map[string]SKUInfo{}
	if len(codes) == 0 {
		return out, nil
	}
	type row struct {
		ID            uint
		Code          string
		MaterialCount int64
		ProductName   string
	}
	var rows []row
	err := r.db.Model(&models.SKU{}).
		Select("skus.id, skus.code, COUNT(sku_materials.id) AS material_count, "+
			"COALESCE(NULLIF(skus.product_name, ''), skus.name) AS product_name").
		Joins("LEFT JOIN sku_materials ON sku_materials.sku_id = skus.id AND sku_materials.deleted_at IS NULL").
		Where("skus.code IN ?", codes).
		Group("skus.id, skus.code, skus.product_name, skus.name").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.Code] = SKUInfo{ID: r.ID, MaterialCount: r.MaterialCount, ProductName: r.ProductName}
	}
	return out, nil
}

// MaterialCounts returns the mapped-material count per SKU id in one query,
// replacing a COUNT per item on the review screen.
func (r *SKURepository) MaterialCounts(skuIDs []uint) (map[uint]int64, error) {
	out := map[uint]int64{}
	if len(skuIDs) == 0 {
		return out, nil
	}
	type row struct {
		SKUID uint `gorm:"column:sku_id"`
		N     int64
	}
	var rows []row
	err := r.db.Model(&models.SKUMaterial{}).
		Select("sku_id, COUNT(*) AS n").
		Where("sku_id IN ?", skuIDs).
		Group("sku_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.SKUID] = r.N
	}
	return out, nil
}

// CountMaterials returns how many materials a SKU is mapped to. Used by the
// order-import validator to distinguish "SKU exists but has no material" from a
// fully set-up SKU.
// ListByCodes returns the SKUs with these codes, WITHOUT preloading materials —
// a bulk importer only needs id/code/product_name, and the preload is what turned
// one lookup into three queries per SKU. Codes are normalized like FindByCode.
func (r *SKURepository) ListByCodes(codes []string) ([]models.SKU, error) {
	keys := make([]string, 0, len(codes))
	seen := map[string]bool{}
	for _, c := range codes {
		k := models.NormalizeCode(c)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	var out []models.SKU
	for start := 0; start < len(keys); start += nameChunk {
		end := start + nameChunk
		if end > len(keys) {
			end = len(keys)
		}
		var rows []models.SKU
		if err := r.db.Where("code IN ?", keys[start:end]).Order("id asc").Find(&rows).Error; err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// MappingsForSKUs returns every (sku_id, material_id) mapping of the given SKUs in
// one query per chunk — the set an importer needs to know which mappings are new.
func (r *SKURepository) MappingsForSKUs(skuIDs []uint) ([]models.SKUMaterial, error) {
	var out []models.SKUMaterial
	for start := 0; start < len(skuIDs); start += idChunk {
		end := start + idChunk
		if end > len(skuIDs) {
			end = len(skuIDs)
		}
		var rows []models.SKUMaterial
		if err := r.db.Where("sku_id IN ?", skuIDs[start:end]).Find(&rows).Error; err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// CreateMany inserts SKUs in batches — one statement per batch instead of one per
// SKU. IDs are filled in on the passed slice.
func (r *SKURepository) CreateMany(rows []models.SKU, batchSize int) error {
	if len(rows) == 0 {
		return nil
	}
	return r.db.CreateInBatches(rows, batchSize).Error
}

// AddMaterialsMany inserts SKU→material mappings in batches. Additive like
// AddMaterial: it never removes anything.
func (r *SKURepository) AddMaterialsMany(rows []models.SKUMaterial, batchSize int) error {
	if len(rows) == 0 {
		return nil
	}
	return r.db.CreateInBatches(rows, batchSize).Error
}

// MarkComboMany flags SKUs as combo in one statement per chunk. Upgrade-only: it
// never clears a combo flag someone set by hand.
func (r *SKURepository) MarkComboMany(ids []uint) error {
	for start := 0; start < len(ids); start += idChunk {
		end := start + idChunk
		if end > len(ids) {
			end = len(ids)
		}
		if err := r.db.Model(&models.SKU{}).
			Where("id IN ? AND is_combo = ?", ids[start:end], false).
			Update("is_combo", true).Error; err != nil {
			return err
		}
	}
	return nil
}

// ChildrenOf returns the live child SKU ids of each given parent, one query per
// chunk. Parents without children are simply absent from the map.
func (r *SKURepository) ChildrenOf(parentIDs []uint) (map[uint][]uint, error) {
	out := map[uint][]uint{}
	for start := 0; start < len(parentIDs); start += idChunk {
		end := start + idChunk
		if end > len(parentIDs) {
			end = len(parentIDs)
		}
		var rows []models.SKU
		if err := r.db.Select("id", "parent_id").
			Where("parent_id IN ?", parentIDs[start:end]).
			Order("id asc").Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, s := range rows {
			if s.ParentID != nil {
				out[*s.ParentID] = append(out[*s.ParentID], s.ID)
			}
		}
	}
	return out, nil
}

// SKUPatch is a partial update of one SKU's descriptive columns. A nil field is
// left untouched; ParentID pointing at 0 clears the parent.
type SKUPatch struct {
	ID          uint
	ParentID    *uint
	LengthMM    *float64
	WidthMM     *float64
	ProductName *string
	Description *string
}

// PatchMany applies many SKUPatches with ONE UPDATE per chunk — each column is
// rewritten as `CASE id WHEN ? THEN ? … ELSE col END`, so SKUs that each carry a
// different value still share a statement. Per-row UPDATEs made re-importing a
// catalogue of a few hundred SKUs cost a few hundred round-trips.
func (r *SKURepository) PatchMany(patches []SKUPatch) error {
	for start := 0; start < len(patches); start += idChunk {
		end := start + idChunk
		if end > len(patches) {
			end = len(patches)
		}
		chunk := patches[start:end]
		ids := make([]uint, 0, len(chunk))
		cols := map[string][]any{} // column → id, value, id, value…
		for _, p := range chunk {
			ids = append(ids, p.ID)
			if p.ParentID != nil {
				var v any // 0 → NULL: detach from the parent
				if *p.ParentID != 0 {
					v = *p.ParentID
				}
				cols["parent_id"] = append(cols["parent_id"], p.ID, v)
			}
			if p.LengthMM != nil {
				cols["length_mm"] = append(cols["length_mm"], p.ID, *p.LengthMM)
			}
			if p.WidthMM != nil {
				cols["width_mm"] = append(cols["width_mm"], p.ID, *p.WidthMM)
			}
			if p.ProductName != nil {
				cols["product_name"] = append(cols["product_name"], p.ID, *p.ProductName)
			}
			if p.Description != nil {
				cols["description"] = append(cols["description"], p.ID, *p.Description)
			}
		}
		if len(cols) == 0 {
			continue
		}
		set := make(map[string]any, len(cols))
		for col, args := range cols {
			sql := "CASE id" + strings.Repeat(" WHEN ? THEN ?", len(args)/2) + " ELSE " + col + " END"
			set[col] = gorm.Expr(sql, args...)
		}
		if err := r.db.Model(&models.SKU{}).Where("id IN ?", ids).Updates(set).Error; err != nil {
			return err
		}
	}
	return nil
}

// PairQuota is a declared production quota for one (SKU, material) pair.
type PairQuota struct {
	SKUID      uint
	MaterialID uint
	Quota      int
}

// SetPairQuotas writes declared quotas onto existing sku_materials rows, ONE
// UPDATE per chunk (a CASE over the pairs) — an import of a few hundred pairs
// must not cost a few hundred round-trips. Pairs that are not mapped are
// silently left alone; the importer only sends mapped pairs.
func (r *SKURepository) SetPairQuotas(pairs []PairQuota) error {
	for start := 0; start < len(pairs); start += idChunk {
		end := start + idChunk
		if end > len(pairs) {
			end = len(pairs)
		}
		chunk := pairs[start:end]
		var cases strings.Builder
		var where strings.Builder
		args := make([]any, 0, len(chunk)*5)
		whereArgs := make([]any, 0, len(chunk)*2)
		for i, pq := range chunk {
			cases.WriteString(" WHEN sku_id = ? AND material_id = ? THEN ?")
			args = append(args, pq.SKUID, pq.MaterialID, pq.Quota)
			if i > 0 {
				where.WriteString(" OR ")
			}
			where.WriteString("(sku_id = ? AND material_id = ?)")
			whereArgs = append(whereArgs, pq.SKUID, pq.MaterialID)
		}
		sql := "UPDATE sku_materials SET products_per_unit = CASE" + cases.String() + " ELSE products_per_unit END WHERE " + where.String()
		if err := r.db.Exec(sql, append(args, whereArgs...)...).Error; err != nil {
			return err
		}
	}
	return nil
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
