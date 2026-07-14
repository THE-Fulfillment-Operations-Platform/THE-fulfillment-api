package services

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Material-data import: seed each material's production quota (định mức) from a
// small 2-column spreadsheet — `Loại VL` (material name) + `Định mức` (max
// products per unit). One row per material (not per SKU), so the quota lives
// exactly where it belongs. Preview → commit, OWNER-only (quota is an OWNER lever).
//
// Blank quota is intentional and never destructive: for a new material it creates
// it "unlimited"; for an existing material it leaves the current quota untouched
// (so a blank cell can't accidentally wipe a quota the owner set).

const (
	matActionCreate   = "CREATE"
	matActionUpdate   = "UPDATE"
	matActionNoChange = "NOCHANGE"

	errQuotaInvalid  = "QUOTA_INVALID"
	errQuotaConflict = "QUOTA_CONFLICT"
	errMaterialBlank = "MATERIAL_BLANK"
)

// quota column header aliases (normalized: diacritics stripped, lowercased, no
// spaces/underscores/dashes/dots/slashes — see normalizeLegacyHeader).
var legacyQuotaHeaders = map[string]bool{
	"dinhmuc": true, "dinhmucsanxuat": true, "dinhmucnvl": true,
	"quota": true, "productsperunit": true, "capacity": true,
	"sanphamdonvi": true, "spdonvi": true,
}

// description column is optional (blank when absent).
var legacyDescHeaders = map[string]bool{
	"mota": true, "ghichu": true, "description": true, "note": true, "desc": true,
}

var quotaDigitsRe = regexp.MustCompile(`-?\d+`)

// MaterialQuotaRow is one parsed row: a material name, its quota (nil = blank)
// and an optional description.
type MaterialQuotaRow struct {
	RowNumber   int    `json:"row_number"`
	Material    string `json:"material"`
	Quota       *int   `json:"quota"`
	Description string `json:"description"`
}

type MaterialImportItem struct {
	Name               string `json:"name"`
	Code               string `json:"code"`
	Exists             bool   `json:"exists"`
	CurrentQuota       *int   `json:"current_quota"`       // existing quota if the material exists
	Quota              *int   `json:"quota"`               // quota from file (nil = unlimited/blank)
	CurrentDescription string `json:"current_description"` // existing description
	Description        string `json:"description"`         // description from file (blank = leave as-is)
	Action             string `json:"action"`              // CREATE | UPDATE | NOCHANGE
	RowNumbers         []int  `json:"row_numbers"`
}

