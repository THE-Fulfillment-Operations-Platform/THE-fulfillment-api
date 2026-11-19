package services

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Cửa xác nhận khi đổi NVL của SKU (#6380).
//
// 19/11/2026 17:11: các SKU WD-PBH-5L-8IN/10IN/12IN đang map Basswood 3 ly bị
// THÊM Basswood 5 ly (3 ly vẫn còn). Hệ thống coi đó là combo 2 tấm, nên 23 sản
// phẩm đã cắt trên 3 ly hiện lại ở màn Tạo batch của 5 ly (livePartsCoverSKUSQL:
// 1 phần sống < 2 NVL) và bị gom thêm 8 batch trùng. Cùng họ lỗi với vụ 17/11.
//
// Không chặn hẳn — combo nhiều tấm và đổi tấm là việc thật — nhưng đổi bộ NVL của
// một SKU đang có hàng trong xưởng, thêm hai NVL cùng loại khác độ dày, hay import
// (chỉ THÊM, không thay) một NVL khác với NVL SKU đang có, đều phải qua một bước
// xác nhận có số liệu: API trả 409 SKU_MATERIAL_CHANGE_CONFIRM kèm chi tiết, giao
// diện hiện hộp xác nhận rồi gửi lại với confirm_material_change = true.

// CodeSKUMaterialChangeConfirm là mã lỗi 409 của cửa xác nhận.
const CodeSKUMaterialChangeConfirm = "SKU_MATERIAL_CHANGE_CONFIRM"

// skuMaterialChangeHint là câu gợi ý đi kèm mọi cảnh báo.
const skuMaterialChangeHint = "Đổi tấm thì xoá NVL cũ, đừng để 2 tấm nếu không phải combo."

// maxImpactBatchCodes giới hạn số mã batch kể ra cho mỗi NVL.
const maxImpactBatchCodes = 8

// MaterialRef nêu tên một NVL trong cảnh báo. ID = 0 là NVL file import sắp tạo.
type MaterialRef struct {
	ID   uint   `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

func (m MaterialRef) key() string {
	if m.ID != 0 {
		return fmt.Sprintf("#%d", m.ID)
	}
	return "new:" + strings.ToLower(strings.TrimSpace(m.Name))
}

func (m MaterialRef) label() string {
	if m.Name != "" {
		return m.Name
	}
	return m.Code
}

func materialRefOf(m *models.Material) MaterialRef {
	if m == nil {
		return MaterialRef{}
	}
	return MaterialRef{ID: m.ID, Code: m.Code, Name: m.Name}
}

// SameFamilyPair là hai NVL cùng loại, khác độ dày (vd Basswood 3 ly + 5 ly).
type SameFamilyPair struct {
	A MaterialRef `json:"a"`
	B MaterialRef `json:"b"`
}

// InProductionMaterial: sản phẩm của SKU đang trong xưởng có phần sống trên một NVL.
type InProductionMaterial struct {
	Material MaterialRef `json:"material"`
	Items    int         `json:"items"`
	// Batches: mã batch (tăng dần), tối đa maxImpactBatchCodes; MoreBatches = số còn lại.
	Batches     []string `json:"batches"`
	MoreBatches int      `json:"more_batches"`
}

// SKUMaterialImpact là một SKU cần xác nhận trước khi đổi bộ NVL.
type SKUMaterialImpact struct {
	SKUID   uint          `json:"sku_id"`
	SKUCode string        `json:"sku_code"`
	Before  []MaterialRef `json:"before"`
	After   []MaterialRef `json:"after"`
	Added   []MaterialRef `json:"added"`
	Removed []MaterialRef `json:"removed"`
	// KeptNotInFile (chỉ import): NVL SKU đang có mà file không ghi. Import chỉ
	// THÊM nên chúng ở lại — file ghi "5 ly" cho SKU đang "3 ly" thành combo 3+5.
	KeptNotInFile []MaterialRef `json:"kept_not_in_file,omitempty"`
	// SameFamily: cặp NVL cùng loại khác độ dày trong bộ mới, ít nhất một chiếc
	// vừa được thêm — gần như chắc là nhầm.
	SameFamily []SameFamilyPair `json:"same_family,omitempty"`
	// InProduction: số sản phẩm (dòng đơn) của SKU còn trong xưởng có phần batch
	// sống; InProductionBy tách theo NVL của phần đó.
	InProduction   int                    `json:"in_production"`
	InProductionBy []InProductionMaterial `json:"in_production_by,omitempty"`
	// Grows: bộ mới nhiều NVL hơn bộ cũ — các sản phẩm đang làm sẽ bị coi là còn
	// thiếu tấm và hiện lại ở màn Tạo batch của NVL mới.
	Grows bool `json:"grows"`
}

// NeedsConfirm: thay đổi này phải được người dùng xác nhận.
func (i SKUMaterialImpact) NeedsConfirm() bool {
	return i.InProduction > 0 || len(i.SameFamily) > 0 || len(i.KeptNotInFile) > 0
}

// skuMaterialChange là bộ NVL của một SKU trước và sau khi sửa. FileSet chỉ có ở
// import (bộ NVL file ghi); nil = người dùng gửi đủ bộ (form SKU).
type skuMaterialChange struct {
	SKUID   uint
	SKUCode string
	Before  []MaterialRef
	After   []MaterialRef
	FileSet []MaterialRef
}

// thicknessUnits là các chữ chỉ độ dày đứng sau con số trong tên NVL.
var thicknessUnits = map[string]bool{"LY": true, "LI": true, "MM": true, "MI": true, "CM": true, "MILI": true, "DAY": true}

func isThicknessToken(t string) bool {
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i == 0 {
		return false
	}
	rest := t[i:]
	return rest == "" || thicknessUnits[rest]
}

// materialFamily là "loại" của NVL: tên bỏ dấu, bỏ phần độ dày. "Basswood 3 ly",
// "BASSWOOD-5-LY", "Basswood 5mm" → "BASSWOOD"; "Mica trong 3 ly" → "MICA-TRONG"
// (khác "MICA-HOLOGRAM"). "" = không suy ra được loại, không bao giờ trùng.
func materialFamily(m MaterialRef) string {
	src := m.Name
	if strings.TrimSpace(src) == "" {
		src = m.Code
	}
	code := materialCode(src)
	if code == "MAT" && strings.TrimSpace(src) == "" {
		return ""
	}
	toks := strings.Split(code, "-")
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		if t == "" || thicknessUnits[t] || isThicknessToken(t) {
			continue
		}
		out = append(out, t)
	}
	return strings.Join(out, "-")
}

// sameFamilyPairs trả các cặp cùng loại trong `after` có ít nhất một NVL thuộc
// `added` (cặp đã có từ trước, người dùng đã chấp nhận, không nhắc lại).
func sameFamilyPairs(after []MaterialRef, added map[string]bool) []SameFamilyPair {
	var out []SameFamilyPair
	for i := 0; i < len(after); i++ {
		fi := materialFamily(after[i])
		if fi == "" {
			continue
		}
		for j := i + 1; j < len(after); j++ {
			if materialFamily(after[j]) != fi {
				continue
			}
			if !added[after[i].key()] && !added[after[j].key()] {
				continue
			}
			out = append(out, SameFamilyPair{A: after[i], B: after[j]})
		}
	}
	return out
}

func refSet(refs []MaterialRef) map[string]bool {
	out := make(map[string]bool, len(refs))
	for _, r := range refs {
		out[r.key()] = true
	}
	return out
}

func refsMinus(a []MaterialRef, b map[string]bool) []MaterialRef {
	out := []MaterialRef{}
	for _, r := range a {
		if !b[r.key()] {
			out = append(out, r)
		}
	}
	return out
}

func sameRefSet(a, b []MaterialRef) bool {
	as, bs := refSet(a), refSet(b)
	if len(as) != len(bs) {
		return false
	}
	for k := range as {
		if !bs[k] {
			return false
		}
	}
	return true
}

// assessSKUMaterialChanges đánh giá các thay đổi bộ NVL và chỉ trả những SKU cần
// xác nhận. Bộ không đổi (chỉ sửa định mức / số lượng) thì bỏ qua, không tốn câu
// truy vấn nào; có SKU đổi bộ thì đọc phần đang sản xuất của chúng trong MỘT câu.
func assessSKUMaterialChanges(repo *repositories.Repositories, changes []skuMaterialChange) ([]SKUMaterialImpact, error) {
	var changed []skuMaterialChange
	var skuIDs []uint
	for _, c := range changes {
		if sameRefSet(c.Before, c.After) {
			continue
		}
		changed = append(changed, c)
		if c.SKUID != 0 {
			skuIDs = append(skuIDs, c.SKUID)
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}

	type matAgg struct {
		ref     MaterialRef
		items   map[uint]bool
		batches map[string]bool
	}
	type skuAgg struct {
		items map[uint]bool
		mats  map[uint]*matAgg
		order []uint
	}
	bySKU := map[uint]*skuAgg{}
	if len(skuIDs) > 0 {
		parts, err := repo.Batch.InProductionPartsForSKUs(skuIDs)
		if err != nil {
			return nil, err
		}
		for _, p := range parts {
			a := bySKU[p.SKUID]
			if a == nil {
				a = &skuAgg{items: map[uint]bool{}, mats: map[uint]*matAgg{}}
				bySKU[p.SKUID] = a
			}
			a.items[p.OrderItemID] = true
			m := a.mats[p.MaterialID]
			if m == nil {
				m = &matAgg{
					ref:   MaterialRef{ID: p.MaterialID, Code: p.MaterialCode, Name: p.MaterialName},
					items: map[uint]bool{}, batches: map[string]bool{},
				}
				a.mats[p.MaterialID] = m
				a.order = append(a.order, p.MaterialID)
			}
			m.items[p.OrderItemID] = true
			if p.BatchCode != "" {
				m.batches[p.BatchCode] = true
			}
		}
	}

	var out []SKUMaterialImpact
	for _, c := range changed {
		before, after := refSet(c.Before), refSet(c.After)
		imp := SKUMaterialImpact{
			SKUID: c.SKUID, SKUCode: c.SKUCode,
			Before: nonNilRefs(c.Before), After: nonNilRefs(c.After),
			Added: refsMinus(c.After, before), Removed: refsMinus(c.Before, after),
			Grows: len(after) > len(before),
		}
		if c.FileSet != nil && len(imp.Added) > 0 {
			if kept := refsMinus(c.Before, refSet(c.FileSet)); len(kept) > 0 {
				imp.KeptNotInFile = kept
			}
		}
		imp.SameFamily = sameFamilyPairs(c.After, refSet(imp.Added))
		if a := bySKU[c.SKUID]; a != nil && c.SKUID != 0 {
			imp.InProduction = len(a.items)
			for _, mid := range a.order {
				m := a.mats[mid]
				codes := make([]string, 0, len(m.batches))
				for code := range m.batches {
					codes = append(codes, code)
				}
				sort.Strings(codes)
				more := 0
				if len(codes) > maxImpactBatchCodes {
					more = len(codes) - maxImpactBatchCodes
					codes = codes[:maxImpactBatchCodes]
				}
				imp.InProductionBy = append(imp.InProductionBy, InProductionMaterial{
					Material: m.ref, Items: len(m.items), Batches: codes, MoreBatches: more,
				})
			}
		}
		if imp.NeedsConfirm() {
			out = append(out, imp)
		}
	}
	return out, nil
}

func nonNilRefs(r []MaterialRef) []MaterialRef {
	if r == nil {
		return []MaterialRef{}
	}
	return r
}

func joinRefLabels(refs []MaterialRef, sep string) string {
	labels := make([]string, 0, len(refs))
	for _, r := range refs {
		labels = append(labels, r.label())
	}
	return strings.Join(labels, sep)
}

// describeSKUMaterialImpact là câu tiếng Việt cho một SKU.
func describeSKUMaterialImpact(imp SKUMaterialImpact) string {
	var parts []string
	if imp.InProduction > 0 {
		where := make([]string, 0, len(imp.InProductionBy))
		for _, m := range imp.InProductionBy {
			s := fmt.Sprintf("%d trên %s", m.Items, m.Material.label())
			if len(m.Batches) > 0 {
				s += " (" + strings.Join(m.Batches, ", ")
				if m.MoreBatches > 0 {
					s += fmt.Sprintf(" +%d batch", m.MoreBatches)
				}
				s += ")"
			}
			where = append(where, s)
		}
		msg := fmt.Sprintf("đang có %d sản phẩm trong xưởng đã vào batch: %s", imp.InProduction, strings.Join(where, "; "))
		if imp.Grows {
			msg += " — thêm NVL lúc này thì các sản phẩm đó bị coi là còn thiếu tấm, hiện lại ở màn Tạo batch của NVL mới và dễ bị gom trùng"
		} else {
			msg += " — chúng vẫn làm tiếp trên NVL cũ; muốn làm lại bằng NVL mới thì phải xoá batch cũ"
		}
		parts = append(parts, msg)
	}
	for _, p := range imp.SameFamily {
		parts = append(parts, fmt.Sprintf("%s và %s cùng loại, khác độ dày — để cả hai là combo 2 tấm, gần như chắc là nhầm", p.A.label(), p.B.label()))
	}
	if len(imp.KeptNotInFile) > 0 {
		parts = append(parts, fmt.Sprintf("file ghi %s nhưng SKU đang dùng %s; import chỉ THÊM nên sau khi áp dụng SKU dùng cả %s",
			joinRefLabels(imp.Added, " + "), joinRefLabels(imp.KeptNotInFile, " + "), joinRefLabels(imp.After, " + ")))
	}
	return "SKU " + imp.SKUCode + ": " + strings.Join(parts, ". ") + "."
}

// skuMaterialChangeConfirmError dựng lỗi 409 của cửa xác nhận.
func skuMaterialChangeConfirmError(impacts []SKUMaterialImpact) *apperr.Error {
	const maxShown = 5
	lines := make([]string, 0, maxShown+2)
	for i, imp := range impacts {
		if i == maxShown {
			lines = append(lines, fmt.Sprintf("… và %d SKU khác.", len(impacts)-maxShown))
			break
		}
		lines = append(lines, describeSKUMaterialImpact(imp))
	}
	lines = append(lines, skuMaterialChangeHint+" Đúng là muốn vậy thì xác nhận để lưu.")
	return apperr.New(http.StatusConflict, CodeSKUMaterialChangeConfirm, strings.Join(lines, "\n")).
		WithDetails(map[string]any{"skus": impacts, "hint": skuMaterialChangeHint})
}

// materialRefsByID đọc tên/mã các NVL theo id — một câu, và chỉ khi có id.
func materialRefsByID(repo *repositories.Repositories, ids []uint) (map[uint]MaterialRef, error) {
	out := map[uint]MaterialRef{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out, nil
	}
	mats, err := repo.Material.ListByIDs(ids)
	if err != nil {
		return nil, err
	}
	for i := range mats {
		out[mats[i].ID] = materialRefOf(&mats[i])
	}
	return out, nil
}
