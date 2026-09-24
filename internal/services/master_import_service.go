package services

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
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
// material: a SKU with no Loại VL is flagged "missing material".
//
// A single "Loại VL" cell may list several materials joined by "+" (or line
// breaks inside the cell), e.g. "Mica trong 3 ly + Basswood 5mm" — that is a
// combo SKU built from all of them, so every part is created and mapped and the
// SKU is flagged IsCombo. When the same SKU is spelled with *different* material
// sets across rows, that SKU is refused (MATERIAL_CONFLICT) rather than mapped
// to the union: a product mapped to a material it isn't made of lands in that
// material's batch and gets cut from the wrong sheet.
type MasterImportService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// LegacyRow is one parsed spreadsheet row reduced to the fields we care about for
// master-data setup. RowNumber is 1-based across data rows. ProductName is the
// human-readable product name ("Tên sản phẩm") — optional; older files omit it.
//
// ParentSKU ("SKU cha") files the row's SKU under an EXISTING parent SKU — the
// parent is imported first (ImportParents), children after. Length/Width are the
// raw "D (mm)" / "R (mm)" cells and Size a combined "D x R" cell, kept raw so a
// bad number is reported against its own row. All optional: blank = unchanged.
type LegacyRow struct {
	RowNumber   int    `json:"row_number"`
	SKU         string `json:"sku"`
	Material    string `json:"material"`
	ProductName string `json:"product_name"`
	ParentSKU   string `json:"parent_sku"`
	Length      string `json:"length"`
	Width       string `json:"width"`
	Size        string `json:"size"`
	// Quota is the raw "Định mức" cell: the declared production quota of this
	// row's (SKU, Loại VL) pair — products per sheet, from the factory's layout
	// file. Blank = not declared (an existing number stays).
	Quota       string `json:"quota"`
	Description string `json:"description"`
}

// SKU status codes surfaced in the preview.
const (
	skuStatusOK       = "OK"               // will map its material set (single or combo)
	skuStatusMissing  = "MISSING_MATERIAL" // SKU present but no Loại VL anywhere
	errSKUMissing     = "SKU_MISSING"
	mappingSourceNote = "Từ import vận hành cũ"

	// Parent → child and size problems. Each drops the SKU from the plan: a child
	// filed under the wrong parent, or with a size nobody can vouch for, is worse
	// than a child that is simply not there yet.
	errMaterialConflict = "MATERIAL_CONFLICT" // the SKU's rows disagree on its material set
	errDimInvalid       = "DIM_INVALID"       // D / R cell is not a size in mm, or only one side given
	errDimConflict      = "DIM_CONFLICT"      // the SKU's rows disagree on D x R
	errDimTooBig        = "DIM_TOO_BIG"       // the product is larger than one sheet of a material it uses
	errParentSelf       = "PARENT_SELF"       // SKU cha = the SKU itself
	errParentConflict   = "PARENT_CONFLICT"   // the SKU's rows name different parents
	errParentNotFound   = "PARENT_NOT_FOUND"  // parent not in the catalog — import it first
	errParentIsChild    = "PARENT_IS_CHILD"   // parent is itself a child (2 levels max)
	errSKUHasChildren   = "SKU_HAS_CHILDREN"  // an existing parent can't become a child
	errQuotaInvalid     = "QUOTA_INVALID"     // Định mức cell is not a positive whole number, or has no Loại VL
	errQuotaConflict    = "QUOTA_CONFLICT"    // the SKU's rows declare different quotas for the same material
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
	// ParentCode is the parent SKU the file files this SKU under ("" = the file
	// says nothing, the current parent stays). ParentChanged: an existing child
	// moves from another parent to this one.
	ParentCode    string   `json:"parent_code,omitempty"`
	ParentChanged bool     `json:"parent_changed,omitempty"`
	LengthMM      *float64 `json:"length_mm,omitempty"`
	WidthMM       *float64 `json:"width_mm,omitempty"`
	Description   string   `json:"description,omitempty"`
	// QuotaByMaterial is the production quota this SKU will have on each of its
	// materials once applied: the file's "Định mức", else the number already
	// declared on the pair, else the size estimate. QuotaSources says which
	// (models.QuotaDeclared / QuotaEstimated) per material name.
	QuotaByMaterial map[string]int    `json:"quota_by_material,omitempty"`
	QuotaSources    map[string]string `json:"quota_sources,omitempty"`
}

type MappingPlan struct {
	SKUCode      string `json:"sku_code"`
	MaterialCode string `json:"material_code"`
	MaterialName string `json:"material_name"`
	Exists       bool   `json:"exists"`
	// Quota is the declared quota the file sets on this pair (0 = the file says
	// nothing; whatever is stored stays).
	Quota int `json:"quota,omitempty"`
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
	MissingCount int `json:"missing_count"`
	ErrorRows    int `json:"error_rows"`
	// QuotasSet: pairs that get a declared quota from the file's Định mức column.
	QuotasSet int `json:"quotas_set"`
	// ChildSKUs: SKUs the file files under a parent; ParentGroups: how many
	// distinct parents they go to.
	ChildSKUs    int `json:"child_skus"`
	ParentGroups int `json:"parent_groups"`
}

