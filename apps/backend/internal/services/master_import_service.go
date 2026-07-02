package services

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/xuri/excelize/v2"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// MasterImportService seeds master data (Materials, SKUs, SKU↔Material mapping)
// from the factory's existing operational spreadsheet. It reuses the two columns
// that already exist in that file — `SKU` and `Loại VL` — and never invents a
// material: a SKU with no Loại VL is flagged "missing material" and a SKU seen
// with several different Loại VL is flagged "needs review" (never auto-merged).
type MasterImportService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// LegacyRow is one parsed spreadsheet row reduced to the two fields we care about
// for master-data setup. RowNumber is 1-based across data rows.
type LegacyRow struct {
	RowNumber int    `json:"row_number"`
	SKU       string `json:"sku"`
	Material  string `json:"material"`
}

// SKU status codes surfaced in the preview.
const (
	skuStatusOK       = "OK"               // exactly one Loại VL → will map
	skuStatusReview   = "NEEDS_REVIEW"     // several distinct Loại VL → do not merge
	skuStatusMissing  = "MISSING_MATERIAL" // SKU present but no Loại VL anywhere
	errSKUMissing     = "SKU_MISSING"
	mappingSourceNote = "Từ import vận hành cũ"
)

// ---------- Preview / plan structures (also stored in MasterImportJob.Plan) ----------

type MaterialPlan struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
}

type SKUPlan struct {
	Code          string   `json:"code"`
	Name          string   `json:"name"`
	Exists        bool     `json:"exists"`
	MaterialNames []string `json:"material_names"`
	Status        string   `json:"status"`
	RowCount      int      `json:"row_count"`
}

type MappingPlan struct {
	SKUCode      string `json:"sku_code"`
	MaterialCode string `json:"material_code"`
	MaterialName string `json:"material_name"`
	Exists       bool   `json:"exists"`
}