type MaterialImportRowError struct {
	RowNumber int    `json:"row_number"`
	Material  string `json:"material"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

type MaterialImportSummary struct {
	TotalRows    int `json:"total_rows"`
	NewMaterials int `json:"new_materials"`
	Updates      int `json:"updates"` // materials whose quota and/or description changed
	Unchanged    int `json:"unchanged"`
	ErrorRows    int `json:"error_rows"`
}

type MaterialImportApplied struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
}

type MaterialImportPreview struct {
	Filename string                   `json:"filename"`
	Items    []MaterialImportItem     `json:"items"`
	Errors   []MaterialImportRowError `json:"errors"`
	Summary  MaterialImportSummary    `json:"summary"`
	Applied  *MaterialImportApplied   `json:"applied,omitempty"`
}

// ---------- Parsing ----------

// parseQuotaCell reads a quota cell. Blank → (nil, ok) = unlimited/leave alone. A
// value with a positive integer → (&n, ok). A value ≤0 → (nil, ok) = unlimited. A
// value with no number at all → (nil, !ok) so the row can be flagged.
func parseQuotaCell(raw string) (*int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	m := quotaDigitsRe.FindString(raw)
	if m == "" {
		return nil, false
	}
	n, err := strconv.Atoi(m)
	if err != nil {
		return nil, false
	}
	if n <= 0 {
		return nil, true
	}
	return &n, true
}

// ParseMaterialQuotaFile parses a CSV/XLSX stream into MaterialQuotaRows,
// auto-detecting the `Loại VL` and `Định mức` columns. Rows whose quota cell
// isn't a number are returned as parse errors (never silently dropped).
func ParseMaterialQuotaFile(source string, r io.Reader) ([]MaterialQuotaRow, []MaterialImportRowError, error) {
	records, err := readLegacyGrid(source, r)
	if err != nil {
		return nil, nil, err
	}
	if len(records) < 2 {
		return nil, nil, apperr.BadRequest("File phải có dòng tiêu đề và ít nhất một dòng dữ liệu")
	}
	header := records[0]
	matIdx, quotaIdx, descIdx := -1, -1, -1
	for i, h := range header {
		n := normalizeLegacyHeader(h)
		if matIdx == -1 && legacyMaterialHeaders[n] {
			matIdx = i
		}
		if quotaIdx == -1 && legacyQuotaHeaders[n] {
			quotaIdx = i
		}
		if descIdx == -1 && legacyDescHeaders[n] {
			descIdx = i
		}
	}
	if matIdx == -1 {
		return nil, nil, apperr.BadRequest("Không tìm thấy cột 'Loại VL' trong file — kiểm tra dòng tiêu đề")
	}
	if quotaIdx == -1 {
		return nil, nil, apperr.BadRequest("Không tìm thấy cột 'Định mức' trong file — kiểm tra dòng tiêu đề")
	}
	// descIdx == -1 is fine: the description column is optional.

	var rows []MaterialQuotaRow
	var parseErrors []MaterialImportRowError
	for di, rec := range records[1:] {
		rowNum := di + 1
		name, rawQuota, desc := "", "", ""
		if matIdx < len(rec) {
			name = strings.TrimSpace(rec[matIdx])
		}
		if quotaIdx < len(rec) {
			rawQuota = strings.TrimSpace(rec[quotaIdx])
		}
		if descIdx >= 0 && descIdx < len(rec) {
			desc = strings.TrimSpace(rec[descIdx])
		}
		if name == "" && rawQuota == "" && desc == "" {
			continue // blank line
		}
		quota, ok := parseQuotaCell(rawQuota)
		if !ok {
			parseErrors = append(parseErrors, MaterialImportRowError{
				RowNumber: rowNum, Material: name, ErrorCode: errQuotaInvalid,
				Message: "Định mức phải là số nguyên (để trống = không giới hạn)",
			})
			continue
		}
		rows = append(rows, MaterialQuotaRow{RowNumber: rowNum, Material: name, Quota: quota, Description: desc})
	}
	return rows, parseErrors, nil
}

// ---------- Analysis ----------

func quotaKey(q *int) string {
	if q == nil {
		return "nil"
	}
	return strconv.Itoa(*q)
}

func quotaEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// analyzeMaterialImport groups rows by material name, detects quota conflicts
// (same material spelled with different quotas) and derives per-material actions.
func (s *CatalogService) analyzeMaterialImport(rows []MaterialQuotaRow, parseErrors []MaterialImportRowError) MaterialImportPreview {
	type agg struct {
		name    string
		keys    map[string]*int // distinct quota values seen for this material
		desc    string          // first non-empty description across the rows
		rowNums []int
	}
	byName := map[string]*agg{}
	var order []string
	errs := append([]MaterialImportRowError{}, parseErrors...)
	total := 0

	for _, r := range rows {
		name := strings.TrimSpace(r.Material)
		desc := strings.TrimSpace(r.Description)
		if name == "" {
			if r.Quota != nil || desc != "" {
				errs = append(errs, MaterialImportRowError{
					RowNumber: r.RowNumber, ErrorCode: errMaterialBlank,
					Message: "Dòng có dữ liệu nhưng thiếu Loại VL",
				})
			}
			continue
		}
		total++
		key := strings.ToLower(name)
		a := byName[key]
		if a == nil {
			a = &agg{name: name, keys: map[string]*int{}}
			byName[key] = a
			order = append(order, key)
		}
		a.keys[quotaKey(r.Quota)] = r.Quota
		if a.desc == "" && desc != "" {
			a.desc = desc
		}
		a.rowNums = append(a.rowNums, r.RowNumber)
	}

	pv := MaterialImportPreview{}
	sum := MaterialImportSummary{TotalRows: total}
	for _, key := range order {
		a := byName[key]
		if len(a.keys) > 1 {
			errs = append(errs, MaterialImportRowError{
				RowNumber: a.rowNums[0], Material: a.name, ErrorCode: errQuotaConflict,
				Message: "Cùng Loại VL nhưng định mức khác nhau giữa các dòng",
			})
			continue
		}
		var quota *int
		for _, v := range a.keys {
			quota = v
		}

		exists := false
		var current *int
		currentDesc := ""
		if m, _ := s.repo.Material.FindByNameInsensitive(a.name); m != nil {
			exists = true
			current = m.ProductsPerUnit
			currentDesc = m.Description
		}

		// Blank cells never overwrite: a blank quota/description only takes effect
		// when creating a brand-new material, never to clear an existing value.
		quotaChange := quota != nil && !quotaEqual(current, quota)
		descChange := a.desc != "" && a.desc != currentDesc
		action := matActionNoChange
		switch {
		case !exists:
			action = matActionCreate
			sum.NewMaterials++
		case quotaChange || descChange:
			action = matActionUpdate
			sum.Updates++
		default:
			sum.Unchanged++
		}

		pv.Items = append(pv.Items, MaterialImportItem{
			Name: a.name, Code: materialCode(a.name), Exists: exists,
			CurrentQuota: current, Quota: quota,
			CurrentDescription: currentDesc, Description: a.desc,
			Action: action, RowNumbers: a.rowNums,
		})
	}

	sum.ErrorRows = len(errs)
	pv.Errors = errs
	pv.Summary = sum
	return pv
}

// ---------- Preview / Commit ----------

// PreviewMaterialImport analyses rows and returns the plan (nothing is written).
func (s *CatalogService) PreviewMaterialImport(filename string, rows []MaterialQuotaRow, parseErrors []MaterialImportRowError) *MaterialImportPreview {
	pv := s.analyzeMaterialImport(rows, parseErrors)
	pv.Filename = filename
	return &pv
}

// CommitMaterialImport applies the plan: find-or-create each material and set its
// quota. Additive and non-destructive — a blank quota never clears an existing one
// (those rows resolve to NOCHANGE in analyze). OWNER-only.
func (s *CatalogService) CommitMaterialImport(actor Actor, rows []MaterialQuotaRow) (*MaterialImportPreview, error) {
	if actor.Role != models.RoleOwner {
		return nil, apperr.Forbidden("Chỉ OWNER được nhập định mức nguyên vật liệu")
	}
	pv := s.analyzeMaterialImport(rows, nil)

	applied := MaterialImportApplied{}
	err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		for _, it := range pv.Items {
			switch it.Action {
			case matActionCreate:
				code := uniqueMaterialCode(txRepo, it.Code)
				m := &models.Material{Code: code, Name: it.Name, Description: it.Description, ProductsPerUnit: it.Quota}
				if err := txRepo.Material.Create(m); err != nil {
					return err
				}
				applied.Created++
			case matActionUpdate:
				m, err := txRepo.Material.FindByNameInsensitive(it.Name)
				if err != nil {
					return err
				}
				if m == nil {
					continue
				}
				// Blank cells never clear an existing value — only overwrite when the
				// file actually provides one.
				if it.Quota != nil {
					m.ProductsPerUnit = it.Quota
				}
				if it.Description != "" {
					m.Description = it.Description
				}
				if err := txRepo.Material.Update(m); err != nil {
					return err
				}
				applied.Updated++
			}
		}
		return nil
	})
	if err != nil {
		return nil, apperr.Internal("could not commit material import").Wrap(err)
	}

	pv.Applied = &applied
	s.audit.Log(actor, "MATERIAL_IMPORT_COMMIT", "material", nil,
		fmt.Sprintf("Material quota import: %d created, %d updated", applied.Created, applied.Updated), nil)
	return &pv, nil
}

// ---------- Sample template ----------

// Mô tả is optional — the importer works with or without it.
var materialTemplateHeaders = []string{"Loại VL", "Định mức", "Mô tả"}

var materialTemplateSample = [][]string{
	{"Mica trong 3 ly", "20", "Mica trong suốt 3mm"},
	{"Gỗ 5 ly 3 layer", "12", ""},           // mô tả để trống cũng được
	{"Mica Hologram", "", "Mica ánh 7 màu"}, // định mức trống = không giới hạn / giữ nguyên
}

// MaterialTemplateXLSX renders the material-quota import sample as a real .xlsx.
func (s *CatalogService) MaterialTemplateXLSX() ([]byte, string, error) {
	grid := append([][]string{materialTemplateHeaders}, materialTemplateSample...)
	data, err := buildTemplateXLSX("Định mức NVL", grid, []float64{28, 12, 32})
	if err != nil {
		return nil, "", err
	}
	return data, "material-quota-template.xlsx", nil
}