type MasterImportApplied struct {
	MaterialsCreated int `json:"materials_created"`
	SKUsCreated      int `json:"skus_created"`
	MappingsCreated  int `json:"mappings_created"`
	// SKUsUpdated: existing SKUs whose parent, D x R or description changed.
	SKUsUpdated int `json:"skus_updated"`
	// QuotasSet: (SKU, material) pairs whose declared quota was written.
	QuotasSet int `json:"quotas_set"`
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

// legacyParentHeaders: the "SKU cha" column — the parent SKU's code.
var legacyParentHeaders = map[string]bool{
	"skucha": true, "maskucha": true, "parentsku": true, "skuparent": true, "parent": true,
}

// Size columns, keyed by dimHeaderKey (so "D (mm)", "D", "Dài (mm)" all match).
// "Size" is deliberately NOT an alias: order files carry a "Size" column of
// Small/Large labels, and this importer also reads those files.
var (
	legacyLengthHeaders = map[string]bool{"d": true, "dai": true, "chieudai": true, "length": true}
	legacyWidthHeaders  = map[string]bool{"r": true, "rong": true, "chieurong": true, "width": true}
	legacySizeHeaders   = map[string]bool{"dxr": true, "kichthuoc": true, "kichthuocdxr": true}
)

// skuQuotaHeaders: the declared production quota of the row's (SKU, Loại VL)
// pair — "Định mức", "SP/tấm", "Định mức (sp/tấm)"… Keyed by quotaHeaderKey.
var skuQuotaHeaders = map[string]bool{
	"dinhmuc": true, "dinhmucsanxuat": true, "dinhmucsptam": true, "dinhmucsanphamtam": true,
	"sptam": true, "sanphamtam": true, "sanphamtrentam": true, "sanphammottam": true,
	"quota": true, "productsperunit": true, "productspersheet": true, "persheet": true,
}

// quotaHeaderKey normalizes a quota header and drops brackets: "Định mức (sp/tấm)" → "dinhmucsptam".
func quotaHeaderKey(h string) string {
	return strings.NewReplacer("(", "", ")", "", "[", "", "]", "").Replace(normalizeLegacyHeader(h))
}

// legacySKUDescHeaders: the SKU's own description. Narrower than the material
// import's aliases on purpose — "Note"/"Ghi chú" in an order file is a note about
// the order, and must not become the SKU's description.
var legacySKUDescHeaders = map[string]bool{
	"mota": true, "motasanpham": true, "motasku": true, "description": true,
}

// dimHeaderKey normalizes a size header and drops the unit: "D (mm)" → "d",
// "D x R (mm)" → "dxr", "Kích thước" → "kichthuoc".
func dimHeaderKey(h string) string {
	k := strings.NewReplacer("(", "", ")", "", "[", "", "]", "").Replace(normalizeLegacyHeader(h))
	return strings.TrimSuffix(k, "mm")
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
	parentIdx, lenIdx, widIdx, sizeIdx, descIdx, quotaIdx := -1, -1, -1, -1, -1, -1
	for i, h := range header {
		n := normalizeLegacyHeader(h)
		d := dimHeaderKey(h)
		q := quotaHeaderKey(h)
		switch {
		case skuIdx == -1 && legacySKUHeaders[n]:
			skuIdx = i
		case matIdx == -1 && legacyMaterialHeaders[n]:
			matIdx = i
		case prodIdx == -1 && legacyProductHeaders[n]:
			prodIdx = i
		case parentIdx == -1 && legacyParentHeaders[n]:
			parentIdx = i
		case lenIdx == -1 && legacyLengthHeaders[d]:
			lenIdx = i
		case widIdx == -1 && legacyWidthHeaders[d]:
			widIdx = i
		case sizeIdx == -1 && legacySizeHeaders[d]:
			sizeIdx = i
		case descIdx == -1 && legacySKUDescHeaders[n]:
			descIdx = i
		case quotaIdx == -1 && skuQuotaHeaders[q]:
			quotaIdx = i
		}
	}
	if skuIdx == -1 {
		return nil, apperr.BadRequest("Không tìm thấy cột 'SKU' trong file — kiểm tra lại dòng tiêu đề")
	}
	// matIdx == -1 is allowed: the file has no 'Loại VL' column, so every SKU will
	// be flagged MISSING_MATERIAL (we never guess a material). Every other column
	// is optional too: a file without it behaves exactly as before.
	cell := func(rec []string, idx int) string {
		if idx >= 0 && idx < len(rec) {
			return strings.TrimSpace(rec[idx])
		}
		return ""
	}
	rows := make([]LegacyRow, 0, len(records)-1)
	for di, rec := range records[1:] {
		rows = append(rows, LegacyRow{
			RowNumber:   di + 1,
			SKU:         cell(rec, skuIdx),
			Material:    cell(rec, matIdx),
			ProductName: cell(rec, prodIdx),
			ParentSKU:   cell(rec, parentIdx),
			Length:      cell(rec, lenIdx),
			Width:       cell(rec, widIdx),
			Size:        cell(rec, sizeIdx),
			Quota:       cell(rec, quotaIdx),
			Description: cell(rec, descIdx),
		})
	}
	return rows, nil
}

// parseQuota reads a "Định mức" cell: blank → 0 (not declared); a positive whole
// number → that; anything else → a message. "40 sp" / "40/tấm" are accepted
// (the unit is stripped), "40,5" is not — half a product per sheet is nonsense.
func parseQuota(raw string) (int, string) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return 0, ""
	}
	m := quotaNumberRe.FindStringSubmatch(raw)
	if m == nil {
		return 0, "Định mức = \"" + raw + "\" không phải số nguyên dương (số sản phẩm một tấm làm ra, vd 40)"
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 0, "Định mức = \"" + raw + "\" phải là số nguyên dương"
	}
	return n, ""
}

