package services

import (
	"sort"
	"strings"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Multi-seller import: ops upload ONE file mixing several sellers' orders and the
// "Seller ID" column decides which seller each row belongs to — instead of
// picking a seller and uploading once per seller. Each seller's share still runs
// through the exact single-seller preview (previewSellerRows) onto its own job,
// so every rule that holds for a one-seller file — SKU mapping, duplicate ORDER
// IDs per seller, dates — holds here unchanged.

// Row error codes for a "Seller ID" cell that names no seller.
const (
	sellerRefMissing   = "SELLER_MISSING"
	sellerRefUnknown   = "SELLER_UNKNOWN"
	sellerRefAmbiguous = "SELLER_AMBIGUOUS"
)

// sameSellerCode reports whether a "Seller ID" cell names this seller code.
// Codes compare normalised (case, spacing, diacritics). An all-digit code also
// matches its value without leading zeros: Excel turns a typed "006" into the
// number 6 unless the cell is formatted as text.
func sameSellerCode(ref, code string) bool {
	r, c := models.NormalizeCode(ref), models.NormalizeCode(code)
	if r == "" || c == "" {
		return false
	}
	if r == c {
		return true
	}
	return isAllDigits(r) && isAllDigits(c) && strings.TrimLeft(r, "0") == strings.TrimLeft(c, "0")
}

// resolveSellerRef finds the seller a "Seller ID" cell names, by CODE only —
// never the internal database id (a "6" would land on whichever seller has id 6)
// and never the display name (not unique, and not what the column holds). An
// exact code wins over a leading-zero match; two leading-zero matches (both
// "06" and "006" exist) are refused rather than guessed. errCode is empty on
// success.
func resolveSellerRef(ref string, sellers []repositories.SellerIdentity) (seller repositories.SellerIdentity, errCode string) {
	r := models.NormalizeCode(ref)
	if r == "" {
		return seller, sellerRefMissing
	}
	var loose []repositories.SellerIdentity
	for _, s := range sellers {
		if models.NormalizeCode(s.Code) == r {
			return s, ""
		}
		if sameSellerCode(r, s.Code) {
			loose = append(loose, s)
		}
	}
	switch len(loose) {
	case 1:
		return loose[0], ""
	case 0:
		return seller, sellerRefUnknown
	default:
		return seller, sellerRefAmbiguous
	}
}

func sellerRefError(n int, row ImportRow, code string) models.ImportError {
	e := models.ImportError{
		RowNumber: n, StoreOrderID: row.StoreOrderID, SKU: row.SKU,
		Field: "Seller ID", ErrorCode: code,
	}
	ref := strings.TrimSpace(row.SellerRef)
	switch code {
	case sellerRefMissing:
		e.Message = "Dòng không ghi Seller ID — không biết đơn này của seller nào"
		e.Suggestion = "Điền mã seller (vd. 005) vào cột Seller ID, hoặc chọn seller ở ô Seller rồi import riêng"
	case sellerRefAmbiguous:
		e.Message = "Seller ID \"" + ref + "\" khớp nhiều seller cùng lúc"
		e.Suggestion = "Ghi đủ mã seller và để ô dạng Text để Excel không bỏ số 0 ở đầu"
	default:
		e.Message = "Seller ID \"" + ref + "\" không khớp mã seller nào trong hệ thống"
		e.Suggestion = "Kiểm tra mã seller ở Master Data → Seller"
	}
	return e
}

// SellerImportShare is one seller's part of a multi-seller file: its own PREVIEW
// job, committed through the usual commit endpoint.
type SellerImportShare struct {
	SellerID    uint   `json:"seller_id"`
	SellerCode  string `json:"seller_code"`
	SellerName  string `json:"seller_name"`
	ImportJobID uint   `json:"import_job_id"`
	TotalRows   int    `json:"total_rows"`
	OrderCount  int    `json:"order_count"`
	ValidRows   int    `json:"valid_rows"`
	ErrorRows   int    `json:"error_rows"`
}

// MultiSellerPreviewResult is a PreviewResult for the whole file (same counters,
// errors and warnings, with row numbers from the file) plus the per-seller split.
// Rows whose "Seller ID" names no seller are counted in ErrorRows and listed in
// Errors, but belong to no job — UnassignedRows says how many.
type MultiSellerPreviewResult struct {
	Status         models.ImportJobStatus `json:"status"`
	TotalRows      int                    `json:"total_rows"`
	OrderCount     int                    `json:"order_count"`
	ValidRows      int                    `json:"valid_rows"`
	ErrorRows      int                    `json:"error_rows"`
	UnassignedRows int                    `json:"unassigned_rows"`
	Errors         []models.ImportError   `json:"errors"`
	Warnings       []models.ImportError   `json:"warnings"`
	Headers        HeaderReport           `json:"headers"`
	Sellers        []SellerImportShare    `json:"sellers"`
}

// PreviewBySellerColumn previews a file whose rows may belong to different
// sellers, assigning each row by its "Seller ID" column (see resolveSellerRef).
// It creates one PREVIEW job per seller present; nothing is committed.
func (s *ImportService) PreviewBySellerColumn(actor Actor, source, filename string, rows []ImportRow, hdr HeaderReport) (*MultiSellerPreviewResult, error) {
	if err := checkImportShape(rows, hdr); err != nil {
		return nil, err
	}
	// Without the column every row would come back "no Seller ID" — one fact
	// about the file, said once.
	if !hdr.Has(fSellerRef) && !anySellerRef(rows) {
		return nil, apperr.BadRequest("File không có cột Seller ID nên không tự chia được theo seller. " +
			"Thêm cột Seller ID, hoặc chọn một seller cụ thể ở ô Seller.")
	}
	sellers, err := s.repo.Seller.Identities()
	if err != nil {
		return nil, apperr.Internal("could not load sellers").Wrap(err)
	}
	skus, err := s.skuInfoForRows(rows)
	if err != nil {
		return nil, apperr.Internal("could not look up SKUs").Wrap(err)
	}
	file := inspectImportFile(rows, hdr)

	type share struct {
		seller repositories.SellerIdentity
		rows   []numberedRow
	}
	shares := map[uint]*share{}
	var unassigned []models.ImportError
	for _, nr := range numberRows(rows) {
		seller, code := resolveSellerRef(nr.Row.SellerRef, sellers)
		if code != "" {
			unassigned = append(unassigned, sellerRefError(nr.Number, nr.Row, code))
			continue
		}
		sh, ok := shares[seller.ID]
		if !ok {
			sh = &share{seller: seller}
			shares[seller.ID] = sh
		}
		sh.rows = append(sh.rows, nr)
	}
	ordered := make([]*share, 0, len(shares))
	for _, sh := range shares {
		ordered = append(ordered, sh)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].seller.Code < ordered[j].seller.Code })

	out := &MultiSellerPreviewResult{
		Status: models.ImportPreview, TotalRows: len(rows), Headers: hdr,
		UnassignedRows: len(unassigned), ErrorRows: len(unassigned),
		Errors: unassigned, Sellers: []SellerImportShare{},
	}
	var rowWarnings []models.ImportError
	for _, sh := range ordered {
		res, err := s.previewSellerRows(actor, sh.seller, source, filename, sh.rows, file, skus)
		if err != nil {
			return nil, err
		}
		out.Sellers = append(out.Sellers, SellerImportShare{
			SellerID: sh.seller.ID, SellerCode: sh.seller.Code, SellerName: sh.seller.Name,
			ImportJobID: res.ImportJobID, TotalRows: res.TotalRows, OrderCount: res.OrderCount,
			ValidRows: res.ValidRows, ErrorRows: res.ErrorRows,
		})
		out.OrderCount += res.OrderCount
		out.ValidRows += res.ValidRows
		out.ErrorRows += res.ErrorRows
		for _, e := range res.Errors {
			e.SellerCode = sh.seller.Code
			out.Errors = append(out.Errors, e)
		}
		for _, w := range res.Warnings {
			w.SellerCode = sh.seller.Code
			rowWarnings = append(rowWarnings, w)
		}
	}
	// Read in file order, not seller by seller: the operator fixes the file.
	sort.SliceStable(out.Errors, func(i, j int) bool { return out.Errors[i].RowNumber < out.Errors[j].RowNumber })
	sort.SliceStable(rowWarnings, func(i, j int) bool { return rowWarnings[i].RowNumber < rowWarnings[j].RowNumber })
	out.Warnings = append(append([]models.ImportError{}, file.warnings...), rowWarnings...)
	if out.Errors == nil {
		out.Errors = []models.ImportError{}
	}
	return out, nil
}

func anySellerRef(rows []ImportRow) bool {
	for _, row := range rows {
		if strings.TrimSpace(row.SellerRef) != "" {
			return true
		}
	}
	return false
}
