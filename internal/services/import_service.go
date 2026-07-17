package services

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"

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
type ImportRow struct {
	StoreOrderID     string  `json:"StoreOrderID"`
	Account          string  `json:"Account"`
	StoreName        string  `json:"StoreName"`
	ShippingMethod   string  `json:"ShippingMethod"`
	Quantity         FlexInt `json:"Quantity"`
	ProductName      string  `json:"ProductName"`
	VariantCode      string  `json:"VariantCode"`
	SKU              string  `json:"SKU"`
	ImageCode        string  `json:"ImageCode"` // "Mã ảnh"
	Design           string  `json:"Design"`
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
	ShippingEmail    string  `json:"ShippingEmail"`
	IOSS             string  `json:"IOSS"`
	Note             string  `json:"Note"`
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
		fn, ok := headerToField[normalizeHeader(k)]
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
		fn(r, strings.TrimSpace(s))
	}
	return nil
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

var headerToField = map[string]func(*ImportRow, string){
	"storeorderid":   func(r *ImportRow, v string) { r.StoreOrderID = v },
	"account":        func(r *ImportRow, v string) { r.Account = v },
	"storename":      func(r *ImportRow, v string) { r.StoreName = v },
	"shippingmethod": func(r *ImportRow, v string) { r.ShippingMethod = v },
	"quantity":       func(r *ImportRow, v string) { n, _ := strconv.Atoi(strings.TrimSpace(v)); r.Quantity = FlexInt(n) },
	"productname":    func(r *ImportRow, v string) { r.ProductName = v },
	"variantcode":    func(r *ImportRow, v string) { r.VariantCode = v },
	"sku":            func(r *ImportRow, v string) { r.SKU = v },
	// "Mã ảnh" — normalized VN header (diacritics preserved), plus safe aliases.
	"mãảnh":            func(r *ImportRow, v string) { r.ImageCode = v },
	"maanh":            func(r *ImportRow, v string) { r.ImageCode = v },
	"imagecode":        func(r *ImportRow, v string) { r.ImageCode = v },
	"design":           func(r *ImportRow, v string) { r.Design = v },
	"mockup":           func(r *ImportRow, v string) { r.Mockup = v },
	"mockupurl":        func(r *ImportRow, v string) { r.Mockup = v },
	"engravetext":      func(r *ImportRow, v string) { r.EngraveText = v },
	"shippingname":     func(r *ImportRow, v string) { r.ShippingName = v },
	"shippingaddress1": func(r *ImportRow, v string) { r.ShippingAddress1 = v },
	"shippingaddress2": func(r *ImportRow, v string) { r.ShippingAddress2 = v },
	"shippingcity":     func(r *ImportRow, v string) { r.ShippingCity = v },
	"shippingzip":      func(r *ImportRow, v string) { r.ShippingZip = v },
	"shippingprovince": func(r *ImportRow, v string) { r.ShippingProvince = v },
	"shippingcountry":  func(r *ImportRow, v string) { r.ShippingCountry = v },
	"shippingphone":    func(r *ImportRow, v string) { r.ShippingPhone = v },
	"shippingemail":    func(r *ImportRow, v string) { r.ShippingEmail = v },
	"ioss":             func(r *ImportRow, v string) { r.IOSS = v },
	"note":             func(r *ImportRow, v string) { r.Note = v },
}

// orderImportTemplateHeaders are the exact column labels the parser recognises
// (every entry normalizes to a headerToField key), in the canonical order sellers
// fill them in. Keep this in sync with the front-end IMPORT_COLUMNS list.
var orderImportTemplateHeaders = []string{
	"StoreOrderID", "Account", "StoreName", "ShippingMethod", "Quantity",
	"ProductName", "VariantCode", "SKU", "Mã ảnh", "Design", "Mockup",
	"EngraveText", "ShippingName", "ShippingAddress1", "ShippingAddress2",
	"ShippingCity", "ShippingZip", "ShippingProvince", "ShippingCountry",
	"ShippingPhone", "ShippingEmail", "IOSS", "Note",
}

// orderImportTemplateSample mirrors the two example rows the front-end used to
// ship in its client-side CSV, so the .xlsx download gives the same guidance.
var orderImportTemplateSample = [][]string{
	{"Etsy-9001", "acc-001", "Etsy-Demo", "Standard", "1", "Personalized Wood Sign", "VAR-1", "WOOD-01", "IMG-9001", "design-a", "https://mockups.example.com/etsy-9001-1.png", "Hello", "John Doe", "12 Main St", "", "Austin", "73301", "TX", "US", "+1900000000", "john@example.com", "", "First order"},
	{"Etsy-9001", "acc-001", "Etsy-Demo", "Standard", "2", "Mica Plate", "VAR-2", "MICA-02", "IMG-9002", "design-b", "https://mockups.example.com/etsy-9001-2.png", "", "John Doe", "12 Main St", "", "Austin", "73301", "TX", "US", "+1900000000", "john@example.com", "", ""},
}

