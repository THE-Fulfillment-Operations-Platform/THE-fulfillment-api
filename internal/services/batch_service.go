package services

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/apptypes"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// BatchService creates production batches (grouped by material) and drives the
// internal production status machine.
type BatchService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// CreateBatchInput creates one batch for a single material from selected items.
// A combo item that needs several materials is handled by creating one batch per
// material (call this endpoint once per material).
type CreateBatchInput struct {
	MaterialID   uint           `json:"material_id" binding:"required"`
	OrderItemIDs []uint         `json:"order_item_ids" binding:"required,min=1"`
	Priority     string         `json:"priority"`
	DueDate      *apptypes.Date `json:"due_date"`
	Note         string         `json:"note"`
}

// Create builds a batch and its batch items. Items whose SKU does not include the
// material, or that are already scheduled for that material, are skipped and
// reported back so the caller knows exactly what was batched.
func (s *BatchService) Create(actor Actor, in CreateBatchInput) (*models.Batch, []uint, error) {
	material, err := s.repo.Material.FindByID(in.MaterialID)
	if err != nil {
		return nil, nil, apperr.BadRequest("material_id does not reference an existing material")
	}

	existing, err := s.repo.Batch.ExistingActiveItemMaterial(in.OrderItemIDs)
	if err != nil {
		return nil, nil, apperr.Internal("could not check existing batch items").Wrap(err)
	}

	priority := models.Priority(in.Priority)
	switch priority {
	case models.PriorityNormal, models.PriorityHigh, models.PriorityUrgent:
	default:
		priority = models.PriorityNormal
	}

	// Production quota of this material (0 = unlimited → never split).
	quota := 0
	if material.ProductsPerUnit != nil {
		quota = *material.ProductsPerUnit
	}

	var rootBatch *models.Batch // the batch returned to the caller (flat batch or parent)
	var skipped []uint

	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		// Resolve eligible items in the caller's order (so the quota split groups them
		// in that same order); everything ineligible is reported back as skipped.
		var eligible []*models.OrderItem
		for _, itemID := range in.OrderItemIDs {
			item, err := txRepo.OrderItem.FindByID(itemID)
			if err != nil {
				skipped = append(skipped, itemID)
				continue
			}
			// Only approved orders may enter production.
			if item.Order == nil || item.Order.ReviewStatus != models.ReviewApproved || itemCancelled(item.CancellationStatus) {
				skipped = append(skipped, itemID)
				continue
			}
			// The item's SKU must include this material.
			if !skuHasMaterial(item.SKU, material.ID) {
				skipped = append(skipped, itemID)
				continue
			}
			// Skip if already scheduled for this material.
			if existing[itemID] != nil && existing[itemID][material.ID] {
				skipped = append(skipped, itemID)
				continue
			}
			eligible = append(eligible, item)
		}
		if len(eligible) == 0 {
			return apperr.Unprocessable("No eligible items for this material (items must be design-ready, include the material, and not already batched)")
		}

		groups := planBatchSplit(eligible, quota)

		// newBatch builds a batch carrying the shared attributes; createWithItems
		// persists a batch, stamps its code, attaches items and records history.
		newBatch := func() *models.Batch {
			return &models.Batch{
				MaterialID: material.ID, Status: models.StatusPending, Priority: priority,
				DueDate: in.DueDate.TimePtr(), Note: in.Note, CreatedByID: actor.IDPtr(),
			}
		}
		attachItems := func(batch *models.Batch, items []*models.OrderItem) error {
			batchItems := make([]models.BatchItem, 0, len(items))
			for _, item := range items {
				batchItems = append(batchItems, models.BatchItem{
					BatchID: batch.ID, OrderItemID: item.ID, MaterialID: material.ID, Status: models.StatusPending,
				})
			}
			if err := txRepo.Batch.CreateItems(batchItems); err != nil {
				return err
			}
			for _, bi := range batchItems {
				_ = recordStatus(txRepo, models.EntityBatchItem, bi.ID, "", string(models.StatusPending), actor, "added to batch "+batch.Code)
			}
			return nil
		}

		// Within quota (or unlimited): one flat batch — identical to legacy behaviour.
		if len(groups) <= 1 {
			batch := newBatch()
			if err := txRepo.Batch.Create(batch); err != nil {
				return err
			}
			batch.Code = fmt.Sprintf("#%d", 101000+batch.ID)
			if err := txRepo.Batch.Update(batch); err != nil {
				return err
			}
			if err := attachItems(batch, eligible); err != nil {
				return err
			}
			_ = recordStatus(txRepo, models.EntityBatch, batch.ID, "", string(models.StatusPending), actor, "batch created")
			rootBatch = batch
			return nil
		}

		// Over quota: a parent batch (holds no items) + one child batch per group,
		// each capped at the material's quota. Codes: parent "#<n>", child "#<n>-<seq>".
		parent := newBatch()
		parent.IsParent = true
		parent.ChildCount = len(groups)
		if err := txRepo.Batch.Create(parent); err != nil {
			return err
		}
		parent.Code = fmt.Sprintf("#%d", 101000+parent.ID)
		if err := txRepo.Batch.Update(parent); err != nil {
			return err
		}
		parentID := parent.ID
		for i, group := range groups {
			child := newBatch()
			child.ParentBatchID = &parentID
			child.Sequence = i + 1
			if err := txRepo.Batch.Create(child); err != nil {
				return err
			}
			child.Code = fmt.Sprintf("%s-%d", parent.Code, i+1)
			if err := txRepo.Batch.Update(child); err != nil {
				return err
			}
			if err := attachItems(child, group); err != nil {
				return err
			}
			_ = recordStatus(txRepo, models.EntityBatch, child.ID, "", string(models.StatusPending), actor, "child batch created under "+parent.Code)
		}
		_ = recordStatus(txRepo, models.EntityBatch, parent.ID, "", string(models.StatusPending), actor, "parent batch created")
		rootBatch = parent
		return nil
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, nil, ae
		}
		return nil, nil, apperr.Internal("could not create batch").Wrap(err)
	}

	// Recompute affected items' internal status outside the create transaction.
	for _, itemID := range in.OrderItemIDs {
		_, _ = recomputeOrderItemStatus(s.repo, itemID, actor)
	}

	full, _ := s.repo.Batch.FindByID(rootBatch.ID)
	s.audit.Log(actor, "BATCH_CREATE", "batch", &rootBatch.ID,
		fmt.Sprintf("Created batch %s (material=%s)", rootBatch.Code, material.Code),
		models.JSONMap{"skipped": skipped, "children": rootBatch.ChildCount})
	return full, skipped, nil
}

