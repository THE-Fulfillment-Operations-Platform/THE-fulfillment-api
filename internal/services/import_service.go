package services

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// ImportService handles order imports: parse → validate (preview) → commit.
type ImportService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// FlexInt parses an integer from either a JSON number or a quoted string so the
// same row type works for CSV and JSON payloads.
type FlexInt int

func (f *FlexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("invalid integer %q", s)
	}
	*f = FlexInt(n)
	return nil
}

// ImportRow mirrors the seller's file columns (one row = one order item).
//
// The 2026-08 template renamed most columns to Vietnamese ("ORDER ID", "MÃ SKU",
// "Địa chỉ nhận", …) and dropped ShippingMethod / ProductName / VariantCode /
// IOSS / ShippingEmail. The field names here stay in the internal English
// vocabulary — the mapping from whatever label a file happens to use lives in
// headerToField, so old files keep importing unchanged.
type ImportRow struct {
	// SellerRef is the seller's own id/code from the "Seller ID" column. It never
	// selects the seller — the importer already knows which seller it is importing
	// for — it is only cross-checked against that seller so a file belonging to
	// someone else cannot be committed onto the wrong account.
	// The json tag is NOT cosmetic: Preview persists rows as JSON on the import
	// job and Commit reads them back through UnmarshalJSON, which resolves keys
	// through headerToField. A tag that does not normalize to a known header key
	// silently loses the value between preview and commit.
	SellerRef string `json:"Seller ID"`
	// OrderDateRaw is the "DATE" column exactly as the file wrote it. Parsing is
	// deferred to parseOrderDate so a bad value becomes a row-level validation
	// error instead of a parse failure that kills the whole upload.
	OrderDateRaw string `json:"OrderDate"`

	StoreOrderID     string  `json:"StoreOrderID"`
	Account          string  `json:"Account"`
	StoreName        string  `json:"StoreName"`
	Quantity         FlexInt `json:"Quantity"`
	SKU              string  `json:"SKU"`
	ImageCode        string  `json:"ImageCode"`   // "Mã ảnh"
	Design           string  `json:"Design"`      // legacy single/front design column (kept compatible)
	FrontDesign      string  `json:"FrontDesign"` // front design link (new)
	BackDesign       string  `json:"BackDesign"`  // back design link (optional, two-sided products)
	Mockup           string  `json:"Mockup"`
	EngraveText      string  `json:"EngraveText"`
	ShippingName     string  `json:"ShippingName"`
	ShippingAddress1 string  `json:"ShippingAddress1"`
	ShippingAddress2 string  `json:"ShippingAddress2"`
	ShippingCity     string  `json:"ShippingCity"`
	ShippingZip      string  `json:"ShippingZip"`
	ShippingProvince string  `json:"ShippingProvince"`
	ShippingCountry  string  `json:"ShippingCountry"`
	ShippingPhone    string  `json:"ShippingPhone"`
	// PhoneAlt backs the template's second phone column ("Phone"). The system keeps
	// ONE recipient phone; this is only a fallback so a file that filled "Phone"
	// and left "ShippingPhone" empty does not ship a parcel with no phone number.
	PhoneAlt string `json:"Phone"`
	Note     string `json:"Note"`
}

// RecipientPhone is the one phone number the order carries: "ShippingPhone" when
// the file filled it, otherwise the "Phone" column.
func (r ImportRow) RecipientPhone() string {
	if v := strings.TrimSpace(r.ShippingPhone); v != "" {
		return v
	}
	return strings.TrimSpace(r.PhoneAlt)
}

// UnmarshalJSON maps a JSON object onto an ImportRow through the same flexible
// header mapping as the file-upload path, so the paste-CSV/JSON path accepts the
// seller template's own column labels ("Mã ảnh", "Account", …) — not just the
// struct's canonical keys. Values may be JSON strings or numbers. This also keeps
// the internal RawRows round-trip lossless (every struct-tag key normalizes back
// to a headerToField entry).
func (r *ImportRow) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		fn, ok := headerToField[headerKey(k)]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			var num json.Number
			if err2 := json.Unmarshal(v, &num); err2 != nil {
				continue // skip bools/objects/null we can't stringify
			}
			s = num.String()
		}
		fn.set(r, strings.TrimSpace(s))
	}
	return nil
}

// FrontDesignValue is the front/single design for a row: the new "Front Design"
// column if present, otherwise the legacy "Design" column (kept compatible).
func (r ImportRow) FrontDesignValue() string {
	if v := strings.TrimSpace(r.FrontDesign); v != "" {
		return v
	}
	return strings.TrimSpace(r.Design)
}

// PreviewResult is returned to the client after validation.
type PreviewResult struct {
	ImportJobID uint                   `json:"import_job_id"`
	Status      models.ImportJobStatus `json:"status"`
	TotalRows   int                    `json:"total_rows"`
	OrderCount  int                    `json:"order_count"`
	ValidRows   int                    `json:"valid_rows"`
	ErrorRows   int                    `json:"error_rows"`
	Errors      []models.ImportError   `json:"errors"`
	// Warnings are non-blocking heads-up rows (e.g. a StoreOrderID that already
	// exists for the seller). They are still imported — the client highlights them
	// so staff can eyeball a possible duplicate — and never gate the commit.
	Warnings []models.ImportError `json:"warnings"`
	// Headers is what the parser made of the file's header row, so the UI can say
	// "these columns were ignored" instead of leaving it to be discovered later.
	Headers HeaderReport `json:"headers"`
}

// csvHeaderMap normalizes a header cell into a canonical key. It NFC-normalizes
// first so a Vietnamese header ("Mã ảnh") matches whether the file stored it
// pre-composed (NFC) or decomposed (NFD, as some macOS/exporter tooling emits).
func normalizeHeader(h string) string {
	h = strings.TrimPrefix(h, string(rune(0xFEFF))) // strip UTF-8 BOM from Excel "CSV UTF-8" exports
	h = norm.NFC.String(h)
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.NewReplacer(" ", "", "_", "", "-", "").Replace(h)
	return h
}

// headerNoteNoise are parenthesised notes that carry no meaning for the mapping —
// "Mã ảnh (nếu có)" is the same column as "Mã ảnh".
var headerNoteNoise = map[string]bool{
	"nếucó": true, "neuco": true, "nếucó?": true,
	"ifhave": true, "ifany": true, "optional": true,
	"tuỳchọn": true, "tuychon": true, "tùychọn": true,
	"khôngbắtbuộc": true, "khongbatbuoc": true,
}

// headerKey turns a raw header cell into the lookup key for headerToField and
// retiredHeaders. On top of normalizeHeader it drops a trailing parenthesised
// note — but ONLY a note that is known filler.
//
// Stripping every parenthesis would be a data-corruption bug, not a convenience:
// "Địa chỉ nhận (Phụ)" would collapse onto "Địa chỉ nhận" and address line 2
// would silently overwrite line 1 on the shipping label.
func headerKey(h string) string {
	k := normalizeHeader(h)
	if i := strings.LastIndexByte(k, '('); i > 0 && strings.HasSuffix(k, ")") {
		if headerNoteNoise[k[i+1:len(k)-1]] {
			return k[:i]
		}
	}
	return k
}

// headerField is one recognised column: a stable field id (used to report which
// required columns a file is missing) plus the setter that fills it in.
type headerField struct {
	id  string
	set func(*ImportRow, string)
}

func hf(id string, set func(*ImportRow, string)) headerField { return headerField{id: id, set: set} }

// Canonical field ids. Only the ones the validator can complain about need to be
// named; the rest are labels for the missing-column report.
const (
	fSellerRef = "SellerRef"
	fOrderDate = "OrderDate"
	fStoreOrd  = "StoreOrderID"
	fSKU       = "SKU"
	fQuantity  = "Quantity"
	fShipName  = "ShippingName"
	fShipAddr1 = "ShippingAddress1"
	fShipCntry = "ShippingCountry"
)