// orderImportTemplateWidths sets per-column Excel widths (in characters): wide for
// the mockup URL / addresses / email, compact for quantity / zip.
var orderImportTemplateWidths = []float64{
	14, 12, 14, 14, 9, 24, 12, 14, 12, 12, 40, 16, 16, 22, 16, 14, 10, 12, 12, 16, 24, 10, 20,
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

// ParseCSV reads a CSV stream into rows using a flexible header mapping.
func ParseCSV(r io.Reader) ([]ImportRow, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, apperr.BadRequest("could not parse CSV: " + err.Error())
	}
	return rowsFromRecords("CSV", records)
}

// ParseXLSX reads the first worksheet of an .xlsx/.xlsm stream into rows using
// the same flexible header mapping as ParseCSV (row 1 = header).
func ParseXLSX(r io.Reader) ([]ImportRow, error) {
	f, err := excelize.OpenReader(r)
	if err != nil {
		return nil, apperr.BadRequest("could not parse XLSX: " + err.Error())
	}
	defer f.Close()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, apperr.BadRequest("XLSX has no worksheets")
	}
	records, err := f.GetRows(sheets[0])
	if err != nil {
		return nil, apperr.BadRequest("could not read XLSX rows: " + err.Error())
	}
	return rowsFromRecords("XLSX", records)
}