// planBatchSplit partitions items into groups, each holding at most `quota`
// products (sum of Quantity), never splitting a single item across groups (an
// item whose own quantity exceeds the quota gets its own over-quota group).
// quota ≤ 0 means unlimited → a single group. Mirrors the web app's
// utils/batch.ts planBatchSplit so the split preview and the created batches
// always agree.
func planBatchSplit(items []*models.OrderItem, quota int) [][]*models.OrderItem {
	if len(items) == 0 {
		return nil
	}
	if quota <= 0 {
		return [][]*models.OrderItem{items}
	}
	var groups [][]*models.OrderItem
	var current []*models.OrderItem
	count := 0
	for _, it := range items {
		q := it.Quantity
		if q < 1 {
			q = 1
		}
		// Start a new group when the current one is non-empty and adding this item
		// would exceed the quota.
		if len(current) > 0 && count+q > quota {
			groups = append(groups, current)
			current = nil
			count = 0
		}
		current = append(current, it)
		count += q
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

func (s *BatchService) Get(id uint) (*models.Batch, error) {
	b, err := s.repo.Batch.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Batch not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return b, nil
}

func (s *BatchService) List(f repositories.BatchFilter) ([]models.Batch, int64, error) {
	f.Page = f.Page.Normalize()
	return s.repo.Batch.List(f)
}

// UpdateStatusInput sets a new production status on a batch.
type UpdateStatusInput struct {
	Status string `json:"status" binding:"required"`
	Note   string `json:"note"`
}

// UpdateStatus moves a batch (and all its batch items) to a new internal status
// and recomputes the affected items. Used by the production board / batch detail
// for Pending → Đã in → Đã cắt → Đã QC.
func (s *BatchService) UpdateStatus(actor Actor, batchID uint, in UpdateStatusInput) (*models.Batch, error) {
	newStatus := models.InternalStatus(in.Status)
	if !newStatus.Valid() {
		return nil, apperr.BadRequest("Invalid status (PENDING|PRINTED|CUT|QC_PASSED)")
	}
	// The production board only advances fabrication stages (PENDING/PRINTED/CUT).
	// QC_PASSED is a product-level gate set exactly once at the QC station when the
	// whole finished product (every material part) is done — not per-material batch.
	// A batch still reaches QC_PASSED, but only by rolling up from its QC-passed items.
	if newStatus == models.StatusQCPassed {
		return nil, apperr.Unprocessable("Không đặt 'Đã QC' ở bảng sản xuất. QC được thực hiện 1 lần ở trạm QC khi cả sản phẩm (mọi NVL) đã sản xuất xong.")
	}
	batch, err := s.Get(batchID)
	if err != nil {
		return nil, err
	}
	// A QC-passed batch is finished; the board never edits it (undoing QC is a
	// deliberate QC-station action, not a status click). Mirrors the FE lock.
	if batch.Status == models.StatusQCPassed {
		return nil, apperr.Unprocessable("Batch đã QC — không cập nhật trạng thái ở bảng sản xuất nữa.")
	}
	// Production moves forward: a batch never regresses to an earlier stage, which
	// protects the QC gate (regressing a CUT batch would un-finish its items). The
	// one exception is OWNER, who may step a batch back to fix a mistaken advance.
	// Rework is remake + a note, not a status rollback.
	if newStatus.Rank() < batch.Status.Rank() && actor.Role != models.RoleOwner {
		return nil, apperr.Unprocessable("Sản xuất chỉ tiến, không lùi: batch đang ở '" + string(batch.Status) + "', không thể hạ về '" + string(newStatus) + "'. (Chỉ OWNER được sửa khi bấm nhầm.)")
	}

	affectedItems := map[uint]bool{}
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		items, err := txRepo.Batch.BatchItemsForBatch(batch.ID)
		if err != nil {
			return err
		}
		for i := range items {
			bi := &items[i]
			item, loadErr := txRepo.OrderItem.FindByID(bi.OrderItemID)
			if loadErr != nil || itemCancelled(item.CancellationStatus) {
				continue
			}
			// Never let a board change touch an already QC-passed part. In a mixed
			// batch (one order QC-passed while another still in production) this stops
			// a batch move — especially an OWNER regression — from silently un-QC'ing it.
			if bi.Status == models.StatusQCPassed {
				continue
			}
			if bi.Status == newStatus {
				continue
			}
			old := string(bi.Status)
			bi.Status = newStatus
			if err := txRepo.Batch.UpdateBatchItem(bi); err != nil {
				return err
			}
			_ = recordStatus(txRepo, models.EntityBatchItem, bi.ID, old, string(newStatus), actor, in.Note)
			affectedItems[bi.OrderItemID] = true
		}
		oldBatch := string(batch.Status)
		batch.Status = newStatus
		if err := txRepo.Batch.Update(batch); err != nil {
			return err
		}
		_ = recordStatus(txRepo, models.EntityBatch, batch.ID, oldBatch, string(newStatus), actor, in.Note)
		return nil
	})
	if err != nil {
		return nil, apperr.Internal("could not update batch status").Wrap(err)
	}

	for itemID := range affectedItems {
		_, _ = recomputeOrderItemStatus(s.repo, itemID, actor)
	}

	// If this is a child batch, roll the change up into its parent's status.
	if batch.ParentBatchID != nil {
		_ = recomputeParentBatchStatus(s.repo, *batch.ParentBatchID, actor)
	}

	s.audit.Log(actor, "BATCH_STATUS_UPDATE", "batch", &batch.ID,
		fmt.Sprintf("Batch %s -> %s", batch.Code, newStatus), nil)
	return s.Get(batch.ID)
}