// headerToField maps a normalized header key onto the field it fills. It holds
// BOTH the 2026-08 Vietnamese template labels and the older English ones, so a
// seller still sitting on the previous file imports without being told to redo
// their sheet. Diacritic-free aliases are listed too — some exporters strip them.
var headerToField = map[string]headerField{
	// --- Seller / store ---
	"sellerid":  hf(fSellerRef, func(r *ImportRow, v string) { r.SellerRef = v }),
	"mãseller":  hf(fSellerRef, func(r *ImportRow, v string) { r.SellerRef = v }),
	"maseller":  hf(fSellerRef, func(r *ImportRow, v string) { r.SellerRef = v }),
	"account":   hf("Account", func(r *ImportRow, v string) { r.Account = v }),
	"shopname":  hf("StoreName", func(r *ImportRow, v string) { r.StoreName = v }),
	"storename": hf("StoreName", func(r *ImportRow, v string) { r.StoreName = v }),

	// --- Order level ---
	// "DATE" is the seller's order date and becomes the order's business day
	// (OrderDate + "STT trong ngày"). See parseOrderDate for the accepted formats.
	"date":         hf(fOrderDate, func(r *ImportRow, v string) { r.OrderDateRaw = v }),
	"ngày":         hf(fOrderDate, func(r *ImportRow, v string) { r.OrderDateRaw = v }),
	"ngay":         hf(fOrderDate, func(r *ImportRow, v string) { r.OrderDateRaw = v }),
	"ngàyđặt":      hf(fOrderDate, func(r *ImportRow, v string) { r.OrderDateRaw = v }),
	"ngaydat":      hf(fOrderDate, func(r *ImportRow, v string) { r.OrderDateRaw = v }),
	"orderdate":    hf(fOrderDate, func(r *ImportRow, v string) { r.OrderDateRaw = v }),
	"orderid":      hf(fStoreOrd, func(r *ImportRow, v string) { r.StoreOrderID = v }),
	"storeorderid": hf(fStoreOrd, func(r *ImportRow, v string) { r.StoreOrderID = v }),

	// --- Recipient ---
	"name":             hf(fShipName, func(r *ImportRow, v string) { r.ShippingName = v }),
	"shippingname":     hf(fShipName, func(r *ImportRow, v string) { r.ShippingName = v }),
	"tênngườinhận":     hf(fShipName, func(r *ImportRow, v string) { r.ShippingName = v }),
	"tennguoinhan":     hf(fShipName, func(r *ImportRow, v string) { r.ShippingName = v }),
	"địachỉnhận":       hf(fShipAddr1, func(r *ImportRow, v string) { r.ShippingAddress1 = v }),
	"diachinhan":       hf(fShipAddr1, func(r *ImportRow, v string) { r.ShippingAddress1 = v }),
	"shippingaddress1": hf(fShipAddr1, func(r *ImportRow, v string) { r.ShippingAddress1 = v }),
	// The "(Phụ)" suffix is load-bearing — it is what separates address line 2 from
	// line 1, which is why headerKey never strips a parenthesis it does not know.
	"địachỉnhận(phụ)":  hf("ShippingAddress2", func(r *ImportRow, v string) { r.ShippingAddress2 = v }),
	"diachinhan(phu)":  hf("ShippingAddress2", func(r *ImportRow, v string) { r.ShippingAddress2 = v }),
	"shippingaddress2": hf("ShippingAddress2", func(r *ImportRow, v string) { r.ShippingAddress2 = v }),
	"thànhphố":         hf("ShippingCity", func(r *ImportRow, v string) { r.ShippingCity = v }),
	"thanhpho":         hf("ShippingCity", func(r *ImportRow, v string) { r.ShippingCity = v }),
	"shippingcity":     hf("ShippingCity", func(r *ImportRow, v string) { r.ShippingCity = v }),
	// "Mã vùng" sits between city and zip in the seller template, i.e. it is the
	// state/province — NOT a telephone area code. Anything else would print a phone
	// prefix onto the shipping label where the state belongs.
	"mãvùng":           hf("ShippingProvince", func(r *ImportRow, v string) { r.ShippingProvince = v }),
	"mavung":           hf("ShippingProvince", func(r *ImportRow, v string) { r.ShippingProvince = v }),
	"shippingprovince": hf("ShippingProvince", func(r *ImportRow, v string) { r.ShippingProvince = v }),
	"zipcode":          hf("ShippingZip", func(r *ImportRow, v string) { r.ShippingZip = v }),
	"zip":              hf("ShippingZip", func(r *ImportRow, v string) { r.ShippingZip = v }),
	"shippingzip":      hf("ShippingZip", func(r *ImportRow, v string) { r.ShippingZip = v }),
	"quốcgia":          hf(fShipCntry, func(r *ImportRow, v string) { r.ShippingCountry = v }),
	"quocgia":          hf(fShipCntry, func(r *ImportRow, v string) { r.ShippingCountry = v }),
	"shippingcountry":  hf(fShipCntry, func(r *ImportRow, v string) { r.ShippingCountry = v }),
	"shippingphone":    hf("ShippingPhone", func(r *ImportRow, v string) { r.ShippingPhone = v }),
	// Second phone column. Kept as a fallback rather than dropped: the seller
	// template puts "Phone" in the address block, so it is the one people actually
	// fill in — discarding it would ship parcels with no contact number.
	"phone":       hf("Phone", func(r *ImportRow, v string) { r.PhoneAlt = v }),
	"sốđiệnthoại": hf("Phone", func(r *ImportRow, v string) { r.PhoneAlt = v }),
	"sodienthoai": hf("Phone", func(r *ImportRow, v string) { r.PhoneAlt = v }),

	// --- Product line ---
	"mãsku":    hf(fSKU, func(r *ImportRow, v string) { r.SKU = v }),
	"masku":    hf(fSKU, func(r *ImportRow, v string) { r.SKU = v }),
	"sku":      hf(fSKU, func(r *ImportRow, v string) { r.SKU = v }),
	"sốlượng":  hf(fQuantity, func(r *ImportRow, v string) { r.Quantity = FlexInt(atoiSafe(v)) }),
	"soluong":  hf(fQuantity, func(r *ImportRow, v string) { r.Quantity = FlexInt(atoiSafe(v)) }),
	"quantity": hf(fQuantity, func(r *ImportRow, v string) { r.Quantity = FlexInt(atoiSafe(v)) }),
	// "Mã ảnh" — VN header (diacritics preserved), plus safe aliases. The template
	// writes it as "Mã ảnh (nếu có)"; headerKey drops the known filler note.
	"mãảnh":     hf("ImageCode", func(r *ImportRow, v string) { r.ImageCode = v }),
	"maanh":     hf("ImageCode", func(r *ImportRow, v string) { r.ImageCode = v }),
	"imagecode": hf("ImageCode", func(r *ImportRow, v string) { r.ImageCode = v }),
	// Front/back design. "DESIGN ORDER" is the new front-side column; the legacy
	// single "Design" column keeps working as the front/single side.
	"designorder":     hf("FrontDesign", func(r *ImportRow, v string) { r.FrontDesign = v }),
	"frontdesign":     hf("FrontDesign", func(r *ImportRow, v string) { r.FrontDesign = v }),
	"frontdesignlink": hf("FrontDesign", func(r *ImportRow, v string) { r.FrontDesign = v }),
	"designfront":     hf("FrontDesign", func(r *ImportRow, v string) { r.FrontDesign = v }),
	"design":          hf("Design", func(r *ImportRow, v string) { r.Design = v }),
	"designback":      hf("BackDesign", func(r *ImportRow, v string) { r.BackDesign = v }),
	"backdesign":      hf("BackDesign", func(r *ImportRow, v string) { r.BackDesign = v }),
	"backdesignlink":  hf("BackDesign", func(r *ImportRow, v string) { r.BackDesign = v }),
	"mockup":          hf("Mockup", func(r *ImportRow, v string) { r.Mockup = v }),
	"mockupurl":       hf("Mockup", func(r *ImportRow, v string) { r.Mockup = v }),
	"engravetext":     hf("EngraveText", func(r *ImportRow, v string) { r.EngraveText = v }),
	"note":            hf("Note", func(r *ImportRow, v string) { r.Note = v }),
	"ghichú":          hf("Note", func(r *ImportRow, v string) { r.Note = v }),
	"ghichu":          hf("Note", func(r *ImportRow, v string) { r.Note = v }),
}

