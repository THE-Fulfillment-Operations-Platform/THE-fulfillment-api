package services

import (
	"sort"
	"strings"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Loại hàng của SKU → dịch vụ THE (#7783). The rules themselves are in
// models/carrier_rules.go; this file reads what they need from Master Data.

// skuGoods is one SKU's kind under the rules in force.
type skuGoods struct {
	Group     int      // index in rules.Groups, -1 = no kind
	Materials []string // the SKU's material names (for messages)
}

// goodsOfSKUs classifies each SKU code (normalized) under rules: its own
// materials, else its parent's; its own HS code, else its parent's. Codes
// unknown to Master Data are absent from the map. Four statements whatever
// the number of codes.
func goodsOfSKUs(repo *repositories.Repositories, rules models.THEServiceRules, codes []string) (map[string]skuGoods, error) {
	out := map[string]skuGoods{}
	skus, err := repo.SKU.ListByCodes(codes)
	if err != nil || len(skus) == 0 {
		return out, err
	}
	var parentIDs []uint
	for i := range skus {
		if skus[i].ParentID != nil {
			parentIDs = append(parentIDs, *skus[i].ParentID)
		}
	}
	parents := map[uint]*models.SKU{}
	if len(parentIDs) > 0 {
		rows, err := repo.SKU.ListByIDs(dedupeIDs(parentIDs))
		if err != nil {
			return nil, err
		}
		for i := range rows {
			parents[rows[i].ID] = &rows[i]
		}
	}
	mats, err := materialsOfSKUs(repo, skus, parents)
	if err != nil {
		return nil, err
	}
	for i := range skus {
		sk := &skus[i]
		var parent *models.SKU
		if sk.ParentID != nil {
			parent = parents[*sk.ParentID]
		}
		out[models.NormalizeCode(sk.Code)] = classifySKU(rules, sk, parent, mats)
	}
	return out, nil
}

// materialsOfSKUs maps SKU id → its materials, for the SKUs and their parents.
func materialsOfSKUs(repo *repositories.Repositories, skus []models.SKU, parents map[uint]*models.SKU) (map[uint][]models.GoodsMaterial, error) {
	ids := make([]uint, 0, len(skus)+len(parents))
	for i := range skus {
		ids = append(ids, skus[i].ID)
	}
	for id := range parents {
		ids = append(ids, id)
	}
	maps, err := repo.SKU.MappingsForSKUs(dedupeIDs(ids))
	if err != nil {
		return nil, err
	}
	var matIDs []uint
	for _, m := range maps {
		matIDs = append(matIDs, m.MaterialID)
	}
	byID := map[uint]models.Material{}
	if len(matIDs) > 0 {
		rows, err := repo.Material.ListByIDs(dedupeIDs(matIDs))
		if err != nil {
			return nil, err
		}
		for _, m := range rows {
			byID[m.ID] = m
		}
	}
	out := map[uint][]models.GoodsMaterial{}
	for _, m := range maps {
		if mat, ok := byID[m.MaterialID]; ok {
			out[m.SKUID] = append(out[m.SKUID], models.GoodsMaterial{Code: mat.Code, Name: mat.Name})
		}
	}
	return out, nil
}

func classifySKU(rules models.THEServiceRules, sk, parent *models.SKU, mats map[uint][]models.GoodsMaterial) skuGoods {
	own := mats[sk.ID]
	if len(own) == 0 && parent != nil {
		own = mats[parent.ID]
	}
	hs := strings.TrimSpace(sk.HSCode)
	if hs == "" && parent != nil {
		hs = strings.TrimSpace(parent.HSCode)
	}
	g := skuGoods{Group: rules.Classify(own, hs)}
	for _, m := range own {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = m.Code
		}
		g.Materials = append(g.Materials, name)
	}
	return g
}

// pickService chooses the THE service of an order's live lines (codes in
// line order, as currentSKUCodes gives them). problem != "" = refuse.
func (s *CarrierService) pickService(c *models.CarrierConfig, codes []string) (models.THEServicePick, string, error) {
	rules, _ := c.Rules()
	if !rules.Auto {
		p, problem := rules.Pick(c.ServiceCode, nil)
		return p, problem, nil
	}
	goods, err := goodsOfSKUs(s.repo, rules, codes)
	if err != nil {
		return models.THEServicePick{}, "", err
	}
	lines := make([]models.THEGoodsLine, 0, len(codes))
	for _, raw := range codes {
		code := models.NormalizeCode(raw)
		g, ok := goods[code]
		if !ok {
			// Not in Master Data: shippingSpecs refuses the order for that already.
			g = skuGoods{Group: -1}
		}
		lines = append(lines, models.THEGoodsLine{SKUCode: code, Group: g.Group, Materials: g.Materials})
	}
	p, problem := rules.Pick(c.ServiceCode, lines)
	return p, problem, nil
}

// ---------- preview (Settings → Kết nối THE) ----------

// THERulesPreview shows, before saving, which kind and service every material
// and SKU of Master Data falls in under a table.
type THERulesPreview struct {
	Groups       []THEGroupPreview    `json:"groups"`
	Unclassified THEGroupPreview      `json:"unclassified"`
	Materials    []THEMaterialPreview `json:"materials"`
	// SKUs counts the orderable SKUs (every live SKU but the parents of others).
	SKUs int `json:"skus"`
}

// THEGroupPreview is one kind (or the unclassified bucket) with its SKU count.
type THEGroupPreview struct {
	Name        string `json:"name"`
	ServiceCode string `json:"service_code"`
	// Blocked: unclassified goods are refused (policy "block").
	Blocked bool `json:"blocked,omitempty"`
	SKUs    int  `json:"skus"`
	// ByHS counts the SKUs of this kind recognised by their HS code only.
	ByHS int `json:"by_hs"`
	// Examples are a few SKU codes (unclassified bucket only).
	Examples []string `json:"examples,omitempty"`
}

// THEMaterialPreview is one material and the kind it falls in.
type THEMaterialPreview struct {
	Code  string `json:"code"`
	Name  string `json:"name"`
	SKUs  int    `json:"skus"`
	Group string `json:"group"` // "" = no kind
	// ServiceCode the SKUs of this material ship with (when alone in a parcel).
	ServiceCode string `json:"service_code"`
}

// PreviewServiceRules classifies all of Master Data under rules (nil = the
// table in force) and defaultService ("" = the connection's own). Read-only.
func (s *CarrierService) PreviewServiceRules(actor Actor, rules *models.THEServiceRules, defaultService string) (*THERulesPreview, error) {
	if !canConfigureCarrier(actor) {
		return nil, apperr.Forbidden("Chỉ Kỹ thuật viên xem được cấu hình kết nối THE")
	}
	c, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	r, _ := c.Rules()
	if rules != nil {
		if r, err = rules.Clean(); err != nil {
			return nil, apperr.BadRequest(err.Error())
		}
		r.Auto = true
	}
	if strings.TrimSpace(defaultService) == "" {
		defaultService = c.ServiceCode
	}
	defaultService = strings.TrimSpace(defaultService)

	skus, err := s.repo.SKU.AllForShipping()
	if err != nil {
		return nil, apperr.Internal("could not read SKUs").Wrap(err)
	}
	byID := map[uint]*models.SKU{}
	isParent := map[uint]bool{}
	for i := range skus {
		byID[skus[i].ID] = &skus[i]
		if skus[i].ParentID != nil {
			isParent[*skus[i].ParentID] = true
		}
	}
	maps, err := s.repo.SKU.MappingsForSKUs(keysOf(byID))
	if err != nil {
		return nil, apperr.Internal("could not read SKU materials").Wrap(err)
	}
	materials, err := s.repo.Material.All()
	if err != nil {
		return nil, apperr.Internal("could not read materials").Wrap(err)
	}
	matByID := map[uint]models.Material{}
	for _, m := range materials {
		matByID[m.ID] = m
	}
	mats := map[uint][]models.GoodsMaterial{}
	skuCount := map[uint]int{} // material id → orderable SKUs using it
	for _, m := range maps {
		mat, ok := matByID[m.MaterialID]
		if !ok {
			continue
		}
		mats[m.SKUID] = append(mats[m.SKUID], models.GoodsMaterial{Code: mat.Code, Name: mat.Name})
		if sk := byID[m.SKUID]; sk != nil && !isParent[sk.ID] {
			skuCount[m.MaterialID]++
		}
	}

	serviceOf := func(i int) string {
		if i < 0 || r.Groups[i].ServiceCode == "" {
			return defaultService
		}
		return r.Groups[i].ServiceCode
	}
	out := &THERulesPreview{Groups: []THEGroupPreview{}, Materials: []THEMaterialPreview{}}
	for i, g := range r.Groups {
		out.Groups = append(out.Groups, THEGroupPreview{Name: g.Name, ServiceCode: serviceOf(i)})
	}
	out.Unclassified = THEGroupPreview{Name: "Chưa xếp loại", ServiceCode: defaultService,
		Blocked: r.Unclassified == models.THEUnclassifiedBlock}
	if out.Unclassified.Blocked {
		out.Unclassified.ServiceCode = ""
	}
	for i := range skus {
		sk := &skus[i]
		if isParent[sk.ID] {
			continue
		}
		out.SKUs++
		var parent *models.SKU
		if sk.ParentID != nil {
			parent = byID[*sk.ParentID]
		}
		g := classifySKU(r, sk, parent, mats)
		if g.Group < 0 {
			out.Unclassified.SKUs++
			if len(out.Unclassified.Examples) < 12 {
				out.Unclassified.Examples = append(out.Unclassified.Examples, sk.Code)
			}
			continue
		}
		out.Groups[g.Group].SKUs++
		if len(g.Materials) == 0 || r.Classify(ownOrParent(mats, sk, parent), "") < 0 {
			out.Groups[g.Group].ByHS++
		}
	}
	for _, m := range materials {
		gi := r.Classify([]models.GoodsMaterial{{Code: m.Code, Name: m.Name}}, "")
		row := THEMaterialPreview{Code: m.Code, Name: m.Name, SKUs: skuCount[m.ID], ServiceCode: serviceOf(gi)}
		if gi >= 0 {
			row.Group = r.Groups[gi].Name
		} else if out.Unclassified.Blocked {
			row.ServiceCode = ""
		}
		out.Materials = append(out.Materials, row)
	}
	sort.SliceStable(out.Materials, func(i, j int) bool {
		if out.Materials[i].SKUs != out.Materials[j].SKUs {
			return out.Materials[i].SKUs > out.Materials[j].SKUs
		}
		return out.Materials[i].Code < out.Materials[j].Code
	})
	return out, nil
}

func ownOrParent(mats map[uint][]models.GoodsMaterial, sk, parent *models.SKU) []models.GoodsMaterial {
	if own := mats[sk.ID]; len(own) > 0 || parent == nil {
		return own
	}
	return mats[parent.ID]
}

func keysOf(m map[uint]*models.SKU) []uint {
	out := make([]uint, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
