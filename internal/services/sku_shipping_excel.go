package services

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Bảng khai thông tin vận chuyển cho SKU: xuất mọi SKU ra một file, xưởng điền
// cân nặng / kích thước hộp / giá trị khai báo / mã HS rồi nạp lại. Mỗi dòng là
// một SKU, đối chiếu theo MÃ SKU (không theo vị trí dòng).
//
// Ô trống nghĩa là "SKU này không tự khai, lấy theo SKU cha" — nên dòng SKU cha
// là chỗ khai một lần cho cả họ, dòng SKU con chỉ điền chỗ khác cha. File xuất
// ra chỉ chứa giá trị TỰ KHAI của từng SKU (không chép giá trị kế thừa xuống
// dòng con), để nạp lại nguyên file không biến giá trị kế thừa thành giá trị
// tự khai.

// MaxSKUShippingImportRows caps one upload — well above the catalogue size.
const MaxSKUShippingImportRows = 3000

var skuShippingHeaders = []string{
	"Mã SKU", "SKU cha", "Tên SKU",
	"Cân nặng (g)", "Dài hộp (cm)", "Rộng hộp (cm)", "Cao hộp (cm)",
	"Giá trị khai báo (USD)", "Mã HS",
}

// Column keys, after shipHeaderKey normalisation.
const (
	shipColCode   = "code"
	shipColWeight = "weight"
	shipColLength = "length"
	shipColWidth  = "width"
	shipColHeight = "height"
	shipColValue  = "value"
	shipColHS     = "hs"
)

var shipHeaderAliases = map[string]string{
	"masku": shipColCode, "sku": shipColCode, "skucode": shipColCode,
	"cannangg": shipColWeight, "cannang": shipColWeight, "trongluong": shipColWeight, "trongluongg": shipColWeight,
	"weight": shipColWeight, "weightg": shipColWeight,
	"daihopcm": shipColLength, "daicm": shipColLength, "dai": shipColLength, "length": shipColLength, "lengthcm": shipColLength,
	"ronghopcm": shipColWidth, "rongcm": shipColWidth, "rong": shipColWidth, "width": shipColWidth, "widthcm": shipColWidth,
	"caohopcm": shipColHeight, "caocm": shipColHeight, "cao": shipColHeight, "height": shipColHeight, "heightcm": shipColHeight,
	"giatrikhaibaousd": shipColValue, "giatrikhaibao": shipColValue, "giatri": shipColValue, "giatriusd": shipColValue,
	"value": shipColValue, "declaredvalue": shipColValue, "valueusd": shipColValue,
	"mahs": shipColHS, "hs": shipColHS, "hscode": shipColHS,
}

func shipHeaderKey(h string) string {
	return shipHeaderAliases[removeVietnameseDiacritics(normalizeTrackingHeader(h))]
}

// ---------- export ----------

// SKUShippingExportXLSX renders the worksheet: every SKU, each parent followed
// by its children, with the values the SKU declares itself.
func (s *CatalogService) SKUShippingExportXLSX() ([]byte, string, error) {
	skus, err := s.repo.SKU.AllForShipping()
	if err != nil {
		return nil, "", apperr.Internal("could not list SKUs").Wrap(err)
	}
	byID := make(map[uint]*models.SKU, len(skus))
	children := map[uint][]*models.SKU{}
	for i := range skus {
		byID[skus[i].ID] = &skus[i]
	}
	var tops []*models.SKU
	for i := range skus {
		sk := &skus[i]
		if sk.ParentID != nil && byID[*sk.ParentID] != nil {
			children[*sk.ParentID] = append(children[*sk.ParentID], sk)
			continue
		}
		tops = append(tops, sk)
	}

	grid := [][]string{skuShippingHeaders}
	row := func(sk *models.SKU) {
		parent := ""
		if sk.ParentID != nil {
			if p := byID[*sk.ParentID]; p != nil {
				parent = p.Code
			}
		}
		grid = append(grid, []string{
			sk.Code, parent, sk.Name,
			fmtShipNumber(sk.ShipWeightG), fmtShipNumber(sk.ShipLengthCM),
			fmtShipNumber(sk.ShipWidthCM), fmtShipNumber(sk.ShipHeightCM),
			fmtShipNumber(sk.DeclaredValue), sk.HSCode,
		})
	}
	for _, top := range tops {
		row(top)
		for _, ch := range children[top.ID] {
			row(ch)
		}
	}
	data, err := buildTemplateXLSX("Van chuyen SKU", grid, []float64{24, 16, 32, 14, 14, 14, 14, 20, 16})
	if err != nil {
		return nil, "", err
	}
	return data, "sku-thong-tin-van-chuyen.xlsx", nil
}

