package services

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Pair-quota import: the production quota of each (SKU, NVL) pair — how many
// products of THAT SKU one sheet of THAT material yields — from a sheet of
// SKU + Loại VL + Định mức. The quota lives on the pair because the same mica
// sheet yields ten small trays but four large ones (see SKUMaterial), yet until
// now it could only be typed SKU by SKU; a pair left blank silently falls back
// to the material's default, or to "unlimited" — one batch however many sheets.
//
// The intended loop is export → fill → import: ExportPairQuotasXLSX lists every
// pair with its current quota (blank = none), the owner fills the blanks, and the
// same file comes back through Preview/Commit. Only existing pairs are touched:
// which SKU uses which material is the SKU import's job, never guessed here.
//
// A blank quota cell skips the row — it never clears a quota someone set.

const (
	errPairSKUBlank    = "SKU_BLANK"
	errPairSKUNotFound = "SKU_NOT_FOUND"
	errPairNotMapped   = "PAIR_NOT_MAPPED"
	errPairAmbiguous   = "PAIR_AMBIGUOUS"
	errPairConflict    = "PAIR_CONFLICT"
	errPairQuotaNotPos = "QUOTA_NOT_POSITIVE"
)

// materialCodeHeaders: the optional "Mã NVL" column. It pins the material when a
// SKU maps two materials sharing one name (the material import allows that).
var materialCodeHeaders = map[string]bool{
	"manvl": true, "mavl": true, "manguyenvatlieu": true, "materialcode": true,
}

// PairQuotaFileRow is one parsed row. Quota nil = blank cell (row skipped).
type PairQuotaFileRow struct {
	RowNumber    int    `json:"row_number"`
	SKU          string `json:"sku"`
	Material     string `json:"material"`
	MaterialCode string `json:"material_code,omitempty"`
	Quota        *int   `json:"quota"`
}

// PairQuotaItem is one pair the file sets.
type PairQuotaItem struct {
	SKUCode      string `json:"sku_code"`
	MaterialCode string `json:"material_code"`
	MaterialName string `json:"material_name"`
	MappingID    uint   `json:"mapping_id"`
	// CurrentQuota is the pair's own quota (nil = none, MaterialQuota applies).
	CurrentQuota  *int   `json:"current_quota"`
	MaterialQuota *int   `json:"material_quota"`
	Quota         int    `json:"quota"`
	Action        string `json:"action"` // UPDATE | NOCHANGE
	RowNumbers    []int  `json:"row_numbers"`
}