func skuHasMaterial(sku *models.SKU, materialID uint) bool {
	if sku == nil {
		return false
	}
	for _, m := range sku.Materials {
		if m.MaterialID == materialID {
			return true
		}
	}
	return false
}

// ---------- Legacy production-template export ----------

// productionTemplateHeaders is the exact, ordered legacy production-template
// header row. The workshop's existing spreadsheet relies on this precise column
// order and spelling — note "Mã nội bộ" intentionally appears twice (positions 1
// and 13) for legacy compatibility. Do not reorder or rename.
var productionTemplateHeaders = []string{
	"Mã nội bộ",
	"SỐ Batch",
	"Ngày",
	"Order ID",
	"SKU",
	"Loại VL",
	"Mô tả Sp để QC (Hiện lên phần QC)",
	"Mã ảnh (copy bên TĐN Ctr + Ship + V...)",
	"Số thứ tự",
	"Số lượng",
	"Link ảnh",
	"Mock up",
	"Mã nội bộ",
	"Tên khách",
	"Tên File",
	"Link in",
	"Link cắt",
}

// seqStr renders a production sequence, leaving an unassigned (0) sequence blank
// so the legacy sheet does not show a spurious ordering position.
func seqStr(n int) string {
	if n == 0 {
		return ""
	}
	return itoa(n)
}