func fmtShipNumber(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// ---------- parse ----------

// SKUShippingFileRow is one data row of an uploaded worksheet. Cells holds the
// raw text of each shipping column the FILE carries (absent columns are left
// untouched on the SKU, so a file with only "Mã SKU" + "Cân nặng (g)" sets
// weights and nothing else).
type SKUShippingFileRow struct {
	Row   int
	Code  string
	Cells map[string]string
}

// ParseSKUShippingCSV / ParseSKUShippingXLSX read an upload into rows.
func ParseSKUShippingCSV(r io.Reader) ([]SKUShippingFileRow, []string, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, nil, apperr.BadRequest("Không đọc được file CSV: " + err.Error())
	}
	return skuShippingRowsFromRecords(records)
}

func ParseSKUShippingXLSX(r io.Reader) ([]SKUShippingFileRow, []string, error) {
	f, err := excelize.OpenReader(r)
	if err != nil {
		return nil, nil, apperr.BadRequest("Không đọc được file Excel: " + err.Error())
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, nil, apperr.BadRequest("File Excel không có sheet nào")
	}
	records, err := f.GetRows(sheets[0])
	if err != nil {
		return nil, nil, apperr.BadRequest("Không đọc được dữ liệu Excel: " + err.Error())
	}
	return skuShippingRowsFromRecords(records)
}

