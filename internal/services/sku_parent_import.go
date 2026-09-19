package services

import (
	"fmt"
	"io"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Parent SKU import — step 1 of the parent → child setup. A parent SKU groups the
// variants of one product: "Hộp nhựa" holds "Hộp nhựa bé", "Hộp nhựa lớn",
// "Hộp nhựa vuông"… This file only creates (or renames) the parents; step 2, the
// regular SKU import, then files children under them through its "SKU cha"
// column, and refuses a parent that isn't here yet.
//
// Preview → commit like the material-quota import: stateless, the client sends
// the rows back and commit re-analyses them inside its transaction.

const (
	errParentRowConflict = "PARENT_ROW_CONFLICT" // same code, different names/descriptions
	errNotParentFile     = "Đây là file SKU con (có cả cột SKU cha lẫn SKU) — dùng Bước 2 · Import SKU con"
)

// ParentSKURow is one line of the step-1 file.
type ParentSKURow struct {
	RowNumber   int    `json:"row_number"`
	SKU         string `json:"sku"`
	ProductName string `json:"product_name"`
	Description string `json:"description"`
}

type ParentSKUImportItem struct {
	Code               string `json:"code"`
	Name               string `json:"name"` // the code as typed in the file
	ProductName        string `json:"product_name"`
	Description        string `json:"description"`
	Exists             bool   `json:"exists"`
	CurrentProductName string `json:"current_product_name"`
	CurrentDescription string `json:"current_description"`
	// ChildCount: how many SKU con the parent already holds.
	ChildCount int    `json:"child_count"`
	Action     string `json:"action"` // CREATE | UPDATE | NOCHANGE
	RowNumbers []int  `json:"row_numbers"`
	skuID      uint
}

type ParentSKUImportSummary struct {
	TotalRows     int `json:"total_rows"`
	New           int `json:"new"`
	Updates       int `json:"updates"`
	Unchanged     int `json:"unchanged"`
	ErrorRows     int `json:"error_rows"`
	DuplicateRows int `json:"duplicate_rows"` // identical repeats folded into one
}

type ParentSKUImportApplied struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
}

type ParentSKUImportPreview struct {
	Filename string                  `json:"filename"`
	Items    []ParentSKUImportItem   `json:"items"`
	Errors   []LegacyRowError        `json:"errors"`
	Summary  ParentSKUImportSummary  `json:"summary"`
	Applied  *ParentSKUImportApplied `json:"applied,omitempty"`
}

// ParseParentSKUFile reads the step-1 file. The code column is "SKU cha" (or a
// plain "SKU"); "Tên sản phẩm" and "Mô tả" are optional. A file carrying BOTH a
// "SKU cha" and a "SKU" column is a step-2 children file uploaded in the wrong
// place — reading it here would make every parent take a child's name.
func ParseParentSKUFile(source string, r io.Reader) ([]ParentSKURow, error) {
	records, err := readLegacyGrid(source, r)
	if err != nil {
		return nil, err
	}
	if len(records) < 2 {
		return nil, apperr.BadRequest("File phải có dòng tiêu đề và ít nhất một dòng dữ liệu")
	}
	parentIdx, skuIdx, prodIdx, descIdx := -1, -1, -1, -1
	for i, h := range records[0] {
		n := normalizeLegacyHeader(h)
		switch {
		case parentIdx == -1 && legacyParentHeaders[n]:
			parentIdx = i
		case skuIdx == -1 && legacySKUHeaders[n]:
			skuIdx = i
		case prodIdx == -1 && legacyProductHeaders[n]:
			prodIdx = i
		case descIdx == -1 && legacySKUDescHeaders[n]:
			descIdx = i
		}
	}
	codeIdx := parentIdx
	switch {
	case parentIdx >= 0 && skuIdx >= 0:
		return nil, apperr.BadRequest(errNotParentFile)
	case codeIdx == -1:
		codeIdx = skuIdx
	}
	if codeIdx == -1 {
		return nil, apperr.BadRequest("Không tìm thấy cột 'SKU cha' trong file — kiểm tra lại dòng tiêu đề")
	}
	cell := func(rec []string, idx int) string {
		if idx >= 0 && idx < len(rec) {
			return strings.TrimSpace(rec[idx])
		}
		return ""
	}
	rows := make([]ParentSKURow, 0, len(records)-1)
	for di, rec := range records[1:] {
		rows = append(rows, ParentSKURow{
			RowNumber:   di + 1,
			SKU:         cell(rec, codeIdx),
			ProductName: cell(rec, prodIdx),
			Description: cell(rec, descIdx),
		})
	}
	return rows, nil
}