type PairQuotaRowError struct {
	RowNumber int    `json:"row_number"`
	SKU       string `json:"sku"`
	Material  string `json:"material"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	// Every row the error covers: a conflict is two rows disagreeing.
	RowNumbers []int `json:"row_numbers,omitempty"`
}

type PairQuotaSummary struct {
	TotalRows     int `json:"total_rows"`
	Updates       int `json:"updates"`
	Unchanged     int `json:"unchanged"`
	ErrorRows     int `json:"error_rows"`
	DuplicateRows int `json:"duplicate_rows"` // same pair + same quota as an earlier row
	BlankRows     int `json:"blank_rows"`     // quota cell left blank → skipped
}

type PairQuotaApplied struct {
	Updated int `json:"updated"`
}

type PairQuotaPreview struct {
	Filename string              `json:"filename"`
	Items    []PairQuotaItem     `json:"items"`
	Errors   []PairQuotaRowError `json:"errors"`
	Summary  PairQuotaSummary    `json:"summary"`
	Applied  *PairQuotaApplied   `json:"applied,omitempty"`
}

// ---------- Parsing ----------

// ParsePairQuotaFile reads SKU / Loại VL / Định mức (+ optional Mã NVL). Every
// other column — the export's product name and material default — is ignored.
func ParsePairQuotaFile(source string, r io.Reader) ([]PairQuotaFileRow, []PairQuotaRowError, error) {
	records, err := readLegacyGrid(source, r)
	if err != nil {
		return nil, nil, err
	}
	if len(records) < 2 {
		return nil, nil, apperr.BadRequest("File phải có dòng tiêu đề và ít nhất một dòng dữ liệu")
	}
	skuIdx, matIdx, codeIdx, quotaIdx := -1, -1, -1, -1
	for i, h := range records[0] {
		n := normalizeLegacyHeader(h)
		switch {
		case skuIdx == -1 && legacySKUHeaders[n]:
			skuIdx = i
		case codeIdx == -1 && materialCodeHeaders[n]:
			codeIdx = i
		case matIdx == -1 && legacyMaterialHeaders[n]:
			matIdx = i
		case quotaIdx == -1 && legacyQuotaHeaders[n]:
			quotaIdx = i
		}
	}
	var missing []string
	if skuIdx == -1 {
		missing = append(missing, "SKU")
	}
	if matIdx == -1 && codeIdx == -1 {
		missing = append(missing, "Loại VL")
	}
	if quotaIdx == -1 {
		missing = append(missing, "Định mức")
	}
	if len(missing) > 0 {
		return nil, nil, apperr.BadRequest("Không tìm thấy cột " + strings.Join(missing, ", ") +
			" — dùng file tải từ nút \"Tải file định mức theo SKU\"")
	}

	cell := func(rec []string, idx int) string {
		if idx >= 0 && idx < len(rec) {
			return strings.TrimSpace(rec[idx])
		}
		return ""
	}
	var rows []PairQuotaFileRow
	var errs []PairQuotaRowError
	for di, rec := range records[1:] {
		row := PairQuotaFileRow{
			RowNumber: di + 1, SKU: cell(rec, skuIdx), Material: cell(rec, matIdx),
			MaterialCode: cell(rec, codeIdx),
		}
		raw := cell(rec, quotaIdx)
		if row.SKU == "" && row.Material == "" && row.MaterialCode == "" && raw == "" {
			continue // blank line
		}
		quota, ok := parseQuotaCell(raw)
		switch {
		case !ok:
			errs = append(errs, PairQuotaRowError{
				RowNumber: row.RowNumber, SKU: row.SKU, Material: row.Material, ErrorCode: errQuotaInvalid,
				Message: "Định mức \"" + raw + "\" không phải số (để trống = bỏ qua dòng)",
			})
			continue
		case raw != "" && quota == nil:
			errs = append(errs, pairQuotaNotPositive(row, raw))
			continue
		}
		row.Quota = quota
		rows = append(rows, row)
	}
	return rows, errs, nil
}

func pairQuotaNotPositive(row PairQuotaFileRow, raw string) PairQuotaRowError {
	return PairQuotaRowError{
		RowNumber: row.RowNumber, SKU: row.SKU, Material: row.Material, ErrorCode: errPairQuotaNotPos,
		Message: "Định mức phải lớn hơn 0 (\"" + raw + "\") — để trống nếu chưa biết",
	}
}

// ---------- Analysis ----------

// matchPairMaterial picks which of the SKU's own mappings a row names. Only the
// SKU's mappings are candidates, so a material name shared across the catalog
// is no problem; within one SKU, a code (when given) pins it exactly.
func matchPairMaterial(row PairQuotaFileRow, mappings []models.SKUMaterial) []models.SKUMaterial {
	var out []models.SKUMaterial
	code := models.NormalizeCode(row.MaterialCode)
	name := models.NormalizeCode(row.Material)
	for _, mp := range mappings {
		switch {
		case code != "":
			if models.NormalizeCode(mp.Material.Code) == code {
				out = append(out, mp)
			}
		case name != "":
			// The name column also takes a code: the SKU screen shows codes.
			if models.NormalizeCode(mp.Material.Name) == name || models.NormalizeCode(mp.Material.Code) == name {
				out = append(out, mp)
			}
		}
	}
	return out
}

func (s *CatalogService) analyzePairQuota(rows []PairQuotaFileRow, parseErrors []PairQuotaRowError) (PairQuotaPreview, error) {
	codes := make([]string, 0, len(rows))
	for _, r := range rows {
		codes = append(codes, r.SKU)
	}
	skus, err := s.repo.SKU.ListByCodes(codes)
	if err != nil {
		return PairQuotaPreview{}, apperr.Internal("could not read SKUs").Wrap(err)
	}
	skuByCode := make(map[string]models.SKU, len(skus))
	ids := make([]uint, 0, len(skus))
	for _, sk := range skus {
		skuByCode[models.NormalizeCode(sk.Code)] = sk
		ids = append(ids, sk.ID)
	}
	mappings, err := s.repo.SKU.MappingsWithMaterial(ids)
	if err != nil {
		return PairQuotaPreview{}, apperr.Internal("could not read SKU materials").Wrap(err)
	}
	mappingsBySKU := map[uint][]models.SKUMaterial{}
	for _, mp := range mappings {
		mappingsBySKU[mp.SKUID] = append(mappingsBySKU[mp.SKUID], mp)
	}

	errs := append([]PairQuotaRowError{}, parseErrors...)
	sum := PairQuotaSummary{}
	type agg struct {
		sku     models.SKU
		mapping models.SKUMaterial
		quotas  map[int]bool
		rowNums []int
		quota   int
	}
	byMapping := map[uint]*agg{}
	var order []uint
	fail := func(r PairQuotaFileRow, code, msg string) {
		errs = append(errs, PairQuotaRowError{
			RowNumber: r.RowNumber, SKU: r.SKU, Material: firstNonEmpty(r.Material, r.MaterialCode),
			ErrorCode: code, Message: msg,
		})
	}
	for _, r := range rows {
		sum.TotalRows++
		mat := firstNonEmpty(r.Material, r.MaterialCode)
		switch {
		case r.SKU == "":
			fail(r, errPairSKUBlank, "Dòng thiếu SKU")
			continue
		case mat == "":
			fail(r, errMaterialBlank, "Dòng thiếu Loại VL")
			continue
		case r.Quota == nil:
			sum.BlankRows++ // not filled in yet — nothing to set
			continue
		case *r.Quota <= 0:
			errs = append(errs, pairQuotaNotPositive(r, strconv.Itoa(*r.Quota)))
			continue
		}
		sk, ok := skuByCode[models.NormalizeCode(r.SKU)]
		if !ok {
			fail(r, errPairSKUNotFound, "SKU "+r.SKU+" không có trong hệ thống")
			continue
		}
		matches := matchPairMaterial(r, mappingsBySKU[sk.ID])
		switch len(matches) {
		case 0:
			fail(r, errPairNotMapped, "SKU "+sk.Code+" chưa gán NVL \""+mat+"\" — gán NVL cho SKU (import SKU) trước rồi mới đặt định mức")
			continue
		case 1:
		default:
			fail(r, errPairAmbiguous, "SKU "+sk.Code+" có nhiều NVL cùng tên \""+mat+"\" — ghi thêm cột Mã NVL để chỉ đúng NVL")
			continue
		}
		mp := matches[0]
		a := byMapping[mp.ID]
		if a == nil {
			a = &agg{sku: sk, mapping: mp, quotas: map[int]bool{}}
			byMapping[mp.ID] = a
			order = append(order, mp.ID)
		} else if a.quotas[*r.Quota] {
			sum.DuplicateRows++
		}
		a.quotas[*r.Quota] = true
		a.quota = *r.Quota
		a.rowNums = append(a.rowNums, r.RowNumber)
	}

	pv := PairQuotaPreview{Items: []PairQuotaItem{}}
	for _, id := range order {
		a := byMapping[id]
		if len(a.quotas) > 1 {
			// Two rows, two numbers for one pair: taking either is a guess.
			nums := make([]int, 0, len(a.quotas))
			for q := range a.quotas {
				nums = append(nums, q)
			}
			sort.Ints(nums)
			vals := make([]string, len(nums))
			for i, q := range nums {
				vals[i] = strconv.Itoa(q)
			}
			errs = append(errs, PairQuotaRowError{
				RowNumber: a.rowNums[0], SKU: a.sku.Code, Material: a.mapping.Material.Name,
				ErrorCode: errPairConflict, RowNumbers: a.rowNums,
				Message: "Cùng SKU + NVL nhưng ghi nhiều định mức khác nhau (" + strings.Join(vals, ", ") + ") — giữ lại một số",
			})
			continue
		}
		action := matActionUpdate
		if quotaEqual(a.mapping.ProductsPerUnit, &a.quota) {
			action = matActionNoChange
			sum.Unchanged++
		} else {
			sum.Updates++
		}
		pv.Items = append(pv.Items, PairQuotaItem{
			SKUCode: a.sku.Code, MaterialCode: a.mapping.Material.Code, MaterialName: a.mapping.Material.Name,
			MappingID: a.mapping.ID, CurrentQuota: a.mapping.ProductsPerUnit,
			MaterialQuota: a.mapping.Material.ProductsPerUnit, Quota: a.quota,
			Action: action, RowNumbers: a.rowNums,
		})
	}
	sum.ErrorRows = len(errs)
	pv.Errors = errs
	pv.Summary = sum
	return pv, nil
}

// ---------- Preview / Commit ----------

// PreviewPairQuotaImport returns the plan; nothing is written.
func (s *CatalogService) PreviewPairQuotaImport(filename string, rows []PairQuotaFileRow, parseErrors []PairQuotaRowError) (*PairQuotaPreview, error) {
	pv, err := s.analyzePairQuota(rows, parseErrors)
	if err != nil {
		return nil, err
	}
	pv.Filename = filename
	return &pv, nil
}

// CommitPairQuotaImport re-analyses the rows (the catalog may have moved since
// the preview) and writes the changed quotas. Rows in error are not applied.
// OWNER-only: the quota is the owner's lever, as on the SKU screen.
func (s *CatalogService) CommitPairQuotaImport(actor Actor, rows []PairQuotaFileRow) (*PairQuotaPreview, error) {
	if actor.Role != models.RoleOwner {
		return nil, apperr.Forbidden("Chỉ OWNER được nhập định mức")
	}
	pv, err := s.analyzePairQuota(rows, nil)
	if err != nil {
		return nil, err
	}
	idsByQuota := map[int][]uint{}
	updated := 0
	for _, it := range pv.Items {
		if it.Action == matActionUpdate {
			idsByQuota[it.Quota] = append(idsByQuota[it.Quota], it.MappingID)
			updated++
		}
	}
	if updated > 0 {
		err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
			return repositories.New(tx).SKU.SetPairQuotas(idsByQuota)
		})
		if err != nil {
			return nil, apperr.Internal("could not save quotas").Wrap(err)
		}
	}
	pv.Applied = &PairQuotaApplied{Updated: updated}
	s.audit.Log(actor, "PAIR_QUOTA_IMPORT_COMMIT", "sku_material", nil,
		fmt.Sprintf("Pair quota import: %d pairs updated, %d errors", updated, len(pv.Errors)), nil)
	return &pv, nil
}

// ---------- Export ----------

// pairQuotaHeaders: "Định mức" is the only column the import writes from; the
// product name and material default are there to read while filling it in.
var pairQuotaHeaders = []string{"SKU", "Tên sản phẩm", "Loại VL", "Mã NVL", "Định mức", "Định mức mặc định của NVL (tham khảo)"}

func quotaCell(q *int) string {
	if q == nil {
		return ""
	}
	return strconv.Itoa(*q)
}

// ExportPairQuotasXLSX lists every (SKU, NVL) pair of the active catalog with
// its current quota — blank where the pair has none — as the sheet to fill in.
func (s *CatalogService) ExportPairQuotasXLSX() ([]byte, string, error) {
	pairs, err := s.repo.SKU.AllPairQuotas()
	if err != nil {
		return nil, "", apperr.Internal("could not read SKU materials").Wrap(err)
	}
	grid := make([][]string, 0, len(pairs)+1)
	grid = append(grid, pairQuotaHeaders)
	for _, p := range pairs {
		grid = append(grid, []string{
			p.SKUCode, p.ProductName, p.MaterialName, p.MaterialCode,
			quotaCell(p.PairQuota), quotaCell(p.MaterialQuota),
		})
	}
	data, err := buildTemplateXLSX("Định mức theo SKU", grid, []float64{26, 30, 28, 22, 12, 22})
	if err != nil {
		return nil, "", err
	}
	return data, "dinh-muc-theo-sku.xlsx", nil
}