// ProductionTemplateGrid builds the legacy production-template grid — the header
// row followed by one row per batch item — for a fully-loaded batch (with
// Items.OrderItem.Order and Items.Material preloaded). It is a pure function so
// the column order and per-field mapping can be unit-tested without a database.
func ProductionTemplateGrid(batch *models.Batch) [][]string {
	date := batch.CreatedAt.Format("2006-01-02")
	grid := make([][]string, 0, len(batch.Items)+1)
	grid = append(grid, productionTemplateHeaders)

	for _, bi := range batch.Items {
		it := bi.OrderItem
		if it == nil || itemCancelled(it.CancellationStatus) {
			continue
		}
		// Loại VL comes from the batch's material (batches are material-scoped).
		materialName := batch.Material.Name
		if bi.Material != nil {
			if bi.Material.Name != "" {
				materialName = bi.Material.Name
			} else {
				materialName = bi.Material.Code
			}
		}
		var orderID, customer string
		if it.Order != nil {
			orderID = it.Order.StoreOrderID
			customer = it.Order.ShippingName
		}
		grid = append(grid, []string{
			it.InternalCode,               // Mã nội bộ
			batch.Code,                    // SỐ Batch
			date,                          // Ngày
			orderID,                       // Order ID
			it.SKUCode,                    // SKU
			materialName,                  // Loại VL
			it.QCDescription,              // Mô tả Sp để QC
			it.ImageCode,                  // Mã ảnh
			seqStr(it.ProductionSequence), // Số thứ tự (blank when unassigned)
			itoa(it.Quantity),             // Số lượng
			it.DesignURL,                  // Link ảnh
			it.MockupURL,                  // Mock up
			it.InternalCode,               // Mã nội bộ (legacy 2nd copy)
			customer,                      // Tên khách
			it.ProductionFileName,         // Tên File
			it.PrintFileURL,               // Link in
			it.CutFileURL,                 // Link cắt
		})
	}
	return grid
}

// productionColumnWidths sets sensible Excel column widths (in characters) for the
// 17 production-template columns, keeping URL columns wide and codes/counts compact
// so the sheet is readable without manual resizing.
var productionColumnWidths = []float64{
	16, // A  Mã nội bộ
	10, // B  SỐ Batch
	12, // C  Ngày
	22, // D  Order ID
	16, // E  SKU
	18, // F  Loại VL
	32, // G  Mô tả QC
	22, // H  Mã ảnh
	9,  // I  Số thứ tự
	9,  // J  Số lượng
	42, // K  Link ảnh
	42, // L  Mock up
	16, // M  Mã nội bộ
	20, // N  Tên khách
	22, // O  Tên File
	42, // P  Link in
	42, // Q  Link cắt
}

// ProductionTemplateXLSX loads a batch and renders the legacy production template as
// a real .xlsx workbook. Unlike CSV, an xlsx always splits into columns cleanly in
// Excel regardless of the machine's locale/list separator, and Vietnamese headers
// need no BOM. The header row is bold on a light fill, frozen, and auto-filtered,
// with per-column widths so it is readable on open.
func (s *BatchService) ProductionTemplateXLSX(batchID uint) ([]byte, string, error) {
	batch, err := s.Get(batchID)
	if err != nil {
		return nil, "", err
	}
	grid := ProductionTemplateGrid(batch)

	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	const sheet = "Sản xuất"
	if err := f.SetSheetName("Sheet1", sheet); err != nil {
		return nil, "", apperr.Internal("could not build production workbook").Wrap(err)
	}

	for r, row := range grid {
		cell, _ := excelize.CoordinatesToCellName(1, r+1)
		cells := make([]interface{}, len(row))
		for i, v := range row {
			cells[i] = v
		}
		if err := f.SetSheetRow(sheet, cell, &cells); err != nil {
			return nil, "", apperr.Internal("could not write production rows").Wrap(err)
		}
	}

	lastCol, _ := excelize.ColumnNumberToName(len(productionTemplateHeaders))
	for i, w := range productionColumnWidths {
		name, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, name, name, w)
	}

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
		return nil, "", apperr.Internal("could not render production workbook").Wrap(err)
	}
	filename := "production-" + strings.ReplaceAll(batch.Code, "#", "") + ".xlsx"
	return buf.Bytes(), filename, nil
}