// analyzeParentImport folds repeated codes, then decides per parent: create,
// rename/re-describe, or leave. A code that is a CHILD today can't become a
// parent (two levels only) and is reported. Blank cells never clear a value.
// Takes the repo so commit can run it on its transaction.
func analyzeParentImport(repo *repositories.Repositories, rows []ParentSKURow) (ParentSKUImportPreview, error) {
	type agg struct {
		code, name, product, desc string
		rows                      []int
		conflict                  bool
	}
	byCode := map[string]*agg{}
	var order []string
	var errs []LegacyRowError
	total, duplicates := 0, 0
	for _, r := range rows {
		raw := strings.TrimSpace(r.SKU)
		product := strings.TrimSpace(r.ProductName)
		desc := strings.TrimSpace(r.Description)
		if raw == "" && product == "" && desc == "" {
			continue // blank line
		}
		total++
		if raw == "" {
			errs = append(errs, LegacyRowError{
				RowNumber: r.RowNumber, ErrorCode: errSKUMissing, Message: "Dòng có tên/mô tả nhưng thiếu mã SKU cha",
			})
			continue
		}
		code := normalizeSKUCode(raw)
		a := byCode[code]
		if a == nil {
			byCode[code] = &agg{code: code, name: raw, product: product, desc: desc, rows: []int{r.RowNumber}}
			order = append(order, code)
			continue
		}
		duplicates++
		a.rows = append(a.rows, r.RowNumber)
		// A repeat may fill a blank the first line left, never contradict it.
		merge := func(have *string, got string) {
			switch {
			case got == "":
			case *have == "":
				*have = got
			case *have != got:
				a.conflict = true
			}
		}
		merge(&a.product, product)
		merge(&a.desc, desc)
	}

	existing, err := repo.SKU.ListByCodes(order)
	if err != nil {
		return ParentSKUImportPreview{}, err
	}
	byCodeRec := make(map[string]*models.SKU, len(existing))
	ids := make([]uint, 0, len(existing))
	var parentIDs []uint // parents of codes that are children today
	for i := range existing {
		s := &existing[i]
		byCodeRec[s.Code] = s
		ids = append(ids, s.ID)
		if s.ParentID != nil {
			parentIDs = append(parentIDs, *s.ParentID)
		}
	}
	children := map[uint][]uint{}
	if len(ids) > 0 {
		if children, err = repo.SKU.ChildrenOf(ids); err != nil {
			return ParentSKUImportPreview{}, err
		}
	}
	parentCode := map[uint]string{}
	if len(parentIDs) > 0 {
		ps, err := repo.SKU.ListByIDs(parentIDs)
		if err != nil {
			return ParentSKUImportPreview{}, err
		}
		for _, p := range ps {
			parentCode[p.ID] = p.Code
		}
	}

	pv := ParentSKUImportPreview{}
	sum := ParentSKUImportSummary{TotalRows: total, DuplicateRows: duplicates}
	for _, code := range order {
		a := byCode[code]
		rec := byCodeRec[code]
		if a.conflict {
			errs = append(errs, LegacyRowError{
				RowNumber: a.rows[0], SKU: a.name, ErrorCode: errParentRowConflict,
				Message: fmt.Sprintf("Mã này lặp ở các dòng %s với tên/mô tả khác nhau — sửa cho thống nhất", joinInts(a.rows)),
			})
			continue
		}
		if rec != nil && rec.ParentID != nil {
			of := parentCode[*rec.ParentID]
			if of == "" {
				of = "SKU khác"
			}
			errs = append(errs, LegacyRowError{
				RowNumber: a.rows[0], SKU: a.name, ErrorCode: errParentIsChild,
				Message: "SKU này đang là SKU con của " + of + " — chỉ hỗ trợ 2 tầng, không làm SKU cha được",
			})
			continue
		}
		it := ParentSKUImportItem{
			Code: code, Name: a.name, ProductName: a.product, Description: a.desc,
			Exists: rec != nil, RowNumbers: a.rows,
		}
		switch {
		case rec == nil:
			it.Action = importActionCreate
			sum.New++
		default:
			it.skuID = rec.ID
			it.CurrentProductName = rec.ProductName
			it.CurrentDescription = rec.Description
			it.ChildCount = len(children[rec.ID])
			if (a.product != "" && a.product != rec.ProductName) || (a.desc != "" && a.desc != rec.Description) {
				it.Action = importActionUpdate
				sum.Updates++
			} else {
				it.Action = importActionNoChange
				sum.Unchanged++
			}
		}
		pv.Items = append(pv.Items, it)
	}
	pv.Errors = errs
	sum.ErrorRows = len(errs)
	pv.Summary = sum
	return pv, nil
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ", ")
}

