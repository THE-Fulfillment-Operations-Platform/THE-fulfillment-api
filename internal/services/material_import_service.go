package services

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Material import: seed each material's SHEET SIZE from a small spreadsheet —
// `Loại VL` (material name) + `Dài (mm)` + `Rộng (mm)` (+ optional `Mô tả`).
// One row per material. The sheet size feeds the ESTIMATED production quota
// (grid packing, models.EstimatedQuota) of every pair nobody has declared a
// quota for; the declared quota itself lives on the (SKU, material) pair and
// comes in through the SKU file's "Định mức" column — not here (khách chốt
// 2026-09-18 for sizes, 2026-09-24 for declared quotas).
//
// A material is identified by its name (case-insensitive). Blank cells are never
// destructive: for a new material they create it without a size; for an existing
// one they leave the stored size / description untouched.

// Per-line actions of the stateless importers (materials, parent SKUs).
const (
	importActionCreate   = "CREATE"
	importActionUpdate   = "UPDATE"
	importActionNoChange = "NOCHANGE"
)

const (
	errMaterialBlank       = "MATERIAL_BLANK"
	errMaterialRowConflict = "MATERIAL_ROW_CONFLICT" // same name, different sizes / descriptions in the file
	errMaterialAmbiguous   = "MATERIAL_AMBIGUOUS"    // several catalog materials already carry this name
)

// description column is optional (blank when absent). Broader aliases than the
// SKU importer's: this file is about materials, so a "Ghi chú" IS the
// material's note.
var legacyDescHeaders = map[string]bool{
	"mota": true, "ghichu": true, "description": true, "note": true, "desc": true,
}

// legacyQuotaHeaders: the quota column of the OLD template. It is no longer
// read — the importer only names it in a notice so an old file doesn't fail
// silently.
var legacyQuotaHeaders = map[string]bool{
	"dinhmuc": true, "dinhmucsanxuat": true, "dinhmucnvl": true,
	"quota": true, "productsperunit": true, "capacity": true,
	"sanphamdonvi": true, "spdonvi": true,
}

// MaterialImportRow is one parsed row: a material name, its sheet size in mm
// (nil = blank) and an optional description.
type MaterialImportRow struct {
	RowNumber   int      `json:"row_number"`
	Material    string   `json:"material"`
	LengthMM    *float64 `json:"length_mm"`
	WidthMM     *float64 `json:"width_mm"`
	Description string   `json:"description"`
}

type MaterialImportItem struct {
	Name               string   `json:"name"`
	Code               string   `json:"code"`
	Exists             bool     `json:"exists"`
	MaterialID         *uint    `json:"material_id"` // the existing material this row updates
	CurrentLengthMM    *float64 `json:"current_length_mm"`
	CurrentWidthMM     *float64 `json:"current_width_mm"`
	LengthMM           *float64 `json:"length_mm"` // size from file (nil = blank → leave as is)
	WidthMM            *float64 `json:"width_mm"`
	CurrentDescription string   `json:"current_description"`
	Description        string   `json:"description"` // description from file (blank = leave as-is)
	Action             string   `json:"action"`      // CREATE | UPDATE | NOCHANGE
	RowNumbers         []int    `json:"row_numbers"`
}

