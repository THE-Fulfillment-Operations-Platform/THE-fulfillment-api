package services

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
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
// material: a SKU with no Loại VL is flagged "missing material".
//
// A single "Loại VL" cell may list several materials joined by "+" (or line
// breaks inside the cell), e.g. "Mica trong 3 ly + Basswood 5mm" — that is a
// combo SKU built from all of them, so every part is created and mapped and the
// SKU is flagged IsCombo. When the same SKU is spelled with *different* material
// sets across rows the plan still maps their union but flags "needs review" so a
// human can double-check an inconsistent file (nothing is dropped or merged away).
type MasterImportService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// LegacyRow is one parsed spreadsheet row reduced to the fields we care about for
// master-data setup. RowNumber is 1-based across data rows. ProductName is the
// human-readable product name ("Tên sản phẩm") — optional; older files omit it.
type LegacyRow struct {
	RowNumber   int    `json:"row_number"`
	SKU         string `json:"sku"`
	Material    string `json:"material"`
	ProductName string `json:"product_name"`
}

// SKU status codes surfaced in the preview.
const (
	skuStatusOK       = "OK"               // consistent material set → will map (single or combo)
	skuStatusReview   = "NEEDS_REVIEW"     // rows disagree on the set → mapped as union, double-check
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
	ProductName   string   `json:"product_name"`            // representative human-readable name (first seen)
	ProductNames  []string `json:"product_names,omitempty"` // all distinct names for this SKU (one spec can label many products)
	Exists        bool     `json:"exists"`
	MaterialNames []string `json:"material_names"`
	Status        string   `json:"status"`
	RowCount      int      `json:"row_count"`
	IsCombo       bool     `json:"is_combo"` // built from ≥2 materials (BOM)
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

// legacyProductHeaders: the human-readable product name column ("Tên sản phẩm").
// Optional — a file without it behaves exactly as before (product name falls back
// to the SKU display name on create).
var legacyProductHeaders = map[string]bool{
	"tensanpham": true, "tensp": true, "tenhienthi": true, "tensanphamhienthi": true,
	"sanpham": true, "productname": true, "product": true,
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
	skuIdx, matIdx, prodIdx := -1, -1, -1
	for i, h := range header {
		n := normalizeLegacyHeader(h)
		if skuIdx == -1 && legacySKUHeaders[n] {
			skuIdx = i
		}
		if matIdx == -1 && legacyMaterialHeaders[n] {
			matIdx = i
		}
		if prodIdx == -1 && legacyProductHeaders[n] {
			prodIdx = i
		}
	}
	if skuIdx == -1 {
		return nil, apperr.BadRequest("Không tìm thấy cột 'SKU' trong file — kiểm tra lại dòng tiêu đề")
	}
	// matIdx == -1 is allowed: the file has no 'Loại VL' column, so every SKU will
	// be flagged MISSING_MATERIAL (we never guess a material). prodIdx == -1 is also
	// allowed: no 'Tên sản phẩm' column → no product name captured.
	rows := make([]LegacyRow, 0, len(records)-1)
	for di, rec := range records[1:] {
		lr := LegacyRow{RowNumber: di + 1}
		if skuIdx < len(rec) {
			lr.SKU = strings.TrimSpace(rec[skuIdx])
		}
		if matIdx >= 0 && matIdx < len(rec) {
			lr.Material = strings.TrimSpace(rec[matIdx])
		}
		if prodIdx >= 0 && prodIdx < len(rec) {
			lr.ProductName = strings.TrimSpace(rec[prodIdx])
		}
		rows = append(rows, lr)
	}
	return rows, nil
}

// ---------- Analysis ----------

type skuAgg struct {
	code         string
	name         string
	matNames     []string        // union of every material seen for this SKU, first-seen order
	productNames []string        // distinct human-readable product names seen, first-seen order
	rowSigs      map[string]bool // distinct per-row material-set signatures (blank rows excluded)
	rowCount     int
	firstSeen    int
}

// catalogSnapshot is the slice of the catalog one import file touches, read up
// front in a fixed number of queries. Looking each name/code up as the plan is
// built cost one query per SKU (three, with the material preload) — a 200-row
// file meant ~600 round-trips, which on a hosted database is a minute of waiting.
type catalogSnapshot struct {
	matByName map[string]*models.Material // key: lower(trim(name))
	skuByCode map[string]*models.SKU      // key: normalized code
	mappings  map[[2]uint]bool            // (skuID, materialID) pairs already stored
}

func (c *catalogSnapshot) material(name string) *models.Material {
	return c.matByName[strings.ToLower(strings.TrimSpace(name))]
}
func (c *catalogSnapshot) sku(code string) *models.SKU {
	return c.skuByCode[models.NormalizeCode(code)]
}

func (s *MasterImportService) loadCatalog(matNames, skuCodes []string) (*catalogSnapshot, error) {
	return loadCatalogSnapshot(s.repo, matNames, skuCodes)
}

// loadCatalogSnapshot reads every material name and SKU code the plan mentions,
// plus the mappings of the SKUs that already exist. Takes the repo so commit can
// run it on its transaction. Errors are returned, never swallowed: pretending the
// catalog is empty would report everything as new — and create it all again.
func loadCatalogSnapshot(repo *repositories.Repositories, matNames, skuCodes []string) (*catalogSnapshot, error) {
	snap := &catalogSnapshot{
		matByName: map[string]*models.Material{},
		skuByCode: map[string]*models.SKU{},
		mappings:  map[[2]uint]bool{},
	}

	mats, err := repo.Material.ListByNamesInsensitive(matNames)
	if err != nil {
		return nil, err
	}
	for i := range mats {
		m := &mats[i]
		key := strings.ToLower(strings.TrimSpace(m.Name))
		// Names are not unique any more (the quota import can keep same-name
		// variants apart); the oldest row wins, as FindByNameInsensitive did.
		if _, seen := snap.matByName[key]; !seen {
			snap.matByName[key] = m
		}
	}

	skus, err := repo.SKU.ListByCodes(skuCodes)
	if err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(skus))
	for i := range skus {
		sku := &skus[i]
		snap.skuByCode[models.NormalizeCode(sku.Code)] = sku
		ids = append(ids, sku.ID)
	}

	if len(ids) > 0 {
		pairs, err := repo.SKU.MappingsForSKUs(ids)
		if err != nil {
			return nil, err
		}
		for _, p := range pairs {
			snap.mappings[[2]uint{p.SKUID, p.MaterialID}] = true
		}
	}
	return snap, nil
}