func atoiSafe(v string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	return n
}

// retiredHeaders are columns the system used to import and deliberately no longer
// does. They are still recognised so a file that carries them gets told "this
// column is no longer used" instead of having it reported as an unknown column —
// and so nobody spends an afternoon wondering why IOSS stopped showing up.
var retiredHeaders = map[string]string{
	"shippingmethod": "ShippingMethod",
	"productname":    "ProductName",
	"variantcode":    "VariantCode",
	"ioss":           "IOSS",
	"shippingemail":  "ShippingEmail",
	"email":          "Email",
}

// requiredHeaderFields are the columns a file MUST contain. Missing any of them
// is a whole-file problem, not a per-row one: without ORDER ID or MÃ SKU every
// single row would fail validation and the operator would be left reading a
// thousand identical row errors instead of "you uploaded the wrong template".
var requiredHeaderFields = []string{fStoreOrd, fSKU, fQuantity, fShipName, fShipAddr1, fShipCntry}

// headerLabels name each field in the current template, for human-facing reports.
var headerLabels = map[string]string{
	fStoreOrd:  "ORDER ID",
	fSKU:       "MÃ SKU",
	fQuantity:  "SỐ LƯỢNG",
	fShipName:  "name (tên người nhận)",
	fShipAddr1: "Địa chỉ nhận",
	fShipCntry: "Quốc Gia",
	fOrderDate: "DATE",
	fSellerRef: "Seller ID",
}

func headerLabel(id string) string {
	if l, ok := headerLabels[id]; ok {
		return l
	}
	return id
}

// orderImportTemplateHeaders are the exact column labels the template ships, in
// the order the seller fills them in (the 2026-08 layout: seller/store, then the
// recipient block, then the product line). Every entry must resolve through
// headerKey — TestOrderImportTemplateXLSX_RoundTrips enforces that.
var orderImportTemplateHeaders = []string{
	"Seller ID", "Account", "Shop name", "DATE", "name",
	"Địa chỉ nhận", "Địa chỉ nhận (Phụ)", "Thành phố", "Mã vùng", "Zipcode", "Quốc Gia",
	"ORDER ID", "MÃ SKU", "Mã ảnh (nếu có)", "SỐ LƯỢNG",
	"DESIGN ORDER", "designBack", "Mockup", "EngraveText (if have)",
	"ShippingPhone", "Note",
}

// orderImportTemplateSample gives two rows of ONE order (same ORDER ID = one
// order, two products): a one-sided product (front only) and a two-sided one
// (front + back), so sellers see both how items group and how designBack is used.
var orderImportTemplateSample = [][]string{
	{"SELLER01", "acc-001", "Etsy-Demo", "2026-08-20", "John Doe", "12 Main St", "", "Austin", "TX", "73301", "US",
		"Etsy-9001", "WOOD-01", "IMG-9001", "1",
		"https://designs.example.com/9001-front.png", "", "https://mockups.example.com/etsy-9001-1.png", "Hello",
		"+1900000000", "First order"},
	{"SELLER01", "acc-001", "Etsy-Demo", "2026-08-20", "John Doe", "12 Main St", "", "Austin", "TX", "73301", "US",
		"Etsy-9001", "MICA-02", "IMG-9002", "2",
		"https://designs.example.com/9002-front.png", "https://designs.example.com/9002-back.png",
		"https://mockups.example.com/etsy-9001-2.png", "",
		"+1900000000", ""},
}

// orderImportTemplateWidths sets per-column Excel widths (in characters): wide for
// the design/mockup URLs and addresses, compact for quantity / zip.
var orderImportTemplateWidths = []float64{
	12, 12, 14, 12, 18,
	26, 20, 14, 10, 10, 10,
	14, 14, 14, 10,
	40, 40, 40, 18,
	16, 22,
}

// OrderImportTemplateXLSX renders the order-import template as a real .xlsx
// workbook. Columns split cleanly in Excel on any locale — a comma CSV opened
// with everything crammed into column A on machines whose list separator is ";"
// — and the Vietnamese header "Mã ảnh" needs no BOM, so nothing comes out garbled.
func (s *ImportService) OrderImportTemplateXLSX() ([]byte, string, error) {
	grid := append([][]string{orderImportTemplateHeaders}, orderImportTemplateSample...)
	data, err := buildTemplateXLSX("Đơn hàng", grid, orderImportTemplateWidths)
	if err != nil {
		return nil, "", err
	}
	return data, "order-import-template.xlsx", nil
}

// HeaderReport is what the parser learned about a file's header row: which
// required columns were absent, which columns are recognised-but-retired, and
// which are not recognised at all.
//
// It exists because the old parser silently ignored every header it did not
// know. A renamed template therefore produced "23 rows are missing StoreOrderID"
// instead of "this file uses different column names" — the single most expensive
// failure mode this import has, because nothing in the message points at the
// actual cause.
type HeaderReport struct {
	Missing []string `json:"missing,omitempty"` // required columns not present (human labels)
	Retired []string `json:"retired,omitempty"` // recognised but no longer imported
	Unknown []string `json:"unknown,omitempty"` // not recognised at all (raw labels)
	Present []string `json:"present,omitempty"` // field ids the header row actually carried
}

// Has reports whether the file carried a column for this field id.
func (h HeaderReport) Has(fieldID string) bool {
	for _, id := range h.Present {
		if id == fieldID {
			return true
		}
	}
	return false
}

// ParseCSV reads a CSV stream into rows using a flexible header mapping.
func ParseCSV(r io.Reader) ([]ImportRow, HeaderReport, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, HeaderReport{}, apperr.BadRequest("could not parse CSV: " + err.Error())
	}
	return rowsFromRecords("CSV", records)
}

// ParseXLSX reads the first worksheet of an .xlsx/.xlsm stream into rows using
// the same flexible header mapping as ParseCSV (row 1 = header).
func ParseXLSX(r io.Reader) ([]ImportRow, HeaderReport, error) {
	f, err := excelize.OpenReader(r)
	if err != nil {
		return nil, HeaderReport{}, apperr.BadRequest("could not parse XLSX: " + err.Error())
	}
	defer f.Close()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, HeaderReport{}, apperr.BadRequest("XLSX has no worksheets")
	}
	records, err := f.GetRows(sheets[0])
	if err != nil {
		return nil, HeaderReport{}, apperr.BadRequest("could not read XLSX rows: " + err.Error())
	}
	return rowsFromRecords("XLSX", records)
}