// rowsFromRecords maps a header-plus-data grid (from CSV or XLSX) into ImportRows.
// kind is only used to label parse errors ("CSV" / "XLSX").
func rowsFromRecords(kind string, records [][]string) ([]ImportRow, error) {
	if len(records) < 2 {
		return nil, apperr.BadRequest(kind + " must contain a header row and at least one data row")
	}

	header := records[0]
	setters := make([]func(*ImportRow, string), len(header))
	for i, h := range header {
		if fn, ok := headerToField[normalizeHeader(h)]; ok {
			setters[i] = fn
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
		rows = append(rows, row)
	}
	return rows, nil
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
// skus is the pre-fetched SKUInfo map for the whole file (see skuInfoForRows).
func (s *ImportService) validateRow(rowNumber int, row ImportRow, skus map[string]repositories.SKUInfo) *models.ImportError {
	mkErr := func(field, code, msg, suggestion string) *models.ImportError {
		return &models.ImportError{
			RowNumber: rowNumber, StoreOrderID: row.StoreOrderID, SKU: row.SKU,
			Field: field, ErrorCode: code, Message: msg, Suggestion: suggestion,
		}
	}

	if strings.TrimSpace(row.StoreOrderID) == "" {
		return mkErr("StoreOrderID", "ORD_MISSING_ID", "StoreOrderID is required", "Provide the store order id")
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
		return mkErr("ShippingAddress1", "ADDR_INVALID", "Shipping name, address1 and country are required",
			"Correct and validate shipping address")
	}
	// Mockup URL: blocking only if present but malformed (missing mockup is handled
	// as a non-blocking required-attention note at commit time).
	if m := strings.TrimSpace(row.Mockup); m != "" {
		if !isValidHTTPURL(m) {
			return mkErr("Mockup", "MOCKUP_INVALID", "Mockup URL is not a valid http(s) URL",
				"Request seller gửi lại link mockup")
		}
	}
	// Design: only blocking if it is provided *as a URL* but malformed. A bare
	// design reference code (e.g. "design-a") is allowed — Design may be a code,
	// not a link, so we only validate values that clearly attempt to be a URL.
	if d := strings.TrimSpace(row.Design); looksLikeURL(d) && !isValidHTTPURL(d) {
		return mkErr("Design", "DESIGN_INVALID", "Design URL is not a valid http(s) URL",
			"Sửa lại link design hoặc để trống nếu chưa có")
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
func (s *ImportService) Preview(actor Actor, sellerID uint, source, filename string, rows []ImportRow) (*PreviewResult, error) {
	if _, err := s.repo.Seller.FindByID(sellerID); err != nil {
		return nil, apperr.BadRequest("seller_id does not reference an existing seller")
	}
	if len(rows) == 0 {
		return nil, apperr.BadRequest("no rows to import")
	}

	// Prefetch the whole file's lookups in two queries: SKU info per distinct
	// code, and which StoreOrderIDs already exist for this seller. The per-row
	// loop below then never touches the database.
	skus, err := s.skuInfoForRows(rows)
	if err != nil {
		return nil, apperr.Internal("could not look up SKUs").Wrap(err)
	}
	storeOrderIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		if id := strings.TrimSpace(row.StoreOrderID); id != "" {
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

	for i, row := range rows {
		if e := s.validateRow(i+1, row, skus); e != nil {
			importErrors = append(importErrors, *e)
			continue
		}
		validRows = append(validRows, row)
		orderSet[strings.ToLower(strings.TrimSpace(row.StoreOrderID))] = true
		// Non-blocking heads-up: this StoreOrderID already exists for the seller
		// from an earlier import. We still import it as a brand-new, independent
		// order with its own internal code — a store order id is a repeatable label
		// — but flag the row so staff can confirm with the customer it isn't an
		// accidental re-send.
		if existingStoreOrders[strings.TrimSpace(row.StoreOrderID)] {
			warnings = append(warnings, models.ImportError{
				RowNumber: i + 1, StoreOrderID: row.StoreOrderID, SKU: row.SKU,
				Field: "StoreOrderID", ErrorCode: "ORD_DUPLICATE",
				Message:    "StoreOrderID đã tồn tại cho seller này — không chặn, kiểm tra kẻo trùng",
				Suggestion: "Xác nhận với khách nếu đây là đơn đã có; nếu đúng là đơn mới thì bỏ qua",
			})
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
	job, err := s.repo.Import.FindByID(jobID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Import job not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
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
		_ = s.repo.Import.Update(job)
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

	created := 0
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		for _, key := range orderKeys {
			g := groups[key]
			// StoreOrderID is a repeatable reference label, not a key: always create
			// a fresh, independent order with its own system-generated internal code.
			// The same store order id arriving again (a later import) simply becomes
			// another order — never an overwrite of an existing one.
			order := &models.Order{}
			order.StoreOrderID = key
			order.StoreOrderRef = key
			order.SellerID = *job.SellerID
			order.StoreName = g.header.StoreName
			order.Account = g.header.Account
			order.ShippingMethod = g.header.ShippingMethod
			order.ShippingName = g.header.ShippingName
			order.ShippingAddress1 = g.header.ShippingAddress1
			order.ShippingAddress2 = g.header.ShippingAddress2
			order.ShippingCity = g.header.ShippingCity
			order.ShippingZip = g.header.ShippingZip
			order.ShippingProvince = g.header.ShippingProvince
			order.ShippingCountry = g.header.ShippingCountry
			order.ShippingPhone = g.header.ShippingPhone
			order.ShippingEmail = g.header.ShippingEmail
			order.IOSS = g.header.IOSS
			order.Note = g.header.Note
			order.SellerStatus = models.SellerStatusProduction
			// (Re)enter the review queue with a clean slate — must be reviewed
			// before entering the design/production flow.
			order.ReviewStatus = models.ReviewPending
			order.ReviewedByID = nil
			order.ReviewedAt = nil
			order.ReviewNote = ""
			order.CancellationStatus = models.CancellationNone
			order.CancellationRequestedByID = nil
			order.CancellationRequestedAt = nil
			order.ImportJobID = &job.ID
			order.CreatedByID = actor.IDPtr()
			if err := txRepo.Order.Create(order); err != nil {
				return err
			}
			// The base code needs the DB-assigned id, so stamp it after Create.
			order.InternalCode = internalBaseCode(order.ID)
			if err := txRepo.Order.Update(order); err != nil {
				return err
			}

			// Build the order's items up front (the internal code only needs the
			// order id + position) and insert them in one statement; assets and
			// required-attention notes follow as two more bulk inserts.
			total := len(g.items)
			items := make([]models.OrderItem, 0, total)
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
					OrderID:        order.ID,
					LineNo:         lineNo + 1,
					InternalCode:   itemInternalCode(order.ID, lineNo+1, total),
					SKUID:          skuID,
					SKUCode:        skuCode,
					ProductName:    row.ProductName,
					VariantCode:    row.VariantCode,
					Quantity:       maxInt(int(row.Quantity), 1),
					ImageCode:      row.ImageCode,
					DesignURL:      row.Design,
					MockupURL:      row.Mockup,
					EngraveText:    row.EngraveText,
					InternalStatus: models.StatusPending,
					DesignStatus:   designStatus,
				})
			}
			if err := tx.Create(&items).Error; err != nil {
				return err
			}
			var assets []models.ItemAsset
			var notes []models.Note
			for i := range items {
				item := &items[i]
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
				if err := tx.Create(&assets).Error; err != nil {
					return err
				}
			}
			if len(notes) > 0 {
				if err := tx.Create(&notes).Error; err != nil {
					return err
				}
			}
			created++
		}

		job.Status = models.ImportCommitted
		job.CreatedCount = created
		return tx.Save(job).Error
	})
	if err != nil {
		return nil, apperr.Internal("could not commit import").Wrap(err)
	}

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
	return strconv.Itoa(100000 + int(orderID))
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