// StreamBatchAssetsZip downloads each batch item's design/mockup/print/cut asset URLs
// and streams them into a ZIP archive for the client.
func (s *BatchService) StreamBatchAssetsZip(ctx context.Context, w io.Writer, batchID uint) error {
	batch, err := s.Get(batchID)
	if err != nil {
		return err
	}

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	// SSRF-safe client: refuses to connect to non-public IPs (see safeurl.go).
	// Asset URLs come from seller import data, so a plain client would be an SSRF sink.
	client := newSafeAssetClient(30 * time.Second)
	manifest := make([]string, 0)
	usedNames := map[string]int{}

	for _, bi := range batch.Items {
		it := bi.OrderItem
		if it == nil || itemCancelled(it.CancellationStatus) {
			continue
		}

		assets := []struct {
			url   string
			type_ string
		}{
			{it.DesignURL, "design"},
			{it.MockupURL, "mockup"},
			{it.PrintFileURL, "print"},
			{it.CutFileURL, "cut"},
		}
		code := sanitizeZipComponent(it.InternalCode)
		if code == "" {
			code = sanitizeZipComponent(batch.Code)
		}

		for _, asset := range assets {
			if strings.TrimSpace(asset.url) == "" {
				continue
			}
			entryName := zipEntryName(code, asset.type_, asset.url, usedNames)
			if err := writeURLToZipEntry(ctx, client, zw, asset.url, entryName); err != nil {
				return err
			}
			manifest = append(manifest, fmt.Sprintf("%s,%s,%s", code, asset.type_, asset.url))
		}
	}

	if len(manifest) == 0 {
		return apperr.Unprocessable("No assets available for batch ZIP download")
	}

	m, err := zw.Create("manifest.txt")
	if err != nil {
		return apperr.Internal("could not write ZIP manifest").Wrap(err)
	}
	if _, err := io.WriteString(m, strings.Join(manifest, "\n")); err != nil {
		return apperr.Internal("could not write ZIP manifest").Wrap(err)
	}

	return zw.Close()
}

func writeURLToZipEntry(ctx context.Context, client *http.Client, zw *zip.Writer, rawURL, entryName string) error {
	// Reject non-http(s) schemes and private/loopback hosts before dialing. The
	// client's dial-time guard is the authoritative SSRF check (also covers
	// redirects + rebinding); this gives an early, clear rejection.
	u, err := validatePublicHTTPURL(rawURL)
	if err != nil {
		return apperr.Unprocessable("Asset URL not allowed: " + err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return apperr.Internal("could not download asset").Wrap(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return apperr.Internal("could not download asset").Wrap(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apperr.Internal(fmt.Sprintf("asset request failed: %s -> %d", rawURL, resp.StatusCode))
	}

	entry, err := zw.Create(entryName)
	if err != nil {
		return apperr.Internal("could not write ZIP entry").Wrap(err)
	}

	// Cap per-asset size so a hostile/huge remote file can't exhaust resources.
	if _, err = io.Copy(entry, io.LimitReader(resp.Body, maxAssetBytes)); err != nil {
		return apperr.Internal("could not stream asset into ZIP").Wrap(err)
	}
	return nil
}

func zipEntryName(code, assetType, rawURL string, usedNames map[string]int) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		u = &url.URL{Path: rawURL}
	}
	ext := path.Ext(u.Path)
	if ext == "" {
		ext = ".bin"
	}
	name := fmt.Sprintf("%s-%s%s", code, assetType, ext)
	if count, ok := usedNames[name]; ok {
		count++
		usedNames[name] = count
		name = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), count, ext)
	} else {
		usedNames[name] = 1
	}
	return name
}

func sanitizeZipComponent(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "#", "")
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, " ", "_")
	return value
}