// rowsFromRecords maps a header-plus-data grid (from CSV or XLSX) into ImportRows
// and reports what it made of the header row (see HeaderReport).
// kind is only used to label parse errors ("CSV" / "XLSX").
func rowsFromRecords(kind string, records [][]string) ([]ImportRow, HeaderReport, error) {
	if len(records) < 2 {
		return nil, HeaderReport{}, apperr.BadRequest(kind + " must contain a header row and at least one data row")
	}

	header := records[0]
	setters := make([]func(*ImportRow, string), len(header))
	seen := map[string]bool{}
	var report HeaderReport
	for i, h := range header {
		raw := strings.TrimSpace(h)
		key := headerKey(h)
		if key == "" {
			continue // a blank spacer column is not a mistake
		}
		if fn, ok := headerToField[key]; ok {
			setters[i] = fn.set
			if !seen[fn.id] {
				seen[fn.id] = true
				report.Present = append(report.Present, fn.id)
			}
			continue
		}
		if label, ok := retiredHeaders[key]; ok {
			report.Retired = appendDistinct(report.Retired, label)
			continue
		}
		report.Unknown = appendDistinct(report.Unknown, raw)
	}
	for _, id := range requiredHeaderFields {
		if !seen[id] {
			report.Missing = append(report.Missing, headerLabel(id))
		}
	}

	rows := make([]ImportRow, 0, len(records)-1)
	for _, rec := range records[1:] {
		var row ImportRow
		for i, cell := range rec {
			if i < len(setters) && setters[i] != nil {
				setters[i](&row, strings.TrimSpace(cell))
			}
		}
		if isBlankRow(row) {
			continue // trailing empty rows are what Excel leaves behind, not data
		}
		rows = append(rows, row)
	}
	return rows, report, nil
}

func appendDistinct(list []string, v string) []string {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return list
		}
	}
	return append(list, v)
}

// isBlankRow reports a row where every column the parser understood is empty.
// Excel files routinely carry a tail of formatted-but-empty rows; importing them
// would produce a screenful of "ORDER ID is required" errors for rows the seller
// never typed anything into.
func isBlankRow(r ImportRow) bool {
	return strings.TrimSpace(r.StoreOrderID) == "" && strings.TrimSpace(r.SKU) == "" &&
		int(r.Quantity) == 0 && strings.TrimSpace(r.ShippingName) == "" &&
		strings.TrimSpace(r.ShippingAddress1) == "" && strings.TrimSpace(r.ShippingCountry) == "" &&
		strings.TrimSpace(r.ImageCode) == "" && strings.TrimSpace(r.Mockup) == "" &&
		strings.TrimSpace(r.FrontDesignValue()) == "" && strings.TrimSpace(r.Note) == "" &&
		strings.TrimSpace(r.SellerRef) == "" && strings.TrimSpace(r.OrderDateRaw) == ""
}

// skuInfoForRows bulk-loads SKUInfo (id + mapped-material count) for every
// distinct SKU code in the file — one query instead of a FindByCode +
// CountMaterials probe per row.
func (s *ImportService) skuInfoForRows(rows []ImportRow) (map[string]repositories.SKUInfo, error) {
	seen := map[string]bool{}
	codes := make([]string, 0, len(rows))
	for _, row := range rows {
		code := models.NormalizeCode(row.SKU)
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		codes = append(codes, code)
	}
	return s.repo.SKU.InfoByCodes(codes)
}

// validateRow checks a single row and returns a blocking error (or nil). The
// rowNumber is 1-based across data rows (matching the wireframe error table).
// zipStateSwapped reports the classic filling mistake: the state sits in the ZIP
// column and the ZIP in the province column.
//
// Why it happens: the template lists ShippingCity | ShippingZip | ShippingProvince,
// while every address on earth reads "City, State ZIP". Anyone filling the sheet
// by eye swaps the last two, and nothing downstream notices — both are free-text
// columns that get printed straight onto the label. The parcel then goes out with
// a state where the postcode should be, and comes back.
//
// Deliberately narrow, because the same columns carry non-US addresses too:
// Canadian postal codes contain letters, the UK has no state at all. So flag only
// the combination that cannot be anything else — a ZIP column holding exactly two
// letters (no postal system uses that) AND a province column holding only digits
// (no province name does).
func zipStateSwapped(zip, province string) bool {
	zip = strings.TrimSpace(zip)
	province = strings.TrimSpace(province)
	if len(zip) != 2 {
		return false
	}
	for _, r := range zip {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	// ZIP+4 is written "55112-1050"; drop the separator before the digit check.
	digits := strings.ReplaceAll(province, "-", "")
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// sellerRefMatches reports whether the file's "Seller ID" cell refers to the
// seller this import is running for. An empty cell always matches — here the
// column is a cross-check, not a selector. Accepts the seller code (see
// sameSellerCode) or the seller name — never the internal database id: a "6"
// that Excel made out of "006" would otherwise pass for whichever seller has id 6.
func sellerRefMatches(ref string, sel repositories.SellerIdentity) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return true
	}
	return sameSellerCode(ref, sel.Code) || strings.EqualFold(ref, strings.TrimSpace(sel.Name))
}

// skus is the pre-fetched SKUInfo map for the whole file (see skuInfoForRows).
// seller is the account the file is being imported into, used to cross-check the
// "Seller ID" column.
func (s *ImportService) validateRow(rowNumber int, row ImportRow, skus map[string]repositories.SKUInfo, seller repositories.SellerIdentity) *models.ImportError {
	mkErr := func(field, code, msg, suggestion string) *models.ImportError {
		return &models.ImportError{
			RowNumber: rowNumber, StoreOrderID: row.StoreOrderID, SKU: row.SKU,
			Field: field, ErrorCode: code, Message: msg, Suggestion: suggestion,
		}
	}

	// The file says it belongs to a different seller than the one selected. This
	// blocks rather than warns: committing a file onto the wrong account creates
	// real orders under a seller who never sold them, and the only way back is
	// deleting orders that production may already have picked up.
	if !sellerRefMatches(row.SellerRef, seller) {
		return mkErr("Seller ID", "SELLER_MISMATCH",
			"Seller ID trong file (\""+strings.TrimSpace(row.SellerRef)+"\") không khớp seller đang import ("+seller.Code+")",
			"Chọn đúng seller ở ô \"Seller\" phía trên, hoặc sửa cột Seller ID trong file")
	}
	if _, _, err := ParseOrderDate(row.OrderDateRaw); err != nil {
		return mkErr("DATE", "DATE_INVALID",
			"Cột DATE không đọc được: \""+strings.TrimSpace(row.OrderDateRaw)+"\"",
			"Dùng định dạng ngày/tháng/năm (20/08/2026) hoặc 2026-08-20")
	}
	if strings.TrimSpace(row.StoreOrderID) == "" {
		return mkErr("StoreOrderID", "ORD_MISSING_ID", "ORDER ID is required", "Provide the store order id")
	}
	if int(row.Quantity) < 1 {
		return mkErr("Quantity", "QTY_INVALID", "Quantity must be a positive integer", "Set quantity >= 1")
	}
	if strings.TrimSpace(row.SKU) == "" {
		return mkErr("SKU", "SKU_MISSING", "SKU is required", "Provide a SKU")
	}
	// A SKU is only "mapped" once it exists AND is linked to at least one material —
	// materials are the axis production batches around, so an order can only proceed
	// when the system knows the SKU's material(s).
	info, ok := skus[models.NormalizeCode(row.SKU)]
	if !ok {
		return mkErr("SKU", "SKU_UNMAPPED", "SKU chưa được setup nguyên vật liệu (chưa có trong master data)",
			"Vào Master Data → Import Excel vận hành cũ hoặc tạo SKU và gán nguyên vật liệu")
	}
	if info.MaterialCount == 0 {
		return mkErr("SKU", "SKU_NO_MATERIAL", "SKU đã có nhưng chưa gán nguyên vật liệu (Loại VL)",
			"Vào Master Data → Mapping để gán nguyên vật liệu cho SKU này")
	}
	// Basic shipping validation.
	if strings.TrimSpace(row.ShippingName) == "" || strings.TrimSpace(row.ShippingAddress1) == "" ||
		strings.TrimSpace(row.ShippingCountry) == "" {
		return mkErr("ShippingAddress1", "ADDR_INVALID", "Thiếu tên người nhận / Địa chỉ nhận / Quốc Gia",
			"Điền đủ 3 cột: name, Địa chỉ nhận, Quốc Gia")
	}
	// Mockup URL: blocking only if present but malformed (missing mockup is handled
	// as a non-blocking required-attention note at commit time).
	if m := strings.TrimSpace(row.Mockup); m != "" {
		if !isValidHTTPURL(m) {
			return mkErr("Mockup", "MOCKUP_INVALID", "Mockup URL is not a valid http(s) URL",
				"Request seller gửi lại link mockup")
		}
	}
	// Design (front/single): only blocking if provided *as a URL* but malformed. A
	// bare design reference code (e.g. "design-a") is allowed — Design may be a code,
	// not a link — so we only validate values that clearly attempt to be a URL.
	if d := row.FrontDesignValue(); looksLikeURL(d) && !isValidHTTPURL(d) {
		return mkErr("Front Design", "DESIGN_INVALID", "Front Design URL is not a valid http(s) URL",
			"Sửa lại link design mặt trước hoặc để trống nếu chưa có")
	}
	// Back design is optional (only two-sided products). Same rule: validate only if
	// it looks like a URL. It must not be identical to the front design.
	if b := strings.TrimSpace(row.BackDesign); b != "" {
		if looksLikeURL(b) && !isValidHTTPURL(b) {
			return mkErr("Back Design", "BACK_DESIGN_INVALID", "Back Design URL is not a valid http(s) URL",
				"Sửa lại link design mặt sau hoặc để trống nếu sản phẩm chỉ có 1 mặt")
		}
		if b == row.FrontDesignValue() {
			return mkErr("Back Design", "DESIGN_SIDE_DUP", "Link design mặt trước và mặt sau đang giống nhau",
				"Kiểm tra lại — mỗi mặt phải là file design riêng, hoặc để trống mặt sau")
		}
	}
	// StoreOrderID uniqueness is intentionally NOT enforced here: a store order id
	// is a repeatable reference label (many items per order, and the same id may
	// legitimately recur across imports). Every row still becomes its own item with
	// its own unique InternalCode. A recurring StoreOrderID is surfaced by Preview
	// as a non-blocking warning (so staff can double-check with the customer), never
	// a blocking error.
	return nil
}