// PreviewParents analyses a step-1 file (nothing is written).
func (s *MasterImportService) PreviewParents(filename string, rows []ParentSKURow) (*ParentSKUImportPreview, error) {
	pv, err := analyzeParentImport(s.repo, rows)
	if err != nil {
		return nil, apperr.Internal("could not read catalog").Wrap(err)
	}
	pv.Filename = filename
	return &pv, nil
}

// CommitParents creates the new parents and refreshes the names/descriptions
// the file changes — batched inserts, one UPDATE for all the refreshes.
func (s *MasterImportService) CommitParents(actor Actor, rows []ParentSKURow) (*ParentSKUImportPreview, error) {
	var pv ParentSKUImportPreview
	applied := ParentSKUImportApplied{}
	err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		var err error
		if pv, err = analyzeParentImport(txRepo, rows); err != nil {
			return err
		}
		var create []models.SKU
		var patches []repositories.SKUPatch
		for _, it := range pv.Items {
			switch it.Action {
			case importActionCreate:
				product := it.ProductName
				if product == "" {
					product = it.Name
				}
				create = append(create, models.SKU{
					Code: it.Code, Name: it.Name, ProductName: product, Description: it.Description, IsActive: true,
				})
			case importActionUpdate:
				p := repositories.SKUPatch{ID: it.skuID}
				if it.ProductName != "" && it.ProductName != it.CurrentProductName {
					v := it.ProductName
					p.ProductName = &v
				}
				if it.Description != "" && it.Description != it.CurrentDescription {
					v := it.Description
					p.Description = &v
				}
				patches = append(patches, p)
			}
		}
		if err := txRepo.SKU.CreateMany(create, skuInsertBatch); err != nil {
			return err
		}
		if err := txRepo.SKU.PatchMany(patches); err != nil {
			return err
		}
		applied.Created, applied.Updated = len(create), len(patches)
		return nil
	})
	if err != nil {
		return nil, apperr.Internal("could not commit parent SKU import").Wrap(err)
	}
	pv.Applied = &applied
	s.audit.Log(actor, "SKU_PARENT_IMPORT_COMMIT", "sku", nil,
		fmt.Sprintf("Parent SKU import: %d created, %d updated", applied.Created, applied.Updated), nil)
	return &pv, nil
}

// ---------- Sample template ----------

var parentTemplateHeaders = []string{"SKU cha", "Tên sản phẩm", "Mô tả"}

var parentTemplateSample = [][]string{
	{"HOP-NHUA", "Hộp nhựa", "Hộp mica nhiều kích thước"},
	{"ORNAMENT-MICA", "Acrylic Ornament", ""}, // mô tả để trống cũng được
}

// ParentTemplateXLSX renders the step-1 sample as a real .xlsx.
func (s *MasterImportService) ParentTemplateXLSX() ([]byte, string, error) {
	grid := append([][]string{parentTemplateHeaders}, parentTemplateSample...)
	data, err := buildTemplateXLSX("SKU cha", grid, []float64{20, 28, 36})
	if err != nil {
		return nil, "", err
	}
	return data, "sku-cha-template.xlsx", nil
}
