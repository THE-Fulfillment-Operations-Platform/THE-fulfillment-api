package services

import (
	"github.com/xuri/excelize/v2"

	"the-fulfillment/backend/internal/apperr"
)

// buildTemplateXLSX renders a header+rows grid into a styled .xlsx workbook: the
// header row is bold on a light fill, frozen, and auto-filtered, with the given
// per-column widths (in characters). Downloaded templates use this instead of a
// comma CSV because an xlsx always splits into clean columns in Excel regardless
// of the machine's locale/list separator, and Vietnamese headers ("Mã ảnh",
// "Loại VL") need no BOM — so the file never opens as garbled, single-column text.
//
// grid[0] is the header row; widths is applied left-to-right and may be shorter
// than the column count (extra columns keep Excel's default width).
func buildTemplateXLSX(sheet string, grid [][]string, widths []float64) ([]byte, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	if err := f.SetSheetName("Sheet1", sheet); err != nil {
		return nil, apperr.Internal("could not build template workbook").Wrap(err)
	}

	for r, row := range grid {
		cell, _ := excelize.CoordinatesToCellName(1, r+1)
		cells := make([]interface{}, len(row))
		for i, v := range row {
			cells[i] = v
		}
		if err := f.SetSheetRow(sheet, cell, &cells); err != nil {
			return nil, apperr.Internal("could not write template rows").Wrap(err)
		}
	}

	colCount := 0
	if len(grid) > 0 {
		colCount = len(grid[0])
	}
	for i, w := range widths {
		if i >= colCount {
			break
		}
		name, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, name, name, w)
	}

	lastCol, _ := excelize.ColumnNumberToName(colCount)
	if style, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "1F2A44"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"E9EDF5"}},
		Alignment: &excelize.Alignment{Vertical: "center"},
	}); err == nil {
		_ = f.SetCellStyle(sheet, "A1", lastCol+"1", style)
	}
	_ = f.SetRowHeight(sheet, 1, 22)
	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})
	_ = f.AutoFilter(sheet, "A1:"+lastCol+itoa(len(grid)), []excelize.AutoFilterOptions{})

	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, apperr.Internal("could not render template workbook").Wrap(err)
	}
	return buf.Bytes(), nil
}