// Preview validates every row, persists the valid rows on an ImportJob (status
// PREVIEW) and returns the per-row errors. Nothing is created in the orders
// tables yet — that happens on Commit.
//
// hdr carries what the parser made of the header row. A file missing a required
// column is rejected here as a whole-file error: the alternative is one identical
// row error per line, which tells the operator nothing about the actual problem.
func (s *ImportService) Preview(actor Actor, sellerID uint, source, filename string, rows []ImportRow, hdr HeaderReport) (*PreviewResult, error) {
	// One light lookup: existence check AND the code needed to cross-check the
	// file's "Seller ID" column. FindByID would also preload the seller's stores.
	seller, found, err := s.repo.Seller.IdentityByID(sellerID)
	if err != nil || !found {
		return nil, apperr.BadRequest("seller_id does not reference an existing seller")
	}
	if err := checkImportShape(rows, hdr); err != nil {
		return nil, err
	}
	skus, err := s.skuInfoForRows(rows)
	if err != nil {
		return nil, apperr.Internal("could not look up SKUs").Wrap(err)
	}
	file := inspectImportFile(rows, hdr)
	res, err := s.previewSellerRows(actor, seller, source, filename, numberRows(rows), file, skus)
	if err != nil {
		return nil, err
	}
	res.Warnings = append(append([]models.ImportError{}, file.warnings...), res.Warnings...)
	res.Headers = hdr
	return res, nil
}

// checkImportShape rejects a file that cannot be previewed at all.
func checkImportShape(rows []ImportRow, hdr HeaderReport) error {
	if len(hdr.Missing) > 0 {
		msg := "File sai template: thiếu cột " + strings.Join(hdr.Missing, ", ")
		if len(hdr.Unknown) > 0 {
			msg += ". Cột không nhận diện được: " + strings.Join(hdr.Unknown, ", ")
		}
		return apperr.BadRequest(msg + ". Tải file mẫu mới ở nút \"Tải template\" rồi điền lại.")
	}
	if len(rows) == 0 {
		return apperr.BadRequest("no rows to import")
	}
	return nil
}

// numberedRow keeps a row's line number in the uploaded file, so a row split off
// into one seller's share still reports the number the operator sees.
type numberedRow struct {
	Number int
	Row    ImportRow
}

func numberRows(rows []ImportRow) []numberedRow {
	out := make([]numberedRow, len(rows))
	for i, row := range rows {
		out[i] = numberedRow{Number: i + 1, Row: row}
	}
	return out
}

// importFileFacts is what the file says as a whole — reported once, whichever
// seller its rows end up with.
type importFileFacts struct {
	hasDateColumn  bool
	hasPhoneColumn bool
	warnings       []models.ImportError // file-level notices, RowNumber 0
}

func inspectImportFile(rows []ImportRow, hdr HeaderReport) importFileFacts {
	today := AppDateString(time.Now())
	var f importFileFacts

	// A missing COLUMN is one fact about the file; a missing CELL is a fact about
	// one row. Reporting the first as the second buries the operator in one
	// identical warning per line and trains them to ignore the warning panel.
	// The data check covers the JSON/paste path, which has no header row at all.
	f.hasDateColumn = hdr.Has(fOrderDate)
	f.hasPhoneColumn = hdr.Has("ShippingPhone") || hdr.Has("Phone")
	for _, row := range rows {
		if !f.hasDateColumn && strings.TrimSpace(row.OrderDateRaw) != "" {
			f.hasDateColumn = true
		}
		if !f.hasPhoneColumn && row.RecipientPhone() != "" {
			f.hasPhoneColumn = true
		}
	}
	if !f.hasDateColumn {
		f.warnings = append(f.warnings, models.ImportError{
			Field: "DATE", ErrorCode: "DATE_COLUMN_MISSING",
			Message:    "File không có cột DATE — tất cả đơn sẽ lấy ngày import (" + today + ")",
			Suggestion: "Thêm cột DATE nếu muốn đơn nằm đúng ngày khách đặt",
		})
	}
	if !f.hasPhoneColumn {
		f.warnings = append(f.warnings, models.ImportError{
			Field: "ShippingPhone", ErrorCode: "PHONE_COLUMN_MISSING",
			Message:    "File không có số điện thoại người nhận ở bất kỳ dòng nào",
			Suggestion: "Nhiều hãng vận chuyển bắt buộc có số — bổ sung cột ShippingPhone",
		})
	}
	if len(hdr.Retired) > 0 {
		f.warnings = append(f.warnings, models.ImportError{
			Field: "Header", ErrorCode: "COL_RETIRED",
			Message:    "Các cột không còn dùng, hệ thống bỏ qua: " + strings.Join(hdr.Retired, ", "),
			Suggestion: "Có thể xoá các cột này khỏi file cho gọn",
		})
	}
	if len(hdr.Unknown) > 0 {
		f.warnings = append(f.warnings, models.ImportError{
			Field: "Header", ErrorCode: "COL_UNKNOWN",
			Message:    "Cột không nhận diện được (dữ liệu trong cột này KHÔNG được nhập): " + strings.Join(hdr.Unknown, ", "),
			Suggestion: "Kiểm tra chính tả tên cột, hoặc tải lại file mẫu mới nhất",
		})
	}
	return f
}