// analyze groups the file rows by SKU and by material and derives the full plan.
func (s *MasterImportService) analyze(rows []LegacyRow) (*MasterImportPreview, error) {
	skuMap := map[string]*skuAgg{}
	var skuOrder []string
	var matOrder []string
	matSeen := map[string]bool{}
	var rowErrors []LegacyRowError
	total := 0

	for _, r := range rows {
		sku := strings.TrimSpace(r.SKU)
		// A cell may pack several materials for a combo SKU ("Mica + Basswood").
		mats := splitMaterials(r.Material)
		if sku == "" && len(mats) == 0 {
			continue // blank line — ignore silently
		}
		total++
		if sku == "" {
			rowErrors = append(rowErrors, LegacyRowError{
				RowNumber: r.RowNumber, Material: strings.TrimSpace(r.Material), ErrorCode: errSKUMissing,
				Message: "Dòng có Loại VL nhưng thiếu SKU",
			})
			continue
		}
		code := normalizeSKUCode(sku)
		agg := skuMap[code]
		if agg == nil {
			agg = &skuAgg{code: code, name: sku, rowSigs: map[string]bool{}, firstSeen: len(skuOrder)}
			skuMap[code] = agg
			skuOrder = append(skuOrder, code)
		}
		agg.rowCount++
		if pn := strings.TrimSpace(r.ProductName); pn != "" && !containsFold(agg.productNames, pn) {
			agg.productNames = append(agg.productNames, pn)
		}
		if len(mats) > 0 {
			agg.rowSigs[rowSignature(mats)] = true
			for _, name := range mats {
				if !containsFold(agg.matNames, name) {
					agg.matNames = append(agg.matNames, name)
				}
				lm := strings.ToLower(name)
				if !matSeen[lm] {
					matSeen[lm] = true
					matOrder = append(matOrder, name)
				}
			}
		}
	}

	pv := &MasterImportPreview{Errors: rowErrors}
	sum := MasterImportSummary{TotalRows: total, ErrorRows: len(rowErrors)}

	// The whole catalog lookup for this file, up front.
	snap, err := s.loadCatalog(matOrder, skuOrder)
	if err != nil {
		return nil, apperr.Internal("could not read catalog").Wrap(err)
	}

	// Materials plan.
	for _, name := range matOrder {
		exists := snap.material(name) != nil
		if !exists {
			sum.NewMaterials++
		}
		pv.Materials = append(pv.Materials, MaterialPlan{Code: materialCode(name), Name: name, Exists: exists})
	}

	// SKU + mapping plan.
	for _, code := range skuOrder {
		agg := skuMap[code]
		skuRec := snap.sku(code)
		skuExists := skuRec != nil
		if !skuExists {
			sum.NewSKUs++
		}

		// A SKU with no material at all is "missing"; otherwise it maps its full
		// material set (single or combo). We only flag "needs review" when the SKU's
		// rows *disagree* on the set — that is a data-entry inconsistency worth a
		// human glance, but we still map the union rather than silently dropping it.
		isCombo := len(agg.matNames) >= 2
		status := skuStatusOK
		switch {
		case len(agg.matNames) == 0:
			status = skuStatusMissing
			sum.MissingCount++
		case len(agg.rowSigs) > 1:
			status = skuStatusReview
			sum.ReviewCount++
		}

		productName := ""
		if len(agg.productNames) > 0 {
			productName = agg.productNames[0]
		}
		pv.SKUs = append(pv.SKUs, SKUPlan{
			Code: agg.code, Name: agg.name, ProductName: productName, ProductNames: agg.productNames,
			Exists: skuExists, MaterialNames: agg.matNames, Status: status, RowCount: agg.rowCount, IsCombo: isCombo,
		})

		// Every material of a non-missing SKU produces a mapping (a combo SKU maps
		// to all of its materials). Mappings that already exist are marked so.
		if status != skuStatusMissing {
			for _, matName := range agg.matNames {
				exists := false
				if skuRec != nil {
					if matRec := snap.material(matName); matRec != nil {
						exists = snap.mappings[[2]uint{skuRec.ID, matRec.ID}]
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
	}

	pv.Summary = sum
	return pv, nil
}

// ---------- Preview / Commit ----------

// Preview analyses the rows, persists a MasterImportJob in PREVIEW state and
// returns the plan. Nothing is written to the catalog tables yet.
func (s *MasterImportService) Preview(actor Actor, source, filename string, rows []LegacyRow) (*MasterImportPreview, error) {
	if len(rows) == 0 {
		return nil, apperr.BadRequest("Không có dòng dữ liệu để phân tích")
	}
	pv, err := s.analyze(rows)
	if err != nil {
		return nil, err
	}
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

		// The whole catalog this plan touches, in a fixed number of reads. Doing it
		// per row cost ~7 statements per SKU, which is minutes for a real file.
		matNames := make([]string, 0, len(pv.Materials))
		for _, m := range pv.Materials {
			matNames = append(matNames, m.Name)
		}
		skuCodes := make([]string, 0, len(pv.SKUs))
		for _, sp := range pv.SKUs {
			skuCodes = append(skuCodes, sp.Code)
		}
		snap, err := loadCatalogSnapshot(txRepo, matNames, skuCodes)
		if err != nil {
			return err
		}

		// ---- Materials: resolve, then insert the new ones in batches ----
		existingCodes, err := txRepo.Material.AllCodes()
		if err != nil {
			return err
		}
		taken := make(map[string]bool, len(existingCodes))
		for _, c := range existingCodes {
			taken[models.NormalizeCode(c)] = true
		}
		matIDByName := map[string]uint{}
		var newMats []models.Material
		for _, m := range pv.Materials {
			key := strings.ToLower(strings.TrimSpace(m.Name))
			if _, done := matIDByName[key]; done {
				continue
			}
			if rec := snap.material(m.Name); rec != nil {
				matIDByName[key] = rec.ID
				continue
			}
			matIDByName[key] = 0 // claimed by a pending insert; filled in below
			newMats = append(newMats, models.Material{Code: mintMaterialCode(taken, m.Code), Name: m.Name})
		}
		if err := txRepo.Material.CreateMany(newMats, materialInsertBatch); err != nil {
			return err
		}
		for i := range newMats {
			matIDByName[strings.ToLower(strings.TrimSpace(newMats[i].Name))] = newMats[i].ID
			applied.MaterialsCreated++
		}

		// ---- SKUs: resolve, batch-insert the new ones, batch the name refreshes ----
		skuIDByCode := map[string]uint{}
		var newSKUs []models.SKU
		// Existing SKUs whose product name the file refreshes, grouped by the new
		// value so identical names go out as one UPDATE ... WHERE id IN (...).
		renameIDs := map[string][]uint{}
		for _, sp := range pv.SKUs {
			if rec := snap.sku(sp.Code); rec != nil {
				skuIDByCode[sp.Code] = rec.ID
				// The file is the source of truth for the human-readable name. Scoped
				// update — never touches the material mapping.
				if sp.ProductName != "" && rec.ProductName != sp.ProductName {
					renameIDs[sp.ProductName] = append(renameIDs[sp.ProductName], rec.ID)
				}
				continue
			}
			// Product name comes from the file's "Tên sản phẩm" column; when the file
			// has no such column, fall back to the SKU display name (legacy behaviour).
			productName := sp.ProductName
			if productName == "" {
				productName = sp.Name
			}
			newSKUs = append(newSKUs, models.SKU{
				Code: sp.Code, Name: sp.Name, ProductName: productName, IsActive: true, IsCombo: sp.IsCombo,
			})
		}
		if err := txRepo.SKU.CreateMany(newSKUs, skuInsertBatch); err != nil {
			return err
		}
		for i := range newSKUs {
			skuIDByCode[newSKUs[i].Code] = newSKUs[i].ID
			applied.SKUsCreated++
		}
		for name, ids := range renameIDs {
			if err := tx.Model(&models.SKU{}).Where("id IN ?", ids).
				Update("product_name", name).Error; err != nil {
				return err
			}
		}

		// ---- Mappings: only the pairs that aren't stored yet, in batches ----
		var newMappings []models.SKUMaterial
		for _, mp := range pv.Mappings {
			skuID := skuIDByCode[mp.SKUCode]
			matID := matIDByName[strings.ToLower(strings.TrimSpace(mp.MaterialName))]
			if skuID == 0 || matID == 0 {
				continue
			}
			pair := [2]uint{skuID, matID}
			// snap.mappings holds what the catalog already had; adding as we go also
			// guards a plan that lists the same pair twice (the unique index would
			// reject the second row and fail the whole batch).
			if snap.mappings[pair] {
				continue
			}
			snap.mappings[pair] = true
			newMappings = append(newMappings, models.SKUMaterial{
				SKUID: skuID, MaterialID: matID, QuantityPerUnit: 1, Note: mappingSourceNote,
			})
		}
		if err := txRepo.SKU.AddMaterialsMany(newMappings, skuInsertBatch); err != nil {
			return err
		}
		applied.MappingsCreated = len(newMappings)

		// Flag as combo any SKU that ended up mapped to ≥2 materials. Counting the
		// real mappings (not just this file's plan) keeps it correct when an import
		// additively pushes an existing single-material SKU over into a combo. This
		// is upgrade-only: an additive import must never clear a manual combo flag.
		// Plan order, not map order, so the statement is reproducible.
		touched := make([]uint, 0, len(skuIDByCode))
		for _, sp := range pv.SKUs {
			if id := skuIDByCode[sp.Code]; id != 0 {
				touched = append(touched, id)
			}
		}
		counts, err := txRepo.SKU.MaterialCounts(touched)
		if err != nil {
			return err
		}
		combos := make([]uint, 0, len(touched))
		for _, id := range touched {
			if counts[id] >= 2 {
				combos = append(combos, id)
			}
		}
		if err := txRepo.SKU.MarkComboMany(combos); err != nil {
			return err
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
	return models.NormalizeCode(s)
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

// materialSplitRe splits a "Loại VL" cell into its component materials. A combo
// SKU lists several materials in one cell joined by "+", and Excel/CSV cells can
// also carry embedded line breaks — both act as separators. A material name must
// therefore never itself contain a "+".
var materialSplitRe = regexp.MustCompile(`[+\r\n]+`)

// splitMaterials turns one cell into its distinct, trimmed material names, e.g.
// "Mica trong 3 ly + Basswood 5mm" → ["Mica trong 3 ly", "Basswood 5mm"].
// Blanks are dropped and case-insensitive duplicates within the cell collapsed.
func splitMaterials(cell string) []string {
	parts := materialSplitRe.Split(cell, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || containsFold(out, p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// rowSignature is a stable key for the *set* of materials on a single row, used
// to detect when different rows of the same SKU disagree. Order-insensitive and
// case-insensitive: "A + B" and "b + a" produce the same signature.
func rowSignature(mats []string) string {
	norm := make([]string, len(mats))
	for i, m := range mats {
		norm[i] = strings.ToLower(strings.TrimSpace(m))
	}
	sort.Strings(norm)
	return strings.Join(norm, "|")
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

// ---------- Sample template download ----------

// masterTemplateHeaders are the columns the importer reads: the human-readable
// "Tên sản phẩm" (optional), "SKU" and "Loại VL". The file the factory uploads may
// carry many more columns — those are ignored — but the sample we hand back keeps
// just these so the required format is obvious.
var masterTemplateHeaders = []string{"Tên sản phẩm", "SKU", "Loại VL"}

// masterTemplateSample is a handful of example rows so the user can see exactly
// what a valid Tên sản phẩm / SKU / Loại VL row looks like before filling in their
// own. The last row shows a combo SKU: several materials in one cell joined by " + ".
var masterTemplateSample = [][]string{
	{"Kệ gỗ treo tường", "BRA-1.6-KEP", "Mica trong 3 ly"},
	{"Thớt gỗ khắc tên", "LWD-12IN", "Gỗ 5 ly 3 lớp"},
	{"Bảng tên để bàn", "NEW-SKU-X", "MDF 3 ly 80x120"},
	{"Đèn gỗ combo", "COMBO-A2-GAI", "Mica trong 3 ly + Mica Hologram"},
}

// MasterTemplateXLSX renders the master-data import sample as a real .xlsx
// workbook (Tên sản phẩm + SKU + Loại VL columns split cleanly in Excel on any
// locale, unlike the old comma CSV that opened as garbled single-column text).
func (s *MasterImportService) MasterTemplateXLSX() ([]byte, string, error) {
	grid := append([][]string{masterTemplateHeaders}, masterTemplateSample...)
	data, err := buildTemplateXLSX("Master data", grid, []float64{28, 24, 28})
	if err != nil {
		return nil, "", err
	}
	return data, "master-data-template.xlsx", nil
}

// skuInsertBatch is how many SKUs / mappings ride in one INSERT.
const skuInsertBatch = 200
