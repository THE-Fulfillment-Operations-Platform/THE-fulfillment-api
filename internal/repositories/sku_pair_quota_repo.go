package repositories

import "the-fulfillment/backend/internal/models"

// MappingsWithMaterial returns every (SKU, material) mapping of the given SKUs
// with the material loaded — what the pair-quota import matches a file's
// "Loại VL" against, and shows the material's own default quota from.
func (r *SKURepository) MappingsWithMaterial(skuIDs []uint) ([]models.SKUMaterial, error) {
	var out []models.SKUMaterial
	for start := 0; start < len(skuIDs); start += idChunk {
		end := start + idChunk
		if end > len(skuIDs) {
			end = len(skuIDs)
		}
		var rows []models.SKUMaterial
		if err := r.db.Preload("Material").Where("sku_id IN ?", skuIDs[start:end]).
			Order("sku_id asc, id asc").Find(&rows).Error; err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// PairQuotaRow is one (SKU, material) mapping as the pair-quota export lists it.
type PairQuotaRow struct {
	SKUCode       string
	ProductName   string
	MaterialCode  string
	MaterialName  string
	PairQuota     *int // the pair's own quota; nil = falls back to MaterialQuota
	MaterialQuota *int // the material's default quota
}

// AllPairQuotas lists every mapping of every active SKU, SKU code order — the
// sheet an owner fills the missing quotas into.
func (r *SKURepository) AllPairQuotas() ([]PairQuotaRow, error) {
	var out []PairQuotaRow
	err := r.db.Table("sku_materials AS sm").
		Select(`s.code AS sku_code, s.product_name AS product_name,
			m.code AS material_code, m.name AS material_name,
			sm.products_per_unit AS pair_quota, m.products_per_unit AS material_quota`).
		Joins("JOIN skus s ON s.id = sm.sku_id AND s.deleted_at IS NULL").
		Joins("JOIN materials m ON m.id = sm.material_id AND m.deleted_at IS NULL").
		Where("sm.deleted_at IS NULL AND s.is_active = ?", true).
		Order("s.code asc, m.name asc, m.id asc").
		Scan(&out).Error
	return out, err
}

// SetPairQuotas writes products_per_unit onto mappings, one UPDATE per distinct
// quota value (an import sets a handful of values across hundreds of pairs).
func (r *SKURepository) SetPairQuotas(idsByQuota map[int][]uint) error {
	for quota, ids := range idsByQuota {
		for start := 0; start < len(ids); start += idChunk {
			end := start + idChunk
			if end > len(ids) {
				end = len(ids)
			}
			if err := r.db.Model(&models.SKUMaterial{}).Where("id IN ?", ids[start:end]).
				Update("products_per_unit", quota).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