// previewSellerRows validates one seller's rows and stores them on a PREVIEW
// job. It returns row-level warnings only; the file-level ones in file are the
// caller's to report, once. skus is the SKU lookup for the whole file.
func (s *ImportService) previewSellerRows(actor Actor, seller repositories.SellerIdentity, source, filename string, rows []numberedRow, file importFileFacts, skus map[string]repositories.SKUInfo) (*PreviewResult, error) {
	sellerID := seller.ID
	// Which StoreOrderIDs already exist for this seller — one query, so the
	// per-row loop below never touches the database.
	storeOrderIDs := make([]string, 0, len(rows))
	for _, nr := range rows {
		if id := strings.TrimSpace(nr.Row.StoreOrderID); id != "" {
			storeOrderIDs = append(storeOrderIDs, id)
		}
	}
	existingStoreOrders, err := s.repo.Order.ExistingStoreOrderIDs(sellerID, storeOrderIDs)
	if err != nil {
		return nil, apperr.Internal("could not check existing store orders").Wrap(err)
	}

	var validRows []ImportRow
	var importErrors []models.ImportError
	var warnings []models.ImportError
	orderSet := map[string]bool{}

	// Date agreed per order: rows sharing an ORDER ID become one order, which can
	// only have one business day. First row wins; a disagreement is surfaced.
	orderDates := map[string]string{}

	today := AppDateString(time.Now())
	oldestSane := AppDateString(time.Now().AddDate(0, 0, -365))
	hasDateColumn, hasPhoneColumn := file.hasDateColumn, file.hasPhoneColumn

	mkWarn := func(rowNumber int, row ImportRow, field, code, msg, suggestion string) {
		warnings = append(warnings, models.ImportError{
			RowNumber: rowNumber, StoreOrderID: row.StoreOrderID, SKU: row.SKU,
			Field: field, ErrorCode: code, Message: msg, Suggestion: suggestion,
		})
	}

	for _, nr := range rows {
		n, row := nr.Number, nr.Row
		if e := s.validateRow(n, row, skus, seller); e != nil {
			importErrors = append(importErrors, *e)
			continue
		}
		validRows = append(validRows, row)
		key := strings.ToLower(strings.TrimSpace(row.StoreOrderID))
		orderSet[key] = true

		// ---- DATE (business day of the order) ----
		date, ambiguous, _ := ParseOrderDate(row.OrderDateRaw) // already validated above
		switch {
		case date == "":
			// Only worth a row-level note when the file HAS the column and this row
			// left it blank; the whole-file case was reported once, above.
			if hasDateColumn {
				mkWarn(n, row, "DATE", "DATE_EMPTY",
					"Cột DATE để trống — đơn sẽ lấy ngày import ("+today+")",
					"Điền ngày đặt hàng nếu muốn đơn nằm đúng ngày của nó")
			}
			date = today
		case ambiguous:
			mkWarn(n, row, "DATE", "DATE_AMBIGUOUS",
				"Ngày \""+strings.TrimSpace(row.OrderDateRaw)+"\" đọc theo kiểu ngày/tháng → "+date+" (có thể khách định ghi tháng/ngày)",
				"Ghi rõ dạng 2026-08-20 để không nhầm")
		case date > today:
			mkWarn(n, row, "DATE", "DATE_FUTURE",
				"Ngày đặt "+date+" nằm ở tương lai so với hôm nay ("+today+")",
				"Kiểm tra lại năm/tháng trong file")
		case date < oldestSane:
			mkWarn(n, row, "DATE", "DATE_TOO_OLD",
				"Ngày đặt "+date+" cách đây hơn 1 năm",
				"Kiểm tra lại năm trong file")
		}
		if prev, ok := orderDates[key]; ok {
			if prev != date {
				mkWarn(n, row, "DATE", "DATE_CONFLICT",
					"Cùng ORDER ID nhưng DATE khác nhau ("+prev+" vs "+date+") — đơn sẽ dùng "+prev,
					"Sửa cho các dòng cùng ORDER ID có cùng ngày")
			}
		} else {
			orderDates[key] = date
		}

		// Non-blocking heads-up: this StoreOrderID already exists for the seller
		// from an earlier import. We still import it as a brand-new, independent
		// order with its own internal code — a store order id is a repeatable label
		// — but flag the row so staff can confirm with the customer it isn't an
		// accidental re-send.
		if existingStoreOrders[strings.TrimSpace(row.StoreOrderID)] {
			mkWarn(n, row, "StoreOrderID", "ORD_DUPLICATE",
				"ORDER ID đã tồn tại cho seller này — không chặn, kiểm tra kẻo trùng",
				"Xác nhận với khách nếu đây là đơn đã có; nếu đúng là đơn mới thì bỏ qua")
		}
		// Cột ZIP đang giữ mã bang và cột Bang đang giữ ZIP — xem zipStateSwapped.
		// Cảnh báo chứ không chặn: máy chỉ SUY ĐOÁN từ hình dạng chuỗi, còn người
		// nhập mới biết chắc. Nhưng phải nói ra, vì không nói thì sai này lặng lẽ
		// đi thẳng lên nhãn gửi hàng và chỉ lộ khi kiện bị trả về.
		if zipStateSwapped(row.ShippingZip, row.ShippingProvince) {
			mkWarn(n, row, "Zipcode", "ADDR_ZIP_STATE_SWAPPED",
				"Có vẻ Zipcode và Mã vùng bị đảo cột: Zipcode=\""+strings.TrimSpace(row.ShippingZip)+
					"\" (giống mã bang), Mã vùng=\""+strings.TrimSpace(row.ShippingProvince)+"\" (giống mã ZIP)",
				"Đổi chỗ hai cột: Zipcode là mã bưu chính (số), Mã vùng là bang/tỉnh")
		}
		// No phone at all means the carrier has no way to reach the recipient.
		if hasPhoneColumn && row.RecipientPhone() == "" {
			mkWarn(n, row, "ShippingPhone", "PHONE_MISSING",
				"Đơn không có số điện thoại người nhận",
				"Điền cột ShippingPhone — nhiều hãng vận chuyển bắt buộc có số")
		}
	}

	raw, _ := models.ToJSONB(validRows)
	job := &models.ImportJob{
		SellerID:    &sellerID,
		Filename:    filename,
		Source:      source,
		Status:      models.ImportPreview,
		TotalRows:   len(rows),
		ValidRows:   len(validRows),
		ErrorRows:   len(importErrors),
		RawRows:     raw,
		CreatedByID: actor.IDPtr(),
	}
	if err := s.repo.Import.Create(job); err != nil {
		return nil, apperr.Internal("could not create import job").Wrap(err)
	}
	for i := range importErrors {
		importErrors[i].ImportJobID = job.ID
	}
	if err := s.repo.Import.CreateErrors(importErrors); err != nil {
		return nil, apperr.Internal("could not store import errors").Wrap(err)
	}

	s.audit.Log(actor, "IMPORT_PREVIEW", "import_job", &job.ID,
		fmt.Sprintf("Previewed import: %d rows, %d valid, %d errors", len(rows), len(validRows), len(importErrors)), nil)

	return &PreviewResult{
		ImportJobID: job.ID, Status: job.Status, TotalRows: len(rows),
		OrderCount: len(orderSet), ValidRows: len(validRows), ErrorRows: len(importErrors),
		Errors: importErrors, Warnings: warnings,
	}, nil
}