func skuShippingRowsFromRecords(records [][]string) ([]SKUShippingFileRow, []string, error) {
	if len(records) < 2 {
		return nil, nil, apperr.BadRequest("File phải có dòng tiêu đề và ít nhất một dòng dữ liệu")
	}
	cols := map[string]int{}
	for i, h := range records[0] {
		if key := shipHeaderKey(h); key != "" {
			if _, dup := cols[key]; !dup {
				cols[key] = i
			}
		}
	}
	if _, ok := cols[shipColCode]; !ok {
		return nil, nil, apperr.BadRequest(`File thiếu cột "Mã SKU"`)
	}
	var present []string
	for _, k := range []string{shipColWeight, shipColLength, shipColWidth, shipColHeight, shipColValue, shipColHS} {
		if _, ok := cols[k]; ok {
			present = append(present, k)
		}
	}
	if len(present) == 0 {
		return nil, nil, apperr.BadRequest(`File không có cột thông tin vận chuyển nào (Cân nặng (g), Dài/Rộng/Cao hộp (cm), Giá trị khai báo (USD), Mã HS)`)
	}

	cell := func(rec []string, i int) string {
		if i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var rows []SKUShippingFileRow
	for i, rec := range records[1:] {
		code := cell(rec, cols[shipColCode])
		empty := code == ""
		cells := make(map[string]string, len(present))
		for _, k := range present {
			v := cell(rec, cols[k])
			cells[k] = v
			if v != "" {
				empty = false
			}
		}
		if empty {
			continue // blank spacer rows are not data
		}
		rows = append(rows, SKUShippingFileRow{Row: i + 2, Code: code, Cells: cells})
	}
	if len(rows) == 0 {
		return nil, nil, apperr.BadRequest("File không có dòng dữ liệu nào")
	}
	if len(rows) > MaxSKUShippingImportRows {
		return nil, nil, apperr.BadRequest(fmt.Sprintf("File có %d dòng — tối đa %d dòng mỗi lần", len(rows), MaxSKUShippingImportRows))
	}
	return rows, present, nil
}

// parseShipCell reads a number cell: "" = clear, "120", "120,5" and "120.5"
// all work (Vietnamese Excel writes a decimal comma). A message comes back for
// anything else — including "1,000" / "1.000", which is one thousand in one
// locale and one in the other (parseDimMM refuses it for the same reason): a
// weight read 1000× too small is a wrong customs declaration on a paid label.
func parseShipCell(raw string) (float64, string) {
	raw = strings.ReplaceAll(strings.TrimSpace(raw), " ", "")
	if raw == "" {
		return 0, ""
	}
	if strings.Count(raw, ",")+strings.Count(raw, ".") > 1 {
		return 0, fmt.Sprintf("%q không phải số (ghi không dấu phân cách hàng nghìn, vd 1000 hoặc 1,5)", raw)
	}
	if sep := strings.IndexAny(raw, ".,"); sep >= 0 && len(raw)-sep-1 == 3 {
		return 0, fmt.Sprintf("%q dễ hiểu nhầm (hàng nghìn hay số lẻ?) — ghi 1000 hoặc 1,5", raw)
	}
	v, err := strconv.ParseFloat(strings.Replace(raw, ",", ".", 1), 64)
	if err != nil || v < 0 {
		return 0, fmt.Sprintf("%q không phải số", raw)
	}
	return v, ""
}

// ---------- preview / commit ----------

const (
	SKUShippingRowChanged   = "CHANGED"
	SKUShippingRowUnchanged = "UNCHANGED"
	SKUShippingRowError     = "ERROR"
)

type SKUShippingPreviewRow struct {
	Row     int      `json:"row"`
	Code    string   `json:"code"`
	SKUID   uint     `json:"sku_id,omitempty"`
	Status  string   `json:"status"`
	Changes []string `json:"changes,omitempty"`
	Error   string   `json:"error,omitempty"`
	// Missing: what the factory still has to declare for this SKU AFTER the
	// import (weight / box), counting what it inherits from its parent.
	Missing []string `json:"missing,omitempty"`
}

type SKUShippingPreview struct {
	Columns    []string                `json:"columns"`
	Rows       []SKUShippingPreviewRow `json:"rows"`
	Total      int                     `json:"total"`
	Changed    int                     `json:"changed"`
	Unchanged  int                     `json:"unchanged"`
	Errors     int                     `json:"errors"`
	Incomplete int                     `json:"incomplete"`
	CanCommit  bool                    `json:"can_commit"`
	Committed  bool                    `json:"committed"`
}

// plan resolves every file row against the catalogue and computes the new
// shipping declaration of each SKU. It never writes.
func (s *CatalogService) planSKUShipping(rows []SKUShippingFileRow, present []string) (*SKUShippingPreview, []repositories.SKUShipping, error) {
	skus, err := s.repo.SKU.AllForShipping()
	if err != nil {
		return nil, nil, apperr.Internal("could not list SKUs").Wrap(err)
	}
	byCode := make(map[string]*models.SKU, len(skus))
	byID := make(map[uint]*models.SKU, len(skus))
	for i := range skus {
		byCode[skus[i].Code] = &skus[i]
		byID[skus[i].ID] = &skus[i]
	}
	has := map[string]bool{}
	for _, k := range present {
		has[k] = true
	}

	out := &SKUShippingPreview{Columns: present, Rows: make([]SKUShippingPreviewRow, 0, len(rows))}
	// next holds the post-import state of every SKU the file touches, so a child
	// row sees its parent's NEW values when "Missing" is computed.
	next := map[uint]models.SKU{}
	seen := map[string]int{}
	var writes []repositories.SKUShipping

	for _, r := range rows {
		pr := SKUShippingPreviewRow{Row: r.Row, Code: r.Code}
		fail := func(msg string) {
			pr.Status, pr.Error = SKUShippingRowError, msg
			out.Rows = append(out.Rows, pr)
			out.Errors++
		}
		if r.Code == "" {
			fail("Thiếu mã SKU")
			continue
		}
		code := models.NormalizeCode(r.Code)
		pr.Code = code
		if first, dup := seen[code]; dup {
			fail(fmt.Sprintf("SKU %s đã có ở dòng %d", code, first))
			continue
		}
		seen[code] = r.Row
		cur := byCode[code]
		if cur == nil {
			fail("Không có SKU nào mang mã " + code)
			continue
		}
		pr.SKUID = cur.ID

		upd := *cur
		in := SKUShippingInput{}
		var cellErr string
		num := func(col string) *float64 {
			if !has[col] {
				return nil
			}
			v, msg := parseShipCell(r.Cells[col])
			if msg != "" && cellErr == "" {
				cellErr = msg
			}
			return &v // 0 = clear (applySKUShipping: ≤0 → nil)
		}
		in.ShipWeightG = num(shipColWeight)
		in.ShipLengthCM = num(shipColLength)
		in.ShipWidthCM = num(shipColWidth)
		in.ShipHeightCM = num(shipColHeight)
		in.DeclaredValue = num(shipColValue)
		if has[shipColHS] {
			hs := r.Cells[shipColHS]
			in.HSCode = &hs
		}
		if cellErr != "" {
			fail(cellErr)
			continue
		}
		if err := applySKUShipping(&upd, in); err != nil {
			msg := err.Error()
			if ae, ok := apperr.As(err); ok {
				msg = ae.Message
			}
			fail(msg)
			continue
		}

		pr.Changes = shippingChanges(cur, &upd)
		if len(pr.Changes) == 0 {
			pr.Status = SKUShippingRowUnchanged
			out.Unchanged++
		} else {
			pr.Status = SKUShippingRowChanged
			out.Changed++
			writes = append(writes, repositories.SKUShipping{
				ID: cur.ID, WeightG: upd.ShipWeightG, LengthCM: upd.ShipLengthCM, WidthCM: upd.ShipWidthCM,
				HeightCM: upd.ShipHeightCM, DeclaredValue: upd.DeclaredValue, HSCode: upd.HSCode,
			})
		}
		next[cur.ID] = upd
		out.Rows = append(out.Rows, pr)
	}

	// What each SKU in the file will still lack, parent fallback included.
	state := func(id uint) *models.SKU {
		if v, ok := next[id]; ok {
			return &v
		}
		return byID[id]
	}
	for i := range out.Rows {
		pr := &out.Rows[i]
		if pr.Status == SKUShippingRowError {
			continue
		}
		sk := state(pr.SKUID)
		var parent *models.SKU
		if sk.ParentID != nil {
			parent = state(*sk.ParentID)
		}
		// The factory's part only: HS code / value may come from the THE
		// connection's defaults, set by the carrier side.
		pr.Missing = models.EffectiveShipping(sk, parent).MissingPhysical()
		if len(pr.Missing) > 0 {
			out.Incomplete++
		}
	}
	out.Total = len(out.Rows)
	out.CanCommit = out.Errors == 0 && out.Changed > 0
	return out, writes, nil
}

// shippingChanges describes, field by field, what an import changes.
func shippingChanges(old, upd *models.SKU) []string {
	var out []string
	numField := func(label, unit string, a, b *float64) {
		if fmtShipNumber(a) == fmtShipNumber(b) {
			return
		}
		show := func(v *float64) string {
			if v == nil {
				return "trống"
			}
			return fmtShipNumber(v) + unit
		}
		out = append(out, fmt.Sprintf("%s: %s → %s", label, show(a), show(b)))
	}
	numField("Cân nặng", " g", old.ShipWeightG, upd.ShipWeightG)
	numField("Dài", " cm", old.ShipLengthCM, upd.ShipLengthCM)
	numField("Rộng", " cm", old.ShipWidthCM, upd.ShipWidthCM)
	numField("Cao", " cm", old.ShipHeightCM, upd.ShipHeightCM)
	numField("Giá trị", " USD", old.DeclaredValue, upd.DeclaredValue)
	if old.HSCode != upd.HSCode {
		show := func(v string) string {
			if v == "" {
				return "trống"
			}
			return v
		}
		out = append(out, fmt.Sprintf("Mã HS: %s → %s", show(old.HSCode), show(upd.HSCode)))
	}
	return out
}

// PreviewSKUShippingImport is the dry run: nothing is written.
func (s *CatalogService) PreviewSKUShippingImport(rows []SKUShippingFileRow, present []string) (*SKUShippingPreview, error) {
	preview, _, err := s.planSKUShipping(rows, present)
	return preview, err
}

// CommitSKUShippingImport re-plans the SAME file and writes it all-or-nothing.
// Any error row refuses the whole file — half a catalogue updated is worse than
// none, because nobody can tell which half.
func (s *CatalogService) CommitSKUShippingImport(actor Actor, rows []SKUShippingFileRow, present []string) (*SKUShippingPreview, error) {
	preview, writes, err := s.planSKUShipping(rows, present)
	if err != nil {
		return nil, err
	}
	if preview.Errors > 0 {
		return nil, apperr.BadRequest(fmt.Sprintf("File còn %d dòng lỗi — sửa rồi nạp lại", preview.Errors))
	}
	if len(writes) == 0 {
		return preview, nil
	}
	if err := s.repo.SKU.SetShippingMany(writes); err != nil {
		return nil, apperr.Internal("could not save shipping data").Wrap(err)
	}
	preview.Committed = true
	preview.CanCommit = false
	s.audit.Log(actor, "SKU_SHIPPING_IMPORT", "sku", nil,
		fmt.Sprintf("Nạp thông tin vận chuyển cho %d SKU", len(writes)), nil)
	return preview, nil
}