type LegacyRowError struct {
	RowNumber int    `json:"row_number"`
	SKU       string `json:"sku"`
	Material  string `json:"material"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

type MasterImportSummary struct {
	TotalRows    int `json:"total_rows"`
	NewMaterials int `json:"new_materials"`
	NewSKUs      int `json:"new_skus"`
	NewMappings  int `json:"new_mappings"`
	ReviewCount  int `json:"review_count"`
	MissingCount int `json:"missing_count"`
	ErrorRows    int `json:"error_rows"`
}

type MasterImportApplied struct {
	MaterialsCreated int `json:"materials_created"`
	SKUsCreated      int `json:"skus_created"`
	MappingsCreated  int `json:"mappings_created"`
}

// MasterImportPreview is returned to the client and also persisted (as Plan) so a
// PREVIEW can be COMMITTED later without re-uploading the file.
type MasterImportPreview struct {
	ImportJobID uint                   `json:"import_job_id"`
	Status      models.ImportJobStatus `json:"status"`
	Filename    string                 `json:"filename"`
	Materials   []MaterialPlan         `json:"materials"`
	SKUs        []SKUPlan              `json:"skus"`
	Mappings    []MappingPlan          `json:"mappings"`
	Errors      []LegacyRowError       `json:"errors"`
	Summary     MasterImportSummary    `json:"summary"`
	Applied     *MasterImportApplied   `json:"applied,omitempty"`
}

// ---------- Parsing ----------

// legacy header aliases (normalized: diacritics stripped, lowercased, no spaces).
var legacySKUHeaders = map[string]bool{
	"sku": true, "masku": true, "skucode": true,
}

var legacyMaterialHeaders = map[string]bool{
	"loaivl": true, "loaivatlieu": true, "loainguyenvatlieu": true,
	"loainvl": true, "nvl": true, "vatlieu": true, "material": true, "chatlieu": true,
}

// ParseLegacyFile parses a CSV or XLSX stream into LegacyRows, auto-detecting the
// SKU and Loại VL columns. source is "XLSX" or "CSV".
func ParseLegacyFile(source string, r io.Reader) ([]LegacyRow, error) {
	records, err := readLegacyGrid(source, r)
	if err != nil {
		return nil, err
	}
	return legacyRowsFromGrid(records)
}

func readLegacyGrid(source string, r io.Reader) ([][]string, error) {
	if source == "XLSX" {
		f, err := excelize.OpenReader(r)
		if err != nil {
			return nil, apperr.BadRequest("Không đọc được file XLSX: " + err.Error())
		}
		defer f.Close()
		sheets := f.GetSheetList()
		if len(sheets) == 0 {
			return nil, apperr.BadRequest("File XLSX không có worksheet nào")
		}
		rows, err := f.GetRows(sheets[0])
		if err != nil {
			return nil, apperr.BadRequest("Không đọc được dòng trong XLSX: " + err.Error())
		}
		return rows, nil
	}
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, apperr.BadRequest("Không đọc được CSV: " + err.Error())
	}
	return records, nil
}

func legacyRowsFromGrid(records [][]string) ([]LegacyRow, error) {
	if len(records) < 2 {
		return nil, apperr.BadRequest("File phải có dòng tiêu đề và ít nhất một dòng dữ liệu")
	}
	header := records[0]
	skuIdx, matIdx := -1, -1
	for i, h := range header {
		n := normalizeLegacyHeader(h)
		if skuIdx == -1 && legacySKUHeaders[n] {
			skuIdx = i
		}
		if matIdx == -1 && legacyMaterialHeaders[n] {
			matIdx = i
		}
	}
	if skuIdx == -1 {
		return nil, apperr.BadRequest("Không tìm thấy cột 'SKU' trong file — kiểm tra lại dòng tiêu đề")
	}
	// matIdx == -1 is allowed: the file has no 'Loại VL' column, so every SKU will
	// be flagged MISSING_MATERIAL (we never guess a material).
	rows := make([]LegacyRow, 0, len(records)-1)
	for di, rec := range records[1:] {
		lr := LegacyRow{RowNumber: di + 1}
		if skuIdx < len(rec) {
			lr.SKU = strings.TrimSpace(rec[skuIdx])
		}
		if matIdx >= 0 && matIdx < len(rec) {
			lr.Material = strings.TrimSpace(rec[matIdx])
		}
		rows = append(rows, lr)
	}
	return rows, nil
}

// ---------- Analysis ----------

type skuAgg struct {
	code      string
	name      string
	matNames  []string
	rowCount  int
	firstSeen int
}

// analyze groups the file rows by SKU and by material and derives the full plan.
func (s *MasterImportService) analyze(rows []LegacyRow) *MasterImportPreview {
	skuMap := map[string]*skuAgg{}
	var skuOrder []string
	var matOrder []string
	matSeen := map[string]bool{}
	var rowErrors []LegacyRowError
	total := 0

	for _, r := range rows {
		sku := strings.TrimSpace(r.SKU)
		mat := strings.TrimSpace(r.Material)
		if sku == "" && mat == "" {
			continue // blank line — ignore silently
		}
		total++
		if sku == "" {
			rowErrors = append(rowErrors, LegacyRowError{
				RowNumber: r.RowNumber, Material: mat, ErrorCode: errSKUMissing,
				Message: "Dòng có Loại VL nhưng thiếu SKU",
			})
			continue
		}
		code := normalizeSKUCode(sku)
		agg := skuMap[code]
		if agg == nil {
			agg = &skuAgg{code: code, name: sku, firstSeen: len(skuOrder)}
			skuMap[code] = agg
			skuOrder = append(skuOrder, code)
		}
		agg.rowCount++
		if mat != "" {
			lm := strings.ToLower(mat)
			if !containsFold(agg.matNames, mat) {
				agg.matNames = append(agg.matNames, mat)
			}
			if !matSeen[lm] {
				matSeen[lm] = true
				matOrder = append(matOrder, mat)
			}
		}
	}

	pv := &MasterImportPreview{Errors: rowErrors}
	sum := MasterImportSummary{TotalRows: total, ErrorRows: len(rowErrors)}

	// Materials plan.
	for _, name := range matOrder {
		exists := false
		if m, _ := s.repo.Material.FindByNameInsensitive(name); m != nil {
			exists = true
		}
		if !exists {
			sum.NewMaterials++
		}
		pv.Materials = append(pv.Materials, MaterialPlan{Code: materialCode(name), Name: name, Exists: exists})
	}

	// SKU + mapping plan.
	for _, code := range skuOrder {
		agg := skuMap[code]
		skuExists := false
		var skuRec *models.SKU
		if rec, err := s.repo.SKU.FindByCode(code); err == nil {
			skuExists = true
			skuRec = rec
		}
		if !skuExists {
			sum.NewSKUs++
		}

		status := skuStatusOK
		switch len(agg.matNames) {
		case 0:
			status = skuStatusMissing
			sum.MissingCount++
		case 1:
			status = skuStatusOK
		default:
			status = skuStatusReview
			sum.ReviewCount++
		}

		pv.SKUs = append(pv.SKUs, SKUPlan{
			Code: agg.code, Name: agg.name, Exists: skuExists,
			MaterialNames: agg.matNames, Status: status, RowCount: agg.rowCount,
		})

		// Only single-material SKUs produce an auto mapping.
		if status == skuStatusOK {
			matName := agg.matNames[0]
			exists := false
			if skuExists && skuRec != nil {
				if matRec, _ := s.repo.Material.FindByNameInsensitive(matName); matRec != nil {
					if ok, _ := s.repo.SKU.MappingExists(skuRec.ID, matRec.ID); ok {
						exists = true
					}
				}
			}
			if !exists {
				sum.NewMappings++
			}
			pv.Mappings = append(pv.Mappings, MappingPlan{
				SKUCode: agg.code, MaterialCode: materialCode(matName), MaterialName: matName, Exists: exists,
			})
		}
	}

	pv.Summary = sum
	return pv
}

// ---------- Preview / Commit ----------

// Preview analyses the rows, persists a MasterImportJob in PREVIEW state and
// returns the plan. Nothing is written to the catalog tables yet.
func (s *MasterImportService) Preview(actor Actor, source, filename string, rows []LegacyRow) (*MasterImportPreview, error) {
	if len(rows) == 0 {
		return nil, apperr.BadRequest("Không có dòng dữ liệu để phân tích")
	}
	pv := s.analyze(rows)
	pv.Filename = filename

	raw, err := models.ToJSONB(pv)
	if err != nil {
		return nil, apperr.Internal("could not serialize plan").Wrap(err)
	}
	job := &models.MasterImportJob{
		Filename: filename, Source: source, Status: models.ImportPreview,
		TotalRows:    pv.Summary.TotalRows,
		NewMaterials: pv.Summary.NewMaterials,
		NewSKUs:      pv.Summary.NewSKUs,
		NewMappings:  pv.Summary.NewMappings,
		ReviewCount:  pv.Summary.ReviewCount,
		MissingCount: pv.Summary.MissingCount,
		ErrorRows:    pv.Summary.ErrorRows,
		Plan:         raw,
		CreatedByID:  actor.IDPtr(),
	}
	if err := s.repo.MasterImport.Create(job); err != nil {
		return nil, apperr.Internal("could not create master import job").Wrap(err)
	}
	pv.ImportJobID = job.ID
	pv.Status = job.Status

	s.audit.Log(actor, "MASTER_IMPORT_PREVIEW", "master_import_job", &job.ID,
		fmt.Sprintf("Preview legacy master data: %d rows, %d new materials, %d new SKUs, %d new mappings, %d review, %d missing, %d errors",
			pv.Summary.TotalRows, pv.Summary.NewMaterials, pv.Summary.NewSKUs, pv.Summary.NewMappings,
			pv.Summary.ReviewCount, pv.Summary.MissingCount, pv.Summary.ErrorRows), nil)
	return pv, nil
}

// Commit applies a PREVIEW job's stored plan: it find-or-creates the materials,
// find-or-creates the SKUs and adds the single-material mappings. It is additive
// and never removes an existing material from a SKU.
func (s *MasterImportService) Commit(actor Actor, jobID uint) (*MasterImportPreview, error) {
	job, err := s.repo.MasterImport.FindByID(jobID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Master import job not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	if job.Status != models.ImportPreview {
		return nil, apperr.Conflict("Import job is not in PREVIEW state")
	}

	var pv MasterImportPreview
	if len(job.Plan) > 0 {
		if err := json.Unmarshal(job.Plan, &pv); err != nil {
			return nil, apperr.Internal("could not read stored plan").Wrap(err)
		}
	}

	applied := MasterImportApplied{}
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		matIDByName := map[string]uint{}
		for _, m := range pv.Materials {
			rec, err := txRepo.Material.FindByNameInsensitive(m.Name)
			if err != nil {
				return err
			}
			if rec == nil {
				code := uniqueMaterialCode(txRepo, m.Code)
				rec = &models.Material{Code: code, Name: m.Name}
				if err := txRepo.Material.Create(rec); err != nil {
					return err
				}
				applied.MaterialsCreated++
			}
			matIDByName[strings.ToLower(strings.TrimSpace(m.Name))] = rec.ID
		}

		skuIDByCode := map[string]uint{}
		for _, sp := range pv.SKUs {
			rec, err := txRepo.SKU.FindByCode(sp.Code)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				rec = &models.SKU{Code: sp.Code, Name: sp.Name, ProductName: sp.Name, IsActive: true}
				if err := txRepo.SKU.Create(rec); err != nil {
					return err
				}
				applied.SKUsCreated++
			} else if err != nil {
				return err
			}
			skuIDByCode[sp.Code] = rec.ID
		}

		for _, mp := range pv.Mappings {
			skuID := skuIDByCode[mp.SKUCode]
			matID := matIDByName[strings.ToLower(strings.TrimSpace(mp.MaterialName))]
			if skuID == 0 || matID == 0 {
				continue
			}
			exists, err := txRepo.SKU.MappingExists(skuID, matID)
			if err != nil {
				return err
			}
			if !exists {
				if err := txRepo.SKU.AddMaterial(skuID, matID, 1, mappingSourceNote); err != nil {
					return err
				}
				applied.MappingsCreated++
			}
		}

		job.Status = models.ImportCommitted
		job.MaterialsCreated = applied.MaterialsCreated
		job.SKUsCreated = applied.SKUsCreated
		job.MappingsCreated = applied.MappingsCreated
		return tx.Save(job).Error
	})
	if err != nil {
		return nil, apperr.Internal("could not commit master import").Wrap(err)
	}

	pv.ImportJobID = job.ID
	pv.Status = job.Status
	pv.Applied = &applied

	s.audit.Log(actor, "MASTER_IMPORT_COMMIT", "master_import_job", &job.ID,
		fmt.Sprintf("Committed legacy master data: created %d materials, %d SKUs, %d mappings",
			applied.MaterialsCreated, applied.SKUsCreated, applied.MappingsCreated), nil)
	return &pv, nil
}

// Get returns the stored plan for a job (reconstructed from Plan).
func (s *MasterImportService) Get(id uint) (*MasterImportPreview, error) {
	job, err := s.repo.MasterImport.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Master import job not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	var pv MasterImportPreview
	if len(job.Plan) > 0 {
		if err := json.Unmarshal(job.Plan, &pv); err != nil {
			return nil, apperr.Internal("could not read stored plan").Wrap(err)
		}
	}
	pv.ImportJobID = job.ID
	pv.Status = job.Status
	pv.Filename = job.Filename
	if job.Status == models.ImportCommitted {
		pv.Applied = &MasterImportApplied{
			MaterialsCreated: job.MaterialsCreated,
			SKUsCreated:      job.SKUsCreated,
			MappingsCreated:  job.MappingsCreated,
		}
	}
	return &pv, nil
}

func (s *MasterImportService) List(page repositories.Page) ([]models.MasterImportJob, int64, error) {
	return s.repo.MasterImport.List(page.Normalize())
}

// ---------- helpers ----------

// normalizeSKUCode mirrors the transformation the order importer applies when it
// looks up a SKU, so a SKU created here always matches an order-file SKU.
func normalizeSKUCode(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// materialCode builds a stable, uppercase, ASCII code from a (possibly Vietnamese)
// material name, e.g. "Mica trong 3 ly" → "MICA-TRONG-3-LY".
func materialCode(name string) string {
	ascii := removeVietnameseDiacritics(strings.ToLower(strings.TrimSpace(name)))
	var b strings.Builder
	prevDash := false
	for _, r := range ascii {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(unicode.ToUpper(r))
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	code := strings.Trim(b.String(), "-")
	if len(code) > 32 {
		code = strings.TrimRight(code[:32], "-")
	}
	if code == "" {
		code = "MAT"
	}
	return code
}

// uniqueMaterialCode returns base, or base-2/base-3/... if the code is taken.
func uniqueMaterialCode(repo *repositories.Repositories, base string) string {
	if base == "" {
		base = "MAT"
	}
	code := base
	for i := 2; ; i++ {
		if _, err := repo.Material.FindByCode(code); err != nil {
			return code // not found → available
		}
		suffix := "-" + strconv.Itoa(i)
		trimTo := 32 - len(suffix)
		b := base
		if len(b) > trimTo {
			b = strings.TrimRight(b[:trimTo], "-")
		}
		code = b + suffix
	}
}

func normalizeLegacyHeader(h string) string {
	h = strings.TrimPrefix(h, string(rune(0xFEFF))) // UTF-8 BOM from Excel exports
	h = removeVietnameseDiacritics(strings.ToLower(strings.TrimSpace(h)))
	return strings.NewReplacer(" ", "", "_", "", "-", "", ".", "", "/", "").Replace(h)
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

// viDiacritics maps lowercase Vietnamese vowels/consonants to ASCII. Callers must
// lowercase first.
var viDiacritics = strings.NewReplacer(
	"à", "a", "á", "a", "ả", "a", "ã", "a", "ạ", "a",
	"ă", "a", "ằ", "a", "ắ", "a", "ẳ", "a", "ẵ", "a", "ặ", "a",
	"â", "a", "ầ", "a", "ấ", "a", "ẩ", "a", "ẫ", "a", "ậ", "a",
	"è", "e", "é", "e", "ẻ", "e", "ẽ", "e", "ẹ", "e",
	"ê", "e", "ề", "e", "ế", "e", "ể", "e", "ễ", "e", "ệ", "e",
	"ì", "i", "í", "i", "ỉ", "i", "ĩ", "i", "ị", "i",
	"ò", "o", "ó", "o", "ỏ", "o", "õ", "o", "ọ", "o",
	"ô", "o", "ồ", "o", "ố", "o", "ổ", "o", "ỗ", "o", "ộ", "o",
	"ơ", "o", "ờ", "o", "ớ", "o", "ở", "o", "ỡ", "o", "ợ", "o",
	"ù", "u", "ú", "u", "ủ", "u", "ũ", "u", "ụ", "u",
	"ư", "u", "ừ", "u", "ứ", "u", "ử", "u", "ữ", "u", "ự", "u",
	"ỳ", "y", "ý", "y", "ỷ", "y", "ỹ", "y", "ỵ", "y",
	"đ", "d",
)

func removeVietnameseDiacritics(s string) string {
	return viDiacritics.Replace(s)
}