// Commit turns a PREVIEW import job's stored valid rows into orders + items.
// Rows sharing a StoreOrderID become one order with many items.
func (s *ImportService) Commit(actor Actor, jobID uint) (*models.ImportJob, error) {
	// Per-phase timings, logged as one line at the end. A commit that runs long
	// is otherwise a black box: the phases below cost wildly different amounts
	// depending on the file (a wide orders insert vs. thousands of item rows),
	// and guessing which one is the problem is how the wrong thing gets tuned.
	timer := newPhaseTimer()

	// Deliberately NOT FindByID: that preloads every stored validation error, and
	// a file that failed on hundreds of rows would drag all of them across the
	// wire for a path that only needs the job header and its rows.
	job, err := s.repo.Import.FindForCommit(jobID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Import job not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	timer.mark("read-job")
	if job.Status != models.ImportPreview {
		return nil, apperr.Conflict("Import job is not in PREVIEW state")
	}
	if job.SellerID == nil {
		return nil, apperr.BadRequest("import job has no seller")
	}

	var rows []ImportRow
	if len(job.RawRows) > 0 {
		if err := json.Unmarshal(job.RawRows, &rows); err != nil {
			return nil, apperr.Internal("could not read stored rows").Wrap(err)
		}
	}
	if len(rows) == 0 {
		job.Status = models.ImportCommitted
		_ = s.repo.Import.MarkCommitted(job.ID, 0)
		return job, nil
	}

	// Group rows by StoreOrderID preserving order.
	type group struct {
		header ImportRow
		items  []ImportRow
	}
	groups := map[string]*group{}
	var orderKeys []string
	for _, row := range rows {
		key := strings.TrimSpace(row.StoreOrderID)
		g, ok := groups[key]
		if !ok {
			g = &group{header: row}
			groups[key] = g
			orderKeys = append(orderKeys, key)
		}
		g.items = append(g.items, row)
	}

	// One SKU lookup for the whole file (the preview already validated rows, but
	// master data may have changed since — the map reflects commit-time truth).
	skus, err := s.skuInfoForRows(rows)
	if err != nil {
		return nil, apperr.Internal("could not look up SKUs").Wrap(err)
	}
	timer.mark("sku-lookup")

	created := 0
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		// Everything below is built in memory and written in batches. Creating an
		// order at a time cost 5-6 statements PER ORDER (sequence, insert, code
		// stamp, items, assets, notes); a thousand-order file meant thousands of
		// round-trips, which against a remote database is tens of minutes. The
		// batched shape is a fixed handful of statements per few hundred rows.
		now := time.Now()
		importDate := AppDateString(now)

		// Each order sits on the business day from its own "DATE" column, so a file
		// uploaded late still files its orders under the day they were placed. Rows
		// sharing an ORDER ID are one order, so the group's first row decides.
		groupDates := make([]string, len(orderKeys))
		countByDate := map[string]int{}
		for i, key := range orderKeys {
			d, _, dateErr := ParseOrderDate(groups[key].header.OrderDateRaw)
			// Preview already blocks unparseable dates; a leftover bad value (an
			// older preview job committed after this deploy) falls back to today
			// rather than failing a commit the operator has already confirmed.
			if d == "" || dateErr != nil {
				d = importDate
			}
			groupDates[i] = d
			countByDate[d]++
		}

		// Reserve one block per distinct day. The days are sorted so concurrent
		// commits always lock daily_counters rows in the same order — two files
		// spanning the same two days would otherwise be able to deadlock each other.
		dates := make([]string, 0, len(countByDate))
		for d := range countByDate {
			dates = append(dates, d)
		}
		sort.Strings(dates)
		nextSeq := make(map[string]int, len(dates))
		for _, d := range dates {
			lastSeq, seqErr := txRepo.Order.ReserveDailySeq(d, countByDate[d], now)
			if seqErr != nil {
				return seqErr
			}
			nextSeq[d] = lastSeq - countByDate[d] + 1
		}

		orders := make([]models.Order, 0, len(orderKeys))
		for i, key := range orderKeys {
			g := groups[key]
			// StoreOrderID is a repeatable reference label, not a key: always create
			// a fresh, independent order with its own system-generated internal code.
			// The same store order id arriving again (a later import) simply becomes
			// another order — never an overwrite of an existing one.
			order := models.Order{}
			order.StoreOrderID = key
			order.StoreOrderRef = key
			order.SellerID = *job.SellerID
			order.StoreName = g.header.StoreName
			order.Account = g.header.Account
			order.ShippingName = g.header.ShippingName
			order.ShippingAddress1 = g.header.ShippingAddress1
			order.ShippingAddress2 = g.header.ShippingAddress2
			order.ShippingCity = g.header.ShippingCity
			order.ShippingZip = g.header.ShippingZip
			order.ShippingProvince = g.header.ShippingProvince
			order.ShippingCountry = g.header.ShippingCountry
			order.ShippingPhone = g.header.RecipientPhone()
			order.Note = g.header.Note
			order.SellerStatus = models.SellerStatusProduction
			// (Re)enter the review queue with a clean slate — must be reviewed
			// before entering the design/production flow.
			order.ReviewStatus = models.ReviewPending
			order.CancellationStatus = models.CancellationNone
			order.TrackingStatus = models.TrackingNone
			order.ImportJobID = &job.ID
			order.CreatedByID = actor.IDPtr()
			// "STT trong ngày" comes out of that day's reserved block, in file order.
			order.OrderDate = groupDates[i]
			order.DailySeq = nextSeq[groupDates[i]]
			nextSeq[groupDates[i]]++
			// internal_code is UNIQUE and can only be computed from the DB id, so the
			// insert carries a placeholder that is already distinct per row —
			// inserting a batch of blanks would collide on the unique index. The
			// stamp right after the insert replaces every one of them, inside this
			// same transaction, so a placeholder can never outlive the commit.
			order.InternalCode = fmt.Sprintf("TMP-%d-%d", job.ID, i)
			orders = append(orders, order)
		}
		if err := txRepo.Order.CreateMany(orders, insertBatchSize(tx, &models.Order{})); err != nil {
			return err
		}
		timer.mark("insert-orders")

		// The internal code is derived from the DB-assigned id, so it can only be
		// stamped after the insert — but the database computes it itself, in one
		// statement for the whole file.
		if err := stampInternalCodes(tx, job.ID, orders); err != nil {
			return err
		}
		timer.mark("stamp-codes")

		items := make([]models.OrderItem, 0, len(rows))
		for i, key := range orderKeys {
			g := groups[key]
			orderID := orders[i].ID
			total := len(g.items)
			for lineNo, row := range g.items {
				skuCode := models.NormalizeCode(row.SKU)
				var skuID *uint
				if info, ok := skus[skuCode]; ok {
					id := info.ID
					skuID = &id
				}
				designStatus := models.DesignPending
				if strings.TrimSpace(row.Mockup) == "" {
					designStatus = models.DesignMissing
				}
				items = append(items, models.OrderItem{
					OrderID:        orderID,
					LineNo:         lineNo + 1,
					InternalCode:   itemInternalCode(orderID, lineNo+1, total),
					SKUID:          skuID,
					SKUCode:        skuCode,
					Quantity:       maxInt(int(row.Quantity), 1),
					ImageCode:      row.ImageCode,
					DesignURL:      row.FrontDesignValue(),
					BackDesignURL:  strings.TrimSpace(row.BackDesign),
					MockupURL:      row.Mockup,
					EngraveText:    row.EngraveText,
					InternalStatus: models.StatusPending,
					DesignStatus:   designStatus,
				})
			}
		}
		if len(items) > 0 {
			if err := tx.CreateInBatches(&items, insertBatchSize(tx, &models.OrderItem{})).Error; err != nil {
				return err
			}
		}
		timer.mark("insert-items")

		// Assets and required-attention notes need the item ids, so they follow —
		// again as two bulk inserts for the whole file rather than per order.
		var assets []models.ItemAsset
		var notes []models.Note
		for i := range items {
			item := &items[i]
			// Record design assets with their side so the versioned history keeps
			// front/back distinct. A one-sided item records a SINGLE design.
			if item.DesignURL != "" {
				side := models.DesignSideSingle
				if item.BackDesignURL != "" {
					side = models.DesignSideFront
				}
				assets = append(assets, models.ItemAsset{
					OrderItemID: item.ID, AssetType: "DESIGN", Side: side, URL: item.DesignURL, Version: 1,
					UploadedByID: actor.IDPtr(),
				})
			}
			if item.BackDesignURL != "" {
				assets = append(assets, models.ItemAsset{
					OrderItemID: item.ID, AssetType: "DESIGN", Side: models.DesignSideBack, URL: item.BackDesignURL, Version: 1,
					UploadedByID: actor.IDPtr(),
				})
			}
			if item.MockupURL != "" {
				assets = append(assets, models.ItemAsset{
					OrderItemID: item.ID, AssetType: "MOCKUP", URL: item.MockupURL, Version: 1,
					UploadedByID: actor.IDPtr(),
				})
			} else {
				// Missing mockup is a blocking-for-QC issue → required attention.
				notes = append(notes, models.Note{
					Title:               "Thiếu Mockup URL",
					Body:                "Item " + item.InternalCode + " chưa có mockup để QC đối chiếu.",
					ReasonCode:          "ART_MISSING",
					Severity:            models.SeverityHigh,
					Status:              models.NoteOpen,
					IsRequiredAttention: true,
					EntityType:          models.EntityOrderItem,
					EntityID:            &item.ID,
					OwnerRole:           models.RoleDesigner,
					CreatedByID:         actor.IDPtr(),
				})
			}
		}
		if len(assets) > 0 {
			if err := tx.CreateInBatches(&assets, insertBatchSize(tx, &models.ItemAsset{})).Error; err != nil {
				return err
			}
		}
		if len(notes) > 0 {
			if err := tx.CreateInBatches(&notes, insertBatchSize(tx, &models.Note{})).Error; err != nil {
				return err
			}
		}
		timer.mark("insert-assets-notes")
		created = len(orders)

		job.Status = models.ImportCommitted
		job.CreatedCount = created
		// Only the two columns that changed. tx.Save(job) would rewrite the whole
		// row — including RawRows, the JSONB blob holding every row of the uploaded
		// file — sending the entire file back to the database to record a status.
		if err := txRepo.Import.MarkCommitted(job.ID, created); err != nil {
			return err
		}
		timer.mark("save-job")
		return nil
	})
	if err != nil {
		// Time the failure too: a commit that dies slowly says something different
		// about where it died than one that dies immediately.
		log.Printf("import commit job=%d FAILED rows=%d %s: %v", job.ID, len(rows), timer.summary(), err)
		return nil, apperr.Internal("could not commit import").Wrap(err)
	}
	log.Printf("import commit job=%d orders=%d rows=%d %s", job.ID, created, len(rows), timer.summary())

	s.audit.Log(actor, "IMPORT_COMMIT", "import_job", &job.ID,
		fmt.Sprintf("Committed import: created %d orders", created), nil)
	return job, nil
}

