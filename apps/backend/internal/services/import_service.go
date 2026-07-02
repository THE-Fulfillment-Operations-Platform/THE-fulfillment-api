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
	StoreName        string  `json:"StoreName"`
	ShippingMethod   string  `json:"ShippingMethod"`
	Quantity         FlexInt `json:"Quantity"`
	ProductName      string  `json:"ProductName"`
	VariantCode      string  `json:"VariantCode"`
	SKU              string  `json:"SKU"`
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

// PreviewResult is returned to the client after validation.
type PreviewResult struct {
	ImportJobID uint                   `json:"import_job_id"`
	Status      models.ImportJobStatus `json:"status"`
	TotalRows   int                    `json:"total_rows"`
	OrderCount  int                    `json:"order_count"`
	ValidRows   int                    `json:"valid_rows"`
	ErrorRows   int                    `json:"error_rows"`
	Errors      []models.ImportError   `json:"errors"`
}

// csvHeaderMap normalizes a header cell into a canonical key.
func normalizeHeader(h string) string {
	h = strings.TrimPrefix(h, string(rune(0xFEFF))) // strip UTF-8 BOM from Excel "CSV UTF-8" exports
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.NewReplacer(" ", "", "_", "", "-", "").Replace(h)
	return h
}

var headerToField = map[string]func(*ImportRow, string){
	"storeorderid":     func(r *ImportRow, v string) { r.StoreOrderID = v },
	"storename":        func(r *ImportRow, v string) { r.StoreName = v },
	"shippingmethod":   func(r *ImportRow, v string) { r.ShippingMethod = v },
	"quantity":         func(r *ImportRow, v string) { n, _ := strconv.Atoi(strings.TrimSpace(v)); r.Quantity = FlexInt(n) },
	"productname":      func(r *ImportRow, v string) { r.ProductName = v },
	"variantcode":      func(r *ImportRow, v string) { r.VariantCode = v },
	"sku":              func(r *ImportRow, v string) { r.SKU = v },
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

// validateRow checks a single row and returns a blocking error (or nil). The
// rowNumber is 1-based across data rows (matching the wireframe error table).
func (s *ImportService) validateRow(rowNumber int, sellerID uint, row ImportRow, seenInFile map[string]bool) *models.ImportError {
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
	sku, err := s.repo.SKU.FindByCode(strings.ToUpper(strings.TrimSpace(row.SKU)))
	if err != nil {
		return mkErr("SKU", "SKU_UNMAPPED", "SKU chưa được setup nguyên vật liệu (chưa có trong master data)",
			"Vào Master Data → Import Excel vận hành cũ hoặc tạo SKU và gán nguyên vật liệu")
	}
	if n, err := s.repo.SKU.CountMaterials(sku.ID); err == nil && n == 0 {
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
		if u, err := url.ParseRequestURI(m); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return mkErr("Mockup", "MOCKUP_INVALID", "Mockup URL is not a valid http(s) URL",
				"Request seller gửi lại link mockup")
		}
	}
	// Dedup within the file is fine (multiple items per order); dedup against the
	// database below.
	key := strings.ToLower(strings.TrimSpace(row.StoreOrderID))
	seenInFile[key] = true

	if _, err := s.repo.Order.FindBySellerAndStoreOrder(sellerID, strings.TrimSpace(row.StoreOrderID)); err == nil {
		return mkErr("StoreOrderID", "ORD_DUPLICATE", "An order with this StoreOrderID already exists for the seller",
			"Review source/idempotency")
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return mkErr("StoreOrderID", "LOOKUP_FAILED", "Could not check for duplicate order", "Retry import")
	}
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

	seenInFile := map[string]bool{}
	var validRows []ImportRow
	var importErrors []models.ImportError
	orderSet := map[string]bool{}

	for i, row := range rows {
		if e := s.validateRow(i+1, sellerID, row, seenInFile); e != nil {
			importErrors = append(importErrors, *e)
			continue
		}
		validRows = append(validRows, row)
		orderSet[strings.ToLower(strings.TrimSpace(row.StoreOrderID))] = true
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
		Errors: importErrors,
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

	created := 0
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		for _, key := range orderKeys {
			g := groups[key]
			order := &models.Order{
				StoreOrderID:     key,
				StoreOrderRef:    key,
				SellerID:         *job.SellerID,
				StoreName:        g.header.StoreName,
				ShippingMethod:   g.header.ShippingMethod,
				ShippingName:     g.header.ShippingName,
				ShippingAddress1: g.header.ShippingAddress1,
				ShippingAddress2: g.header.ShippingAddress2,
				ShippingCity:     g.header.ShippingCity,
				ShippingZip:      g.header.ShippingZip,
				ShippingProvince: g.header.ShippingProvince,
				ShippingCountry:  g.header.ShippingCountry,
				ShippingPhone:    g.header.ShippingPhone,
				ShippingEmail:    g.header.ShippingEmail,
				IOSS:             g.header.IOSS,
				Note:             g.header.Note,
				SellerStatus:     models.SellerStatusProduction,
				ImportJobID:      &job.ID,
				CreatedByID:      actor.IDPtr(),
			}
			if err := txRepo.Order.Create(order); err != nil {
				return err
			}
			order.InternalCode = fmt.Sprintf("ORD-%06d", order.ID)
			if err := txRepo.Order.Update(order); err != nil {
				return err
			}

			for lineNo, row := range g.items {
				skuCode := strings.ToUpper(strings.TrimSpace(row.SKU))
				sku, _ := txRepo.SKU.FindByCode(skuCode)
				var skuID *uint
				if sku != nil {
					skuID = &sku.ID
				}
				designStatus := models.DesignPending
				if strings.TrimSpace(row.Mockup) == "" {
					designStatus = models.DesignMissing
				}
				item := &models.OrderItem{
					OrderID:        order.ID,
					LineNo:         lineNo + 1,
					InternalCode:   fmt.Sprintf("ORD-%06d_%d", order.ID, lineNo+1),
					SKUID:          skuID,
					SKUCode:        skuCode,
					ProductName:    row.ProductName,
					VariantCode:    row.VariantCode,
					Quantity:       maxInt(int(row.Quantity), 1),
					DesignURL:      row.Design,
					MockupURL:      row.Mockup,
					EngraveText:    row.EngraveText,
					InternalStatus: models.StatusPending,
					DesignStatus:   designStatus,
				}
				if err := tx.Create(item).Error; err != nil {
					return err
				}
				if row.Mockup != "" {
					if err := tx.Create(&models.ItemAsset{
						OrderItemID: item.ID, AssetType: "MOCKUP", URL: row.Mockup, Version: 1,
						UploadedByID: actor.IDPtr(),
					}).Error; err != nil {
						return err
					}
				} else {
					// Missing mockup is a blocking-for-QC issue → required attention.
					if err := tx.Create(&models.Note{
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
					}).Error; err != nil {
						return err
					}
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