var quotaNumberRe = regexp.MustCompile(`^(\d+)\s*(?:sp|sản phẩm|san pham|pcs|/\s*tấm|/\s*tam|sp\s*/\s*tấm|sp\s*/\s*tam)?$`)

// dimNumberRe is one size figure: digits with an optional decimal part, either
// "." or the Vietnamese "," — optionally followed by the only unit accepted, mm.
var dimNumberRe = regexp.MustCompile(`^(\d+(?:[.,]\d+)?)\s*(?:mm)?$`)

// dimPairRe splits a combined "D x R" cell: "80 x 60", "80x60mm", "80 × 60".
var dimPairRe = regexp.MustCompile(`^(.+?)\s*[xX×*]\s*(.+)$`)

// parseDimMM reads one D or R cell in millimetres. Blank → (nil, ""). Anything
// that isn't a positive number of mm → a message: "4in" or "10cm" is refused
// rather than silently read as 4 mm, because a wrong size is cut wrong.
func parseDimMM(raw, label string) (*float64, string) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return nil, ""
	}
	m := dimNumberRe.FindStringSubmatch(raw)
	if m == nil {
		return nil, label + " = \"" + raw + "\" không phải số mm (vd 80 hoặc 80,5)"
	}
	// "1,000" / "1.000" is one thousand in one locale and one in the other —
	// Excel renders a thousands-formatted 1000 exactly like that. Refuse to guess.
	if dot := strings.IndexAny(m[1], ".,"); dot >= 0 && len(m[1])-dot-1 == 3 {
		return nil, label + " = \"" + m[1] + "\" dễ hiểu nhầm (hàng nghìn hay số lẻ?) — ghi số không dấu phân cách hàng nghìn, vd 1000 hoặc 1,5"
	}
	v, err := strconv.ParseFloat(strings.Replace(m[1], ",", ".", 1), 64)
	if err != nil || v <= 0 {
		return nil, label + " phải lớn hơn 0"
	}
	if v > maxDimMM {
		return nil, fmt.Sprintf("%s = %s mm quá lớn (tối đa %d mm)", label, m[1], maxDimMM)
	}
	v = math.Round(v*100) / 100
	return &v, ""
}

// parseRowDims resolves a row's D x R from its separate D/R cells, falling back
// to the combined "D x R" cell for whichever side is blank.
func parseRowDims(r LegacyRow) (length, width *float64, msg string) {
	return parseDims(r.Length, r.Width, r.Size)
}

// parseDims reads a D cell, an R cell and a combined "D x R" cell (any may be
// blank) into millimetres. The combined cell fills whichever side has no cell
// of its own.
func parseDims(d, rr, size string) (length, width *float64, msg string) {
	if size = strings.TrimSpace(size); size != "" && (d == "" || rr == "") {
		m := dimPairRe.FindStringSubmatch(size)
		if m == nil {
			return nil, nil, "D x R = \"" + size + "\" phải có dạng 80 x 60 (mm)"
		}
		if d == "" {
			d = m[1]
		}
		if rr == "" {
			rr = m[2]
		}
	}
	if length, msg = parseDimMM(d, "D"); msg != "" {
		return nil, nil, msg
	}
	if width, msg = parseDimMM(rr, "R"); msg != "" {
		return nil, nil, msg
	}
	return length, width, ""
}

// fmtDim renders a size for a message: "80", "60,5", or "?" when undeclared.
func fmtDim(v *float64) string {
	if v == nil {
		return "?"
	}
	return strings.Replace(strconv.FormatFloat(*v, 'f', -1, 64), ".", ",", 1)
}

func dimEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ---------- Analysis ----------

type skuAgg struct {
	code          string
	name          string
	matNames      []string          // union of every material seen for this SKU, first-seen order
	productNames  []string          // distinct human-readable product names seen, first-seen order
	rowSets       map[string]string // distinct per-row material sets: signature → as written (blank rows excluded)
	rowCount      int
	firstSeen     int
	firstRow      int            // file row of the SKU's first line — where a SKU-level error points
	parentCodes   []string       // distinct normalized "SKU cha" codes its rows name
	length        *float64       // first declared D
	width         *float64       // first declared R
	dimConflict   bool           // two rows declare different D x R
	description   string         // first non-blank Mô tả
	quotas        map[string]int // effective quota per material name (preview)
	quotaSrc      map[string]string
	declared      map[string]int // file's Định mức per lower(material name)
	quotaConflict bool           // two rows declare different quotas for one material
}