type MaterialImportRowError struct {
	RowNumber int    `json:"row_number"`
	Material  string `json:"material"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	// Every file row this error covers. A conflict is caused by two or more rows
	// at once, so reporting only RowNumber would send the user to fix one half of
	// a disagreement; the preview highlights all of them.
	RowNumbers []int `json:"row_numbers,omitempty"`
}

type MaterialImportSummary struct {
	TotalRows    int `json:"total_rows"`
	NewMaterials int `json:"new_materials"`
	Updates      int `json:"updates"` // materials whose size and/or description changed
	Unchanged    int `json:"unchanged"`
	ErrorRows    int `json:"error_rows"`
	// DuplicateRows counts the rows folded away because an earlier row already
	// carried the same material with the same data.
	DuplicateRows int `json:"duplicate_rows"`
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
	// Notices are file-level remarks that are not errors — e.g. an old-template
	// "Định mức" column that is ignored now.
	Notices []string               `json:"notices,omitempty"`
	Applied *MaterialImportApplied `json:"applied,omitempty"`
}

// ---------- Parsing ----------

// ParseMaterialImportFile parses a CSV/XLSX stream into MaterialImportRows,
// auto-detecting the `Loại VL`, `Dài (mm)` / `Rộng (mm)` (or a combined
// `D x R`) and `Mô tả` columns. Rows whose size cells aren't millimetres are
// returned as parse errors (never silently dropped). The notices name columns
// of the old template that are ignored now.
func ParseMaterialImportFile(source string, r io.Reader) ([]MaterialImportRow, []MaterialImportRowError, []string, error) {
	records, err := readLegacyGrid(source, r)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(records) < 2 {
		return nil, nil, nil, apperr.BadRequest("File phải có dòng tiêu đề và ít nhất một dòng dữ liệu")
	}
	header := records[0]
	matIdx, lenIdx, widIdx, sizeIdx, descIdx := -1, -1, -1, -1, -1
	var notices []string
	for i, h := range header {
		n := normalizeLegacyHeader(h)
		d := dimHeaderKey(h)
		switch {
		case matIdx == -1 && legacyMaterialHeaders[n]:
			matIdx = i
		case lenIdx == -1 && legacyLengthHeaders[d]:
			lenIdx = i
		case widIdx == -1 && legacyWidthHeaders[d]:
			widIdx = i
		case sizeIdx == -1 && legacySizeHeaders[d]:
			sizeIdx = i
		case descIdx == -1 && legacyDescHeaders[n]:
			descIdx = i
		case legacyQuotaHeaders[n]:
			notices = append(notices, fmt.Sprintf("Cột \"%s\" bị bỏ qua — định mức là của từng cặp SKU – NVL, khai ở cột \"Định mức\" của file SKU (Master Data → SKU → Import SKU con), không ở file NVL", strings.TrimSpace(h)))
		}
	}
	if matIdx == -1 {
		return nil, nil, nil, apperr.BadRequest("Không tìm thấy cột 'Loại VL' trong file — kiểm tra dòng tiêu đề")
	}
	if lenIdx == -1 && widIdx == -1 && sizeIdx == -1 {
		return nil, nil, nil, apperr.BadRequest("Không tìm thấy cột 'Dài (mm)' / 'Rộng (mm)' trong file — kiểm tra dòng tiêu đề")
	}
	cell := func(rec []string, idx int) string {
		if idx >= 0 && idx < len(rec) {
			return strings.TrimSpace(rec[idx])
		}
		return ""
	}

	var rows []MaterialImportRow
	var parseErrors []MaterialImportRowError
	for di, rec := range records[1:] {
		rowNum := di + 1
		name := cell(rec, matIdx)
		rawL, rawW, rawSize, desc := cell(rec, lenIdx), cell(rec, widIdx), cell(rec, sizeIdx), cell(rec, descIdx)
		if name == "" && rawL == "" && rawW == "" && rawSize == "" && desc == "" {
			continue // blank line
		}
		length, width, msg := parseDims(rawL, rawW, rawSize)
		if msg != "" {
			parseErrors = append(parseErrors, MaterialImportRowError{
				RowNumber: rowNum, Material: name, ErrorCode: errDimInvalid, Message: msg,
			})
			continue
		}
		rows = append(rows, MaterialImportRow{RowNumber: rowNum, Material: name, LengthMM: length, WidthMM: width, Description: desc})
	}
	return rows, parseErrors, notices, nil
}

// ---------- Analysis ----------

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

// analyzeMaterialImport folds the rows of each material together, checks them
// against each other and against the catalog, and derives the per-material
// action. Takes the repo so commit can run it on its transaction.
func analyzeMaterialImport(repo *repositories.Repositories, rows []MaterialImportRow, parseErrors []MaterialImportRowError) (MaterialImportPreview, error) {
	type agg struct {
		name, desc    string
		length, width *float64
		rowNums       []int
		conflict      string // what the rows disagree on ("" = consistent)
	}
	byName := map[string]*agg{}
	var order []string
	errs := append([]MaterialImportRowError{}, parseErrors...)
	total, duplicates := 0, 0

	for _, r := range rows {
		name := strings.TrimSpace(r.Material)
		desc := strings.TrimSpace(r.Description)
		hasSize := r.LengthMM != nil || r.WidthMM != nil
		if name == "" {
			if hasSize || desc != "" {
				errs = append(errs, MaterialImportRowError{
					RowNumber: r.RowNumber, ErrorCode: errMaterialBlank,
					Message: "Dòng có dữ liệu nhưng thiếu Loại VL",
				})
			}
			continue
		}
		total++
		if (r.LengthMM == nil) != (r.WidthMM == nil) {
			errs = append(errs, MaterialImportRowError{
				RowNumber: r.RowNumber, Material: name, ErrorCode: errDimInvalid,
				Message: "Cần cả Dài lẫn Rộng (mm) — hoặc để trống cả hai",
			})
			continue
		}
		key := strings.ToLower(name)
		a := byName[key]
		if a == nil {
			byName[key] = &agg{name: name, desc: desc, length: r.LengthMM, width: r.WidthMM, rowNums: []int{r.RowNumber}}
			order = append(order, key)
			continue
		}
		a.rowNums = append(a.rowNums, r.RowNumber)
		// A repeat may fill a blank an earlier line left, never contradict it.
		switch {
		case r.LengthMM == nil:
		case a.length == nil:
			a.length, a.width = r.LengthMM, r.WidthMM
		case !dimEqual(a.length, r.LengthMM) || !dimEqual(a.width, r.WidthMM):
			a.conflict = "kích thước"
		}
		switch {
		case desc == "":
		case a.desc == "":
			a.desc = desc
		case !strings.EqualFold(a.desc, desc):
			if a.conflict == "" {
				a.conflict = "mô tả"
			}
		}
		if a.conflict == "" {
			duplicates++
		}
	}

	names := make([]string, 0, len(order))
	for _, key := range order {
		names = append(names, byName[key].name)
	}
	found, err := repo.Material.ListByNamesInsensitive(names)
	if err != nil {
		return MaterialImportPreview{}, err
	}
	catalog := map[string][]*models.Material{}
	for i := range found {
		m := &found[i]
		key := strings.ToLower(strings.TrimSpace(m.Name))
		catalog[key] = append(catalog[key], m)
	}

	pv := MaterialImportPreview{}
	sum := MaterialImportSummary{TotalRows: total, DuplicateRows: duplicates}
	for _, key := range order {
		a := byName[key]
		if a.conflict != "" {
			errs = append(errs, MaterialImportRowError{
				RowNumber: a.rowNums[0], Material: a.name, ErrorCode: errMaterialRowConflict, RowNumbers: a.rowNums,
				Message: fmt.Sprintf("Loại VL này lặp ở các dòng %s với %s khác nhau — sửa cho thống nhất", joinInts(a.rowNums), a.conflict),
			})
			continue
		}
		matches := catalog[key]
		if len(matches) > 1 {
			codes := make([]string, 0, len(matches))
			for _, m := range matches {
				codes = append(codes, m.Code)
			}
			errs = append(errs, MaterialImportRowError{
				RowNumber: a.rowNums[0], Material: a.name, ErrorCode: errMaterialAmbiguous, RowNumbers: a.rowNums,
				Message: fmt.Sprintf("Hệ thống đang có %d NVL cùng tên này (%s) — gộp hoặc đổi tên trước rồi import lại", len(matches), strings.Join(codes, ", ")),
			})
			continue
		}
		it := MaterialImportItem{
			Name: a.name, Code: materialCode(a.name), LengthMM: a.length, WidthMM: a.width,
			Description: a.desc, RowNumbers: a.rowNums,
		}
		if len(matches) == 0 {
			it.Action = importActionCreate
			sum.NewMaterials++
		} else {
			m := matches[0]
			id := m.ID
			it.Exists, it.MaterialID = true, &id
			it.CurrentLengthMM, it.CurrentWidthMM, it.CurrentDescription = m.LengthMM, m.WidthMM, m.Description
			// Blank cells never overwrite: a blank size/description only takes effect
			// when creating a brand-new material, never to clear an existing value.
			sizeChange := a.length != nil && (!dimEqual(m.LengthMM, a.length) || !dimEqual(m.WidthMM, a.width))
			descChange := a.desc != "" && a.desc != m.Description
			if sizeChange || descChange {
				it.Action = importActionUpdate
				sum.Updates++
			} else {
				it.Action = importActionNoChange
				sum.Unchanged++
			}
		}
		pv.Items = append(pv.Items, it)
	}

	sum.ErrorRows = len(errs)
	pv.Errors = errs
	pv.Summary = sum
	return pv, nil
}

// ---------- Preview / Commit ----------

// PreviewMaterialImport analyses rows and returns the plan (nothing is written).
func (s *CatalogService) PreviewMaterialImport(filename string, rows []MaterialImportRow, parseErrors []MaterialImportRowError, notices []string) (*MaterialImportPreview, error) {
	pv, err := analyzeMaterialImport(s.repo, rows, parseErrors)
	if err != nil {
		return nil, apperr.Internal("could not read materials").Wrap(err)
	}
	pv.Filename = filename
	pv.Notices = notices
	return &pv, nil
}

// CommitMaterialImport applies the plan: find-or-create each material and set
// its sheet size / description. Additive and non-destructive — a blank cell
// never clears an existing value (those rows resolve to NOCHANGE in analyze).
// The rows are re-analysed inside the transaction so what is applied is what
// the catalog looks like now, not at preview time.
func (s *CatalogService) CommitMaterialImport(actor Actor, rows []MaterialImportRow) (*MaterialImportPreview, error) {
	var pv MaterialImportPreview
	applied := MaterialImportApplied{}
	err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		var err error
		if pv, err = analyzeMaterialImport(txRepo, rows, nil); err != nil {
			return err
		}
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
			case importActionCreate:
				toCreate = append(toCreate, models.Material{
					Code: mintMaterialCode(taken, it.Code), Name: it.Name,
					Description: it.Description, LengthMM: it.LengthMM, WidthMM: it.WidthMM,
				})
				applied.Created++
			case importActionUpdate:
				// By ID: analyze already decided which catalog row this line is. Only
				// the columns the file actually fills are written — a blank cell never
				// clears a value.
				if it.MaterialID == nil {
					continue
				}
				fields := map[string]any{}
				if it.LengthMM != nil {
					fields["length_mm"] = *it.LengthMM
					fields["width_mm"] = *it.WidthMM
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
		fmt.Sprintf("Material size import: %d created, %d updated", applied.Created, applied.Updated), nil)
	return &pv, nil
}

// ---------- Sample template ----------

// Mô tả is optional — the importer works with or without it.
var materialTemplateHeaders = []string{"Loại VL", "Dài (mm)", "Rộng (mm)", "Mô tả"}

var materialTemplateSample = [][]string{
	{"Mica trong 3 ly", "1220", "2440", "Tấm mica trong suốt 3mm"},
	{"Basswood 5mm", "600", "900", ""},                      // mô tả để trống cũng được
	{"Chỉ móc thẻ kẹp", "", "", "Phụ kiện, không tính tấm"}, // kích thước trống = không giới hạn
}

// MaterialTemplateXLSX renders the material import sample as a real .xlsx.
func (s *CatalogService) MaterialTemplateXLSX() ([]byte, string, error) {
	grid := append([][]string{materialTemplateHeaders}, materialTemplateSample...)
	data, err := buildTemplateXLSX("Kích thước NVL", grid, []float64{28, 12, 12, 32})
	if err != nil {
		return nil, "", err
	}
	return data, "material-import-template.xlsx", nil
}
