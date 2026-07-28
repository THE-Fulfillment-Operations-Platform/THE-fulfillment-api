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
	MaterialID         *uint  `json:"material_id"`         // the existing material this row updates
	CurrentQuota       *int   `json:"current_quota"`       // existing quota if the material exists
	Quota              *int   `json:"quota"`               // quota from file (nil = unlimited/blank)
	CurrentDescription string `json:"current_description"` // existing description
	Description        string `json:"description"`         // description from file (blank = leave as-is)
	Action             string `json:"action"`              // CREATE | UPDATE | NOCHANGE
	RowNumbers         []int  `json:"row_numbers"`
	// NameVariant marks a material whose name also appears on another line of the
	// file with a different quota/description — those are separate materials, not a
	// contradiction, so the preview can flag them instead of silently merging.
	NameVariant bool `json:"name_variant"`
}

type MaterialImportRowError struct {
	RowNumber int    `json:"row_number"`
	Material  string `json:"material"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	// Every file row this error covers. A quota conflict is caused by two or more
	// rows at once, so reporting only RowNumber would send the owner to fix one
	// half of a disagreement; the preview highlights all of them.
	RowNumbers []int `json:"row_numbers,omitempty"`
}

type MaterialImportSummary struct {
	TotalRows    int `json:"total_rows"`
	NewMaterials int `json:"new_materials"`
	Updates      int `json:"updates"` // materials whose quota and/or description changed
	Unchanged    int `json:"unchanged"`
	ErrorRows    int `json:"error_rows"`
	// DuplicateRows counts the rows folded away because an earlier row carried the
	// exact same Loại VL + Định mức + Mô tả (a real spreadsheet duplicate).
	DuplicateRows int `json:"duplicate_rows"`
	// NameVariants counts the materials that share a name with another line but
	// differ in quota/description — kept apart on purpose, worth showing.
	NameVariants int `json:"name_variants"`
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

// materialInsertBatch is how many new materials ride in one INSERT.
const materialInsertBatch = 200

// mintMaterialCode hands out a free code for `base`, appending -2, -3… on
// collision and remembering what it handed out. Cấp mã trong bộ nhớ,
// resolved against an in-memory set so an import costs one code read in total
// rather than a lookup per new material.
func mintMaterialCode(taken map[string]bool, base string) string {
	base = models.NormalizeCode(base)
	if base == "" {
		base = "MAT"
	}
	code := base
	for i := 2; taken[code]; i++ {
		suffix := "-" + strconv.Itoa(i)
		trimTo := 32 - len(suffix)
		b := base
		if len(b) > trimTo {
			b = strings.TrimRight(b[:trimTo], "-")
		}
		code = b + suffix
	}
	taken[code] = true
	return code
}

// tripleKey identifies one material line by its three columns together — Loại VL
// + Định mức + Mô tả. It is what "dòng trùng nhau" means for this import: only
// rows equal in all three are the same material and get folded into one. Differ in
// any one column and it is a separate material, kept apart.
func tripleKey(name string, quota *int, desc string) string {
	return strings.ToLower(strings.TrimSpace(name)) + "\x00" +
		quotaKey(quota) + "\x00" +
		strings.ToLower(strings.TrimSpace(desc))
}

// materialIndex is the slice of the catalog a single import touches, read in one
// query up front. Matching every line against this map is what keeps an import of
// a few hundred rows at a couple of round-trips instead of one per row — on a
// hosted database that is the difference between a second and minutes.
type materialIndex struct {
	byName map[string][]*models.Material // key: lower(trim(name))
}

// loadMaterialIndex reads every catalog material carrying one of the file's names.
// A failure is returned, never swallowed: pretending the catalog is empty would
// turn every line into CREATE and duplicate the whole thing on commit.
func (s *CatalogService) loadMaterialIndex(rows []MaterialQuotaRow) (*materialIndex, error) {
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Material)
	}
	found, err := s.repo.Material.ListByNamesInsensitive(names)
	if err != nil {
		return nil, err
	}
	idx := &materialIndex{byName: make(map[string][]*models.Material, len(found))}
	for i := range found {
		m := &found[i]
		key := strings.ToLower(strings.TrimSpace(m.Name))
		idx.byName[key] = append(idx.byName[key], m)
	}
	return idx, nil
}

// matchCatalogMaterial picks which existing material (if any) a file line refers
// to. Names are not unique in the catalog — this import can deliberately create
// several materials sharing one — so the line is matched on its data, best fit
// first: same quota AND description, then same quota, and only when the file uses
// that name on a single line does a name-only match count (the plain "sửa định
// mức của NVL này" case). Anything already claimed by an earlier line is off the
// table, and no fit at all means the line is a new material.
func (idx *materialIndex) matchCatalogMaterial(name string, quota *int, desc string, nameVariant bool, claimed map[uint]bool) *models.Material {
	candidates := idx.byName[strings.ToLower(strings.TrimSpace(name))]
	var best *models.Material
	bestScore := 0
	for _, c := range candidates {
		if claimed[c.ID] {
			continue
		}
		// A blank cell says "leave as is", so it matches whatever is stored.
		quotaMatch := quota == nil || quotaEqual(c.ProductsPerUnit, quota)
		descMatch := desc == "" || strings.EqualFold(strings.TrimSpace(c.Description), desc)
		score := 0
		switch {
		case quotaMatch && descMatch:
			score = 3
		case quotaMatch:
			score = 2
		case !nameVariant:
			score = 1
		}
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	return best
}

// analyzeMaterialImport folds exact-duplicate rows together, keeps same-name rows
// that differ in quota/description apart as separate materials, and derives the
// per-material action against what is already in the catalog.
func (s *CatalogService) analyzeMaterialImport(rows []MaterialQuotaRow, parseErrors []MaterialImportRowError) (MaterialImportPreview, error) {
	idx, err := s.loadMaterialIndex(rows)
	if err != nil {
		return MaterialImportPreview{}, apperr.Internal("could not read materials").Wrap(err)
	}
	type agg struct {
		name    string
		quota   *int
		desc    string
		rowNums []int
	}
	byTriple := map[string]*agg{}
	var order []string
	// Which distinct triples each name covers, so a name used on several lines can
	// be flagged (and matched against the catalog) without a second pass.
	variantsByName := map[string]int{}
	errs := append([]MaterialImportRowError{}, parseErrors...)
	total, duplicates := 0, 0

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
		key := tripleKey(name, r.Quota, desc)
		a := byTriple[key]
		if a == nil {
			a = &agg{name: name, quota: r.Quota, desc: desc}
			byTriple[key] = a
			order = append(order, key)
			variantsByName[strings.ToLower(name)]++
		} else {
			duplicates++ // same three columns as an earlier row → one material
		}
		a.rowNums = append(a.rowNums, r.RowNumber)
	}

	pv := MaterialImportPreview{}
	sum := MaterialImportSummary{TotalRows: total, DuplicateRows: duplicates}
	// A catalog material can back at most one line of the file: once a line claims
	// it, the next line with the same name has to create its own material instead of
	// both overwriting the same row.
	claimed := map[uint]bool{}
	for _, key := range order {
		a := byTriple[key]
		nameVariant := variantsByName[strings.ToLower(a.name)] > 1
		if nameVariant {
			sum.NameVariants++
		}

		match := idx.matchCatalogMaterial(a.name, a.quota, a.desc, nameVariant, claimed)
		exists := match != nil
		var materialID *uint
		var current *int
		currentDesc := ""
		if exists {
			claimed[match.ID] = true
			id := match.ID
			materialID = &id
			current = match.ProductsPerUnit
			currentDesc = match.Description
		}

		// Blank cells never overwrite: a blank quota/description only takes effect
		// when creating a brand-new material, never to clear an existing value.
		quotaChange := a.quota != nil && !quotaEqual(current, a.quota)
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
			Name: a.name, Code: materialCode(a.name), Exists: exists, MaterialID: materialID,
			CurrentQuota: current, Quota: a.quota,
			CurrentDescription: currentDesc, Description: a.desc,
			Action: action, RowNumbers: a.rowNums, NameVariant: nameVariant,
		})
	}

	sum.ErrorRows = len(errs)
	pv.Errors = errs
	pv.Summary = sum
	return pv, nil
}

// ---------- Preview / Commit ----------

// PreviewMaterialImport analyses rows and returns the plan (nothing is written).
func (s *CatalogService) PreviewMaterialImport(filename string, rows []MaterialQuotaRow, parseErrors []MaterialImportRowError) (*MaterialImportPreview, error) {
	pv, err := s.analyzeMaterialImport(rows, parseErrors)
	if err != nil {
		return nil, err
	}
	pv.Filename = filename
	return &pv, nil
}

// CommitMaterialImport applies the plan: find-or-create each material and set its
// quota. Additive and non-destructive — a blank quota never clears an existing one
// (those rows resolve to NOCHANGE in analyze). OWNER-only.
func (s *CatalogService) CommitMaterialImport(actor Actor, rows []MaterialQuotaRow) (*MaterialImportPreview, error) {
	if actor.Role != models.RoleOwner {
		return nil, apperr.Forbidden("Chỉ OWNER được nhập định mức nguyên vật liệu")
	}
	pv, err := s.analyzeMaterialImport(rows, nil)
	if err != nil {
		return nil, err
	}

	applied := MaterialImportApplied{}
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		// Every code in one read, then mint in memory: probing the database per new
		// material turned a 200-material file into 200+ round-trips.
		existingCodes, err := txRepo.Material.AllCodes()
		if err != nil {
			return err
		}
		taken := make(map[string]bool, len(existingCodes))
		for _, c := range existingCodes {
			taken[models.NormalizeCode(c)] = true
		}

		var toCreate []models.Material
		for _, it := range pv.Items {
			switch it.Action {
			case matActionCreate:
				toCreate = append(toCreate, models.Material{
					Code: mintMaterialCode(taken, it.Code), Name: it.Name,
					Description: it.Description, ProductsPerUnit: it.Quota,
				})
				applied.Created++
			case matActionUpdate:
				// By ID, not by name: several materials may share a name, and analyze
				// already decided which one this line belongs to. A name lookup here
				// would write to whichever row the DB returned first. Only the columns
				// the file actually fills are written — a blank cell never clears a value.
				if it.MaterialID == nil {
					continue
				}
				fields := map[string]any{}
				if it.Quota != nil {
					fields["products_per_unit"] = *it.Quota
				}
				if it.Description != "" {
					fields["description"] = it.Description
				}
				if len(fields) == 0 {
					continue
				}
				if err := tx.Model(&models.Material{}).Where("id = ?", *it.MaterialID).
					Updates(fields).Error; err != nil {
					return err
				}
				applied.Updated++
			}
		}
		// One INSERT per batch instead of one per material.
		return txRepo.Material.CreateMany(toCreate, materialInsertBatch)
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