// Get returns an import job with its errors.
func (s *ImportService) Get(id uint) (*models.ImportJob, error) {
	job, err := s.repo.Import.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Import job not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return job, nil
}

// List returns import jobs.
func (s *ImportService) List(page repositories.Page) ([]models.ImportJob, int64, error) {
	return s.repo.Import.List(page.Normalize())
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// internalBaseCode returns the workshop-style 6-digit order base code (e.g.
// "100035"), derived from the order's DB id so it is globally unique, monotonic,
// and independent of the (freely repeating) StoreOrderID.
func internalBaseCode(orderID uint) string {
	return strconv.Itoa(internalCodeBase + int(orderID))
}

// itemInternalCode formats a workshop-style item code — "100035_1/5" — as
// base_position/total: the item's position within its order out of the order's
// total item count. This is the QR/tem code the workshop and scan stations read.
func itemInternalCode(orderID uint, pos, total int) string {
	return fmt.Sprintf("%s_%d/%d", internalBaseCode(orderID), pos, total)
}

// looksLikeURL reports whether v appears to be an attempted http(s) link (so it
// should be validated) rather than a bare reference code (left untouched).
func looksLikeURL(v string) bool {
	lv := strings.ToLower(strings.TrimSpace(v))
	return strings.HasPrefix(lv, "http://") || strings.HasPrefix(lv, "https://") || strings.Contains(v, "://")
}

// isValidHTTPURL reports whether v parses as an absolute http(s) URL with a host.
func isValidHTTPURL(v string) bool {
	u, err := url.ParseRequestURI(strings.TrimSpace(v))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// phaseTimer records how long each stage of a long operation took, so the log
// line at the end says WHERE the time went instead of only how much there was.
type phaseTimer struct {
	start  time.Time
	last   time.Time
	phases []string
}

func newPhaseTimer() *phaseTimer {
	now := time.Now()
	return &phaseTimer{start: now, last: now}
}

// mark closes the phase that ended here and opens the next one.
func (t *phaseTimer) mark(name string) {
	now := time.Now()
	t.phases = append(t.phases, fmt.Sprintf("%s=%dms", name, now.Sub(t.last).Milliseconds()))
	t.last = now
}

// summary renders "total=1234ms read-job=12ms insert-orders=900ms …".
func (t *phaseTimer) summary() string {
	return fmt.Sprintf("total=%dms %s", time.Since(t.start).Milliseconds(), strings.Join(t.phases, " "))
}

// maxStmtParams caps how many bind parameters a single INSERT may carry.
//
// The commit's cost is not the NUMBER of statements — that has been batched for
// a while — but their SHAPE. A parameterised statement costs the database
// roughly in proportion to its parameter count (parse + plan), and the same
// count has to cross the wire. Orders are a 51-column table, so the old flat
// batch of 200 rows produced a ~10,000-placeholder INSERT: exactly the shape
// StatusHistoryRepository.CreateBulk already warns about, where a
// 7,000-placeholder statement measured ~1.4s against the remote database.
//
// Sizing each batch from the model's own width keeps every statement in the same
// modest range whatever the table, trading a few more round trips (cheap, and
// pipelined on one connection) for statements the database can parse quickly.
const maxStmtParams = 2000

// insertSchemaCache is GORM's own schema cache shape, kept package-level so a
// model is reflected over once per process rather than once per import.
var insertSchemaCache sync.Map

// insertBatchSize returns how many rows of `model` fit in one INSERT within the
// parameter budget. Unknown models fall back to a conservative batch.
func insertBatchSize(db *gorm.DB, model any) int {
	s, err := schema.Parse(model, &insertSchemaCache, db.NamingStrategy)
	if err != nil || len(s.DBNames) == 0 {
		return 40
	}
	n := maxStmtParams / len(s.DBNames)
	if n < 1 {
		return 1
	}
	if n > 200 {
		return 200
	}
	return n
}

// internalCodeBase is the offset that turns a DB id into the workshop's 6-digit
// order code (id 35 → "100035").
const internalCodeBase = 100000

// stampInternalCodes writes the id-derived order code onto the orders this job
// just inserted.
//
// The code can only be computed after the insert (it comes from the DB id) — but
// it does not have to be computed in Go: "100000 + id" is arithmetic the
// database can do for itself. So one statement carrying ONE parameter now covers
// the whole file, however many orders it holds, replacing an UPDATE … CASE that
// sent two parameters per order in chunks. It is scoped by import_job_id (an
// indexed column) and by the TMP- placeholder, so it can never reach an order
// from another import and re-running it is a no-op.
func stampInternalCodes(tx *gorm.DB, jobID uint, orders []models.Order) error {
	if len(orders) == 0 {
		return nil
	}
	sql := fmt.Sprintf(
		`UPDATE orders SET internal_code = CAST(%d + id AS TEXT)
		 WHERE import_job_id = ? AND internal_code LIKE 'TMP-%%'`, internalCodeBase)
	if err := tx.Exec(sql, jobID).Error; err != nil {
		return err
	}
	// Mirror the stamp onto the in-memory rows so the caller holds committed truth.
	for i := range orders {
		orders[i].InternalCode = internalBaseCode(orders[i].ID)
	}
	return nil
}