// skuError reports a SKU-level problem against the SKU's first row.
func (a *skuAgg) skuError(code, msg string) LegacyRowError {
	return LegacyRowError{
		RowNumber: a.firstRow, SKU: a.name, Material: strings.Join(a.matNames, " + "),
		ErrorCode: code, Message: msg,
	}
}

// catalogSnapshot is the slice of the catalog one import file touches, read up
// front in a fixed number of queries. Looking each name/code up as the plan is
// built cost one query per SKU (three, with the material preload) — a 200-row
// file meant ~600 round-trips, which on a hosted database is a minute of waiting.
type catalogSnapshot struct {
	matByName map[string]*models.Material // key: lower(trim(name))
	skuByCode map[string]*models.SKU      // key: normalized code
	mappings  map[[2]uint]bool            // (skuID, materialID) pairs already stored
	quota     map[[2]uint]int             // declared quota per stored pair (0 = none)
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
		quota:     map[[2]uint]int{},
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
			if p.ProductsPerUnit != nil && *p.ProductsPerUnit > 0 {
				snap.quota[[2]uint{p.SKUID, p.MaterialID}] = *p.ProductsPerUnit
			}
		}
	}
	return snap, nil
}

// analyze groups the file rows by SKU and by material and derives the full plan.
// A SKU the file files under a parent is only planned when that parent already
// exists as a top-level SKU — parents are imported first (ImportParents). Every
// SKU-level problem (parent missing, rows disagreeing…) drops that SKU and
// reports it, rather than guessing.
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
		parent := strings.TrimSpace(r.ParentSKU)
		if sku == "" && len(mats) == 0 && parent == "" {
			continue // blank line — ignore silently
		}
		total++
		if sku == "" {
			rowErrors = append(rowErrors, LegacyRowError{
				RowNumber: r.RowNumber, Material: strings.TrimSpace(r.Material), ErrorCode: errSKUMissing,
				Message: "Dòng có Loại VL / SKU cha nhưng thiếu SKU",
			})
			continue
		}
		code := normalizeSKUCode(sku)
		length, width, dimMsg := parseRowDims(r)
		if dimMsg == "" && (length == nil) != (width == nil) {
			dimMsg = "Cần cả D lẫn R (mm) — hoặc để trống cả hai"
		}
		if dimMsg != "" {
			rowErrors = append(rowErrors, LegacyRowError{
				RowNumber: r.RowNumber, SKU: sku, Material: strings.TrimSpace(r.Material),
				ErrorCode: errDimInvalid, Message: dimMsg,
			})
			continue
		}
		quota, quotaMsg := parseQuota(r.Quota)
		if quotaMsg == "" && quota > 0 && len(mats) == 0 {
			quotaMsg = "Định mức phải đi kèm Loại VL trên cùng dòng (định mức là của cặp SKU – NVL)"
		}
		if quotaMsg != "" {
			rowErrors = append(rowErrors, LegacyRowError{
				RowNumber: r.RowNumber, SKU: sku, Material: strings.TrimSpace(r.Material),
				ErrorCode: errQuotaInvalid, Message: quotaMsg,
			})
			continue
		}
		parentCode := ""
		if parent != "" {
			parentCode = normalizeSKUCode(parent)
			if parentCode == code {
				rowErrors = append(rowErrors, LegacyRowError{
					RowNumber: r.RowNumber, SKU: sku, Material: strings.TrimSpace(r.Material),
					ErrorCode: errParentSelf, Message: "SKU cha trùng chính SKU này",
				})
				continue
			}
		}

		agg := skuMap[code]
		if agg == nil {
			agg = &skuAgg{code: code, name: sku, rowSets: map[string]string{}, firstSeen: len(skuOrder), firstRow: r.RowNumber}
			skuMap[code] = agg
			skuOrder = append(skuOrder, code)
		}
		agg.rowCount++
		if pn := strings.TrimSpace(r.ProductName); pn != "" && !containsFold(agg.productNames, pn) {
			agg.productNames = append(agg.productNames, pn)
		}
		if parentCode != "" && !containsFold(agg.parentCodes, parentCode) {
			agg.parentCodes = append(agg.parentCodes, parentCode)
		}
		if length != nil {
			if agg.length != nil && *agg.length != *length {
				agg.dimConflict = true
			} else {
				agg.length = length
			}
		}
		if width != nil {
			if agg.width != nil && *agg.width != *width {
				agg.dimConflict = true
			} else {
				agg.width = width
			}
		}
		if d := strings.TrimSpace(r.Description); d != "" && agg.description == "" {
			agg.description = d
		}
		if len(mats) > 0 {
			if sig := rowSignature(mats); agg.rowSets[sig] == "" {
				agg.rowSets[sig] = strings.Join(mats, " + ")
			}
			for _, name := range mats {
				if !containsFold(agg.matNames, name) {
					agg.matNames = append(agg.matNames, name)
				}
				// A row's Định mức applies to every material on that row (a combo
				// row wanting different numbers per material lists them one per row).
				if quota > 0 {
					if agg.declared == nil {
						agg.declared = map[string]int{}
					}
					lk := strings.ToLower(name)
					if prev, ok := agg.declared[lk]; ok && prev != quota {
						agg.quotaConflict = true
					} else {
						agg.declared[lk] = quota
					}
				}
				lm := strings.ToLower(name)
				if !matSeen[lm] {
					matSeen[lm] = true
					matOrder = append(matOrder, name)
				}
			}
		}
	}

	// Parents are looked up in the same catalog read as the SKUs themselves.
	lookupCodes := append([]string{}, skuOrder...)
	fileChild := map[string]bool{} // codes this file files under a parent
	for _, code := range skuOrder {
		agg := skuMap[code]
		lookupCodes = append(lookupCodes, agg.parentCodes...)
		if len(agg.parentCodes) > 0 {
			fileChild[code] = true
		}
	}

	// The whole catalog lookup for this file, up front.
	snap, err := s.loadCatalog(matOrder, lookupCodes)
	if err != nil {
		return nil, apperr.Internal("could not read catalog").Wrap(err)
	}
	// Existing SKUs the file wants to file under a parent: are any of them
	// parents already? One query, and only when the file has a SKU cha column.
	var becomingChild []uint
	for _, code := range skuOrder {
		if rec := snap.sku(code); rec != nil && fileChild[code] {
			becomingChild = append(becomingChild, rec.ID)
		}
	}
	children := map[uint][]uint{}
	if len(becomingChild) > 0 {
		if children, err = s.repo.SKU.ChildrenOf(becomingChild); err != nil {
			return nil, apperr.Internal("could not read catalog").Wrap(err)
		}
	}

	// Validate each SKU's parent and size; a failing SKU leaves the plan whole.
	accepted := make([]string, 0, len(skuOrder))
	for _, code := range skuOrder {
		agg := skuMap[code]
		var bad *LegacyRowError
		fail := func(errCode, msg string) {
			e := agg.skuError(errCode, msg)
			bad = &e
		}
		switch {
		case len(agg.rowSets) > 1:
			sets := make([]string, 0, len(agg.rowSets))
			for _, v := range agg.rowSets {
				sets = append(sets, v)
			}
			sort.Strings(sets)
			fail(errMaterialConflict, "Các dòng của SKU này khai Loại VL khác nhau ("+strings.Join(sets, " / ")+") — sửa cho thống nhất")
		case agg.dimConflict:
			fail(errDimConflict, "Các dòng của SKU này khai D x R khác nhau — sửa cho thống nhất")
		case agg.quotaConflict:
			fail(errQuotaConflict, "Các dòng của SKU này khai định mức khác nhau cho cùng một Loại VL — sửa cho thống nhất")
		case len(agg.parentCodes) > 1:
			fail(errParentConflict, "Các dòng của SKU này khai nhiều SKU cha khác nhau: "+strings.Join(agg.parentCodes, ", "))
		case len(agg.parentCodes) == 1:
			pc := agg.parentCodes[0]
			parent := snap.sku(pc)
			rec := snap.sku(code)
			switch {
			case parent == nil:
				fail(errParentNotFound, "SKU cha "+pc+" chưa có trong hệ thống — import SKU cha trước (bước 1)")
			case parent.ParentID != nil:
				fail(errParentIsChild, "SKU cha "+pc+" đang là SKU con của SKU khác — chỉ hỗ trợ 2 tầng cha → con")
			case fileChild[pc]:
				fail(errParentIsChild, "SKU cha "+pc+" cũng đang được khai là SKU con trong file này — chỉ hỗ trợ 2 tầng")
			case rec != nil && len(children[rec.ID]) > 0:
				fail(errSKUHasChildren, fmt.Sprintf("SKU này đang là SKU cha của %d SKU con — không làm SKU con được", len(children[rec.ID])))
			}
		}
		if bad == nil {
			// The product must fit on one sheet of every material it uses. Sizes as
			// they will be after the import: the file's when declared, else what the
			// catalog already holds. A material the file creates has no size yet.
			rec := snap.sku(code)
			probe := &models.SKU{LengthMM: agg.length, WidthMM: agg.width}
			if probe.LengthMM == nil && rec != nil {
				probe.LengthMM, probe.WidthMM = rec.LengthMM, rec.WidthMM
			}
			for _, name := range agg.matNames {
				m := snap.material(name)
				if m == nil {
					continue
				}
				if !models.ProductFitsSheet(probe, m) {
					fail(errDimTooBig, fmt.Sprintf("SKU %s × %s mm to hơn một tấm %s (%s × %s mm)",
						fmtDim(probe.LengthMM), fmtDim(probe.WidthMM), m.Name, fmtDim(m.LengthMM), fmtDim(m.WidthMM)))
					break
				}
				// Effective quota after the import: the file's number, else the one
				// already declared on the pair, else the size estimate.
				q, src := agg.declared[strings.ToLower(name)], models.QuotaDeclared
				if q == 0 && rec != nil {
					q = snap.quota[[2]uint{rec.ID, m.ID}]
				}
				if q == 0 {
					q, src = models.EstimatedQuota(probe, m), models.QuotaEstimated
				}
				if q > 0 {
					if agg.quotas == nil {
						agg.quotas = map[string]int{}
						agg.quotaSrc = map[string]string{}
					}
					agg.quotas[m.Name] = q
					agg.quotaSrc[m.Name] = string(src)
				}
			}
		}
		if bad != nil {
			rowErrors = append(rowErrors, *bad)
			continue
		}
		accepted = append(accepted, code)
	}

	// SKU-level errors were found after the row pass; file order reads better.
	sort.SliceStable(rowErrors, func(i, j int) bool { return rowErrors[i].RowNumber < rowErrors[j].RowNumber })
	pv := &MasterImportPreview{Errors: rowErrors}
	sum := MasterImportSummary{TotalRows: total, ErrorRows: len(rowErrors)}

	// Materials plan — only what an accepted SKU uses, in first-seen file order,
	// so a dropped SKU doesn't leave a stray new material behind.
	used := map[string]bool{}
	for _, code := range accepted {
		for _, m := range skuMap[code].matNames {
			used[strings.ToLower(m)] = true
		}
	}
	for _, name := range matOrder {
		if !used[strings.ToLower(name)] {
			continue
		}
		exists := snap.material(name) != nil
		if !exists {
			sum.NewMaterials++
		}
		pv.Materials = append(pv.Materials, MaterialPlan{Code: materialCode(name), Name: name, Exists: exists})
	}

	// SKU + mapping plan.
	parents := map[string]bool{}
	for _, code := range accepted {
		agg := skuMap[code]
		skuRec := snap.sku(code)
		skuExists := skuRec != nil
		if !skuExists {
			sum.NewSKUs++
		}

		// A SKU with no material at all is "missing"; otherwise it maps its full
		// material set (single or combo) — rows that disagreed were refused above.
		isCombo := len(agg.matNames) >= 2
		status := skuStatusOK
		if len(agg.matNames) == 0 {
			status = skuStatusMissing
			sum.MissingCount++
		}

		productName := ""
		if len(agg.productNames) > 0 {
			productName = agg.productNames[0]
		}
		plan := SKUPlan{
			Code: agg.code, Name: agg.name, ProductName: productName, ProductNames: agg.productNames,
			Exists: skuExists, MaterialNames: agg.matNames, Status: status, RowCount: agg.rowCount, IsCombo: isCombo,
			LengthMM: agg.length, WidthMM: agg.width, Description: agg.description,
			QuotaByMaterial: agg.quotas, QuotaSources: agg.quotaSrc,
		}
		if len(agg.parentCodes) == 1 {
			plan.ParentCode = agg.parentCodes[0]
			parentID := snap.sku(plan.ParentCode).ID
			plan.ParentChanged = skuRec != nil && skuRec.ParentID != nil && *skuRec.ParentID != parentID
			sum.ChildSKUs++
			parents[plan.ParentCode] = true
		}
		pv.SKUs = append(pv.SKUs, plan)

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
				declared := agg.declared[strings.ToLower(matName)]
				if declared > 0 {
					sum.QuotasSet++
				}
				pv.Mappings = append(pv.Mappings, MappingPlan{
					SKUCode: agg.code, MaterialCode: materialCode(matName), MaterialName: matName, Exists: exists,
					Quota: declared,
				})
			}
		}
	}
	sum.ParentGroups = len(parents)

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
		fmt.Sprintf("Preview legacy master data: %d rows, %d new materials, %d new SKUs, %d new mappings, %d missing, %d errors",
			pv.Summary.TotalRows, pv.Summary.NewMaterials, pv.Summary.NewSKUs, pv.Summary.NewMappings,
			pv.Summary.MissingCount, pv.Summary.ErrorRows), nil)
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
			if sp.ParentCode != "" {
				skuCodes = append(skuCodes, sp.ParentCode)
			}
		}
		snap, err := loadCatalogSnapshot(txRepo, matNames, skuCodes)
		if err != nil {
			return err
		}

		// ---- Parents: re-checked, the preview may be minutes old ----
		// A parent deleted or filed under another SKU since then stops the whole
		// commit: applying the rest would quietly import children as standalone.
		parentIDByCode := map[string]uint{}
		var becomingChild []uint
		for _, sp := range pv.SKUs {
			if sp.ParentCode == "" {
				continue
			}
			p := snap.sku(sp.ParentCode)
			if p == nil || p.ParentID != nil {
				return apperr.Conflict("SKU cha " + sp.ParentCode + " không còn hợp lệ (đã xoá hoặc thành SKU con) — bấm Xem trước lại")
			}
			parentIDByCode[sp.ParentCode] = p.ID
			if rec := snap.sku(sp.Code); rec != nil {
				becomingChild = append(becomingChild, rec.ID)
			}
		}
		if len(becomingChild) > 0 {
			children, err := txRepo.SKU.ChildrenOf(becomingChild)
			if err != nil {
				return err
			}
			if len(children) > 0 {
				return apperr.Conflict("Có SKU vừa được gán SKU con nên không làm SKU con được nữa — bấm Xem trước lại")
			}
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
		// Existing SKUs whose parent, D x R or description the file changes — all
		// in one CASE-per-column UPDATE, since each carries its own values. A blank
		// cell never clears: the patch only carries what the file actually says.
		var patches []repositories.SKUPatch
		for _, sp := range pv.SKUs {
			var parentID *uint
			if id, ok := parentIDByCode[sp.ParentCode]; ok {
				parentID = &id
			}
			if rec := snap.sku(sp.Code); rec != nil {
				skuIDByCode[sp.Code] = rec.ID
				// The file is the source of truth for the human-readable name. Scoped
				// update — never touches the material mapping.
				if sp.ProductName != "" && rec.ProductName != sp.ProductName {
					renameIDs[sp.ProductName] = append(renameIDs[sp.ProductName], rec.ID)
				}
				p := repositories.SKUPatch{ID: rec.ID}
				if parentID != nil && (rec.ParentID == nil || *rec.ParentID != *parentID) {
					p.ParentID = parentID
				}
				if sp.LengthMM != nil && !dimEqual(rec.LengthMM, sp.LengthMM) {
					p.LengthMM = sp.LengthMM
				}
				if sp.WidthMM != nil && !dimEqual(rec.WidthMM, sp.WidthMM) {
					p.WidthMM = sp.WidthMM
				}
				if sp.Description != "" && rec.Description != sp.Description {
					desc := sp.Description
					p.Description = &desc
				}
				if p.ParentID != nil || p.LengthMM != nil || p.WidthMM != nil || p.Description != nil {
					patches = append(patches, p)
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
				ParentID: parentID, LengthMM: sp.LengthMM, WidthMM: sp.WidthMM, Description: sp.Description,
			})
		}
		if err := txRepo.SKU.PatchMany(patches); err != nil {
			return err
		}
		applied.SKUsUpdated = len(patches)
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

		// ---- Mappings: only the pairs that aren't stored yet, in batches; the
		// file's Định mức rides on new pairs and is written onto existing ones ----
		var newMappings []models.SKUMaterial
		var quotaUpdates []repositories.PairQuota
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
				if mp.Quota > 0 && snap.quota[pair] != mp.Quota {
					quotaUpdates = append(quotaUpdates, repositories.PairQuota{SKUID: skuID, MaterialID: matID, Quota: mp.Quota})
					snap.quota[pair] = mp.Quota
				}
				continue
			}
			snap.mappings[pair] = true
			var quota *int
			if mp.Quota > 0 {
				q := mp.Quota
				quota = &q
				applied.QuotasSet++
			}
			newMappings = append(newMappings, models.SKUMaterial{
				SKUID: skuID, MaterialID: matID, QuantityPerUnit: 1, ProductsPerUnit: quota, Note: mappingSourceNote,
			})
		}
		if err := txRepo.SKU.AddMaterialsMany(newMappings, skuInsertBatch); err != nil {
			return err
		}
		applied.MappingsCreated = len(newMappings)
		if err := txRepo.SKU.SetPairQuotas(quotaUpdates); err != nil {
			return err
		}
		applied.QuotasSet += len(quotaUpdates)

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
		// The applied counts ride in the stored plan too: the job table has no
		// column for SKUs updated, and Get should report what the commit did.
		pv.Applied = &applied
		raw, err := models.ToJSONB(pv)
		if err != nil {
			return err
		}
		job.Plan = raw
		return tx.Save(job).Error
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok && ae.Code != "INTERNAL" {
			return nil, ae
		}
		return nil, apperr.Internal("could not commit master import").Wrap(err)
	}

	pv.ImportJobID = job.ID
	pv.Status = job.Status
	pv.Applied = &applied

	s.audit.Log(actor, "MASTER_IMPORT_COMMIT", "master_import_job", &job.ID,
		fmt.Sprintf("Committed legacy master data: created %d materials, %d SKUs, %d mappings; updated %d SKUs",
			applied.MaterialsCreated, applied.SKUsCreated, applied.MappingsCreated, applied.SKUsUpdated), nil)
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
	if job.Status == models.ImportCommitted && pv.Applied == nil {
		// Jobs committed before the plan carried its own applied counts.
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

// masterTemplateHeaders are the columns the importer reads: "SKU cha" (the parent
// SKU, imported first — blank for a standalone SKU), "SKU", the human-readable
// "Tên sản phẩm", "Loại VL", the size "D (mm)" × "R (mm)", the declared "Định
// mức" (products per sheet of that Loại VL) and "Mô tả". Only "SKU" is required. The file the factory uploads may carry many more columns — those
// are ignored — but the sample we hand back keeps just these so the format is
// obvious.
var masterTemplateHeaders = []string{"SKU cha", "SKU", "Tên sản phẩm", "Loại VL", "D (mm)", "R (mm)", "Định mức", "Mô tả"}

// masterTemplateSample is a handful of example rows so the user can see exactly
// what a valid row looks like before filling in their own: three children of the
// parent HOP-NHUA (which must already exist — imported in step 1), a standalone
// SKU with no parent, and a combo SKU (several materials in one cell joined by " + ").
var masterTemplateSample = [][]string{
	{"HOP-NHUA", "HOP-NHUA-BE", "Hộp nhựa bé", "Mica trong 3 ly", "80", "60", "90", "Hộp nắp trượt"},
	{"HOP-NHUA", "HOP-NHUA-LON", "Hộp nhựa lớn", "Mica trong 3 ly", "160", "120", "20", ""},
	{"HOP-NHUA", "HOP-NHUA-VUONG", "Hộp nhựa vuông", "Mica trong 3 ly", "100", "100", "", ""},
	{"", "LWD-12IN", "Thớt gỗ khắc tên", "Gỗ 5 ly 3 lớp", "304.8", "203.2", "12", ""},
	{"", "COMBO-A2-GAI", "Đèn gỗ combo", "Mica trong 3 ly + Mica Hologram", "", "", "", ""},
}

// MasterTemplateXLSX renders the master-data import sample as a real .xlsx
// workbook (columns split cleanly in Excel on any locale, unlike the old comma
// CSV that opened as garbled single-column text).
func (s *MasterImportService) MasterTemplateXLSX() ([]byte, string, error) {
	grid := append([][]string{masterTemplateHeaders}, masterTemplateSample...)
	data, err := buildTemplateXLSX("Master data", grid, []float64{16, 20, 24, 30, 10, 10, 10, 28})
	if err != nil {
		return nil, "", err
	}
	return data, "master-data-template.xlsx", nil
}

// masterExportEstimateHeader is the extra, read-only column of the export: the
// size estimate, there to be compared with, never imported (the importer only
// reads "Định mức").
const masterExportEstimateHeader = "Ước tính theo kích thước (import bỏ qua)"

// MasterExportXLSX renders the catalog's child and standalone SKUs, one row per
// (SKU, material), in the import file's own layout — so the factory fills the
// "Định mức" column from its layout file and imports the same file straight
// back. Parents are left out (step 1 owns them); a SKU without materials still
// gets a row so nothing in the catalog is invisible. Sorted by parent then code.
func (s *MasterImportService) MasterExportXLSX() ([]byte, string, error) {
	skus, _, err := s.repo.SKU.List(repositories.Page{PageSize: repositories.PageSizeAll}.Normalize())
	if err != nil {
		return nil, "", apperr.Internal("could not read catalog").Wrap(err)
	}
	byID := map[uint]*models.SKU{}
	hasChildren := map[uint]bool{}
	for i := range skus {
		byID[skus[i].ID] = &skus[i]
		if skus[i].ParentID != nil {
			hasChildren[*skus[i].ParentID] = true
		}
	}
	dimCell := func(v *float64) string {
		if v == nil {
			return ""
		}
		return fmtDim(v)
	}
	parentCode := func(sku *models.SKU) string {
		if sku.ParentID == nil {
			return ""
		}
		if p := byID[*sku.ParentID]; p != nil {
			return p.Code
		}
		return ""
	}
	type row struct {
		parent string
		cells  []string
	}
	var rows []row
	for i := range skus {
		sku := &skus[i]
		if hasChildren[sku.ID] {
			continue
		}
		base := []string{parentCode(sku), sku.Code, sku.ProductName}
		if len(sku.Materials) == 0 {
			rows = append(rows, row{parent: base[0], cells: append(base, "", dimCell(sku.LengthMM), dimCell(sku.WidthMM), "", sku.Description, "")})
			continue
		}
		for j := range sku.Materials {
			sm := &sku.Materials[j]
			declared := ""
			if sm.ProductsPerUnit != nil && *sm.ProductsPerUnit > 0 {
				declared = strconv.Itoa(*sm.ProductsPerUnit)
			}
			estimate := ""
			if q := models.EstimatedQuota(sku, &sm.Material); q > 0 {
				estimate = strconv.Itoa(q)
			}
			rows = append(rows, row{parent: base[0], cells: append(append([]string{}, base...),
				sm.Material.Name, dimCell(sku.LengthMM), dimCell(sku.WidthMM), declared, sku.Description, estimate)})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].parent != rows[j].parent {
			return rows[i].parent < rows[j].parent
		}
		return rows[i].cells[1] < rows[j].cells[1]
	})
	grid := [][]string{append(append([]string{}, masterTemplateHeaders...), masterExportEstimateHeader)}
	for _, r := range rows {
		grid = append(grid, r.cells)
	}
	data, err := buildTemplateXLSX("SKU", grid, []float64{16, 20, 24, 30, 10, 10, 10, 28, 20})
	if err != nil {
		return nil, "", err
	}
	return data, "sku-hien-co.xlsx", nil
}

// skuInsertBatch is how many SKUs / mappings ride in one INSERT.
const skuInsertBatch = 200
