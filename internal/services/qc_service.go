package services

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// QCService implements quality control: scanning an item tem to pull up its
// mockup, then confirming pass (matches mockup) or fail (rework).
type QCService struct {
	repo  *repositories.Repositories
	audit *AuditService
	// thumb serves cached, shrunk copies of seller mockups. Nil (or disabled) is
	// a supported state: the scan payload then carries no thumbnail URL and the
	// station falls back to loading the mockup from its origin.
	thumb *ThumbService
}

// ScanRef identifies an item by its tem code or internal id.
type ScanRef struct {
	Code   string `json:"code"`
	ItemID *uint  `json:"item_id"`
}

func (s *QCService) resolveItem(ref ScanRef) (*models.OrderItem, error) {
	if ref.ItemID != nil {
		return s.findItemByID(*ref.ItemID)
	}
	code := strings.TrimSpace(ref.Code)
	if code == "" {
		return nil, apperr.BadRequest("Hãy quét hoặc nhập mã item")
	}
	item, err := s.repo.OrderItem.FindByCode(code)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Không tìm thấy item nào khớp mã vừa quét")
		}
		return nil, apperr.Internal("Không tra cứu được dữ liệu").Wrap(err)
	}
	if itemCancelled(item.CancellationStatus) {
		return nil, apperr.Conflict("Sản phẩm đã huỷ, không thể QC")
	}
	return item, nil
}

func (s *QCService) findItemByID(id uint) (*models.OrderItem, error) {
	item, err := s.repo.OrderItem.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Không tìm thấy item")
		}
		return nil, apperr.Internal("Không tra cứu được dữ liệu").Wrap(err)
	}
	if itemCancelled(item.CancellationStatus) {
		return nil, apperr.Conflict("Sản phẩm đã huỷ, không thể QC")
	}
	return item, nil
}

// QCScanResult is everything the QC station needs to compare product vs mockup.
type QCScanResult struct {
	ItemID       uint   `json:"item_id"`
	ItemCode     string `json:"item_code"`
	OrderCode    string `json:"order_code"`
	StoreOrderID string `json:"store_order_id"`
	SKUCode      string `json:"sku_code"`
	Quantity     int    `json:"quantity"`
	MaterialName string `json:"material_name"` // Loại VL
	// MaterialDescription is the spec text of the material(s) the item is produced
	// in (e.g. "Gỗ 5mm 3 lớp - kích thước 4 inch in UV dán vào nhau") — shown next
	// to the product name so QC can check size/material against the physical item.
	MaterialDescription string `json:"material_description"`
	QCDescription       string `json:"qc_description"` // Mô tả SP để QC
	// SKUDescription is the catalog product description for the SKU — richer, more
	// stable product spec text for QC to check against (falls back to nothing when
	// the SKU has none). SKUProductName is the catalog product name.
	SKUDescription string `json:"sku_description"`
	SKUProductName string `json:"sku_product_name"`
	ImageCode      string `json:"image_code"` // Mã ảnh
	EngraveText    string `json:"engrave_text"`
	DesignURL      string `json:"design_url"` // Link ảnh / design (front/single)
	BackDesignURL  string `json:"back_design_url"`
	MockupURL      string `json:"mockup_url"`
	// MockupThumbURL points at our own cached, screen-sized copy of MockupURL.
	// The station renders this and keeps MockupURL for the "open in a new tab"
	// link, which should still lead to the seller's full-resolution original.
	// Empty when thumbnailing is off or the item has no mockup.
	MockupThumbURL string                `json:"mockup_thumb_url"`
	PrintFileURL   string                `json:"print_file_url"`
	CutFileURL     string                `json:"cut_file_url"`
	InternalStatus models.InternalStatus `json:"internal_status"`
	Batches        []QCScanBatch         `json:"batches"`
}

// QCScanBatch is one production part of the item.
type QCScanBatch struct {
	BatchItemID  uint                  `json:"batch_item_id"`
	BatchCode    string                `json:"batch_code"`
	MaterialCode string                `json:"material_code"`
	Status       models.InternalStatus `json:"status"`
}

// Scan returns item/order/sku/batch info plus the seller mockup to compare against.
func (s *QCService) Scan(actor Actor, ref ScanRef) (*QCScanResult, error) {
	item, err := s.resolveItem(ref)
	if err != nil {
		return nil, err
	}
	res := &QCScanResult{
		ItemID: item.ID, ItemCode: item.InternalCode, SKUCode: item.SKUCode,
		Quantity:      item.Quantity,
		QCDescription: item.QCDescription, ImageCode: item.ImageCode,
		EngraveText: item.EngraveText, DesignURL: item.DesignURL, BackDesignURL: item.BackDesignURL,
		MockupURL:    item.MockupURL,
		PrintFileURL: item.PrintFileURL, CutFileURL: item.CutFileURL,
		InternalStatus: item.InternalStatus,
	}
	res.MockupThumbURL = s.thumb.SignedURL(item.ID, item.MockupURL)
	if item.Order != nil {
		res.OrderCode = item.Order.InternalCode
		res.StoreOrderID = item.Order.StoreOrderID
	}
	if item.SKU != nil {
		res.SKUDescription = item.SKU.Description
		res.SKUProductName = skuProductName(item.SKU)
	}
	// Loại VL: the material(s) this item is produced in. Prefer the batch parts
	// (the concrete production material); fall back to the SKU's mapped materials.
	res.MaterialName = itemMaterialNames(item)
	res.MaterialDescription = itemMaterialDescriptions(item)
	for _, bi := range item.BatchItems {
		b := QCScanBatch{BatchItemID: bi.ID, Status: bi.Status}
		if bi.Batch != nil {
			b.BatchCode = bi.Batch.Code
		}
		if bi.Material != nil {
			b.MaterialCode = bi.Material.Code
		}
		res.Batches = append(res.Batches, b)
	}
	s.audit.Log(actor, "QC_SCAN", "order_item", &item.ID, "Scanned item "+item.InternalCode+" for QC", nil)
	s.warmTrayThumbnails(item)
	return res, nil
}

// warmTrayThumbnails pre-builds the mockup thumbnails for the rest of the tray
// the scanned item came from. QC does not scan random items — it works through a
// batch, one piece after another — so the item on screen is a reliable predictor
// of the next dozen. Doing this at scan time means only the first piece of a
// tray ever waits for an upstream fetch.
//
// Best-effort throughout: a failed lookup costs a slower scan, nothing more.
func (s *QCService) warmTrayThumbnails(item *models.OrderItem) {
	if !s.thumb.Enabled() || item == nil {
		return
	}
	batchIDs := make([]uint, 0, len(item.BatchItems))
	for _, bi := range item.BatchItems {
		if bi.Scrapped() {
			continue
		}
		batchIDs = append(batchIDs, bi.BatchID)
	}
	if len(batchIDs) == 0 {
		return
	}
	mockups, err := s.repo.OrderItem.MockupsForBatches(batchIDs)
	if err != nil {
		return
	}
	s.thumb.Warm(mockups)
}

// itemMaterialNames returns a comma-separated list of the distinct material
// names an item is produced in — the batch parts' materials if it is batched,
// otherwise the SKU's mapped materials. Requires BatchItems.Material and/or
// SKU.Materials.Material to be preloaded.
func itemMaterialNames(item *models.OrderItem) string {
	return strings.Join(itemMaterialValues(item, func(m *models.Material) string { return m.Name }), ", ")
}

// itemMaterialDescriptions is the same over the materials' spec descriptions.
// Combos may span several materials, so entries are joined with "; " to keep
// each description readable as one unit.
func itemMaterialDescriptions(item *models.OrderItem) string {
	return strings.Join(itemMaterialValues(item, func(m *models.Material) string { return m.Description }), "; ")
}

// itemMaterialValues collects one field over the distinct materials an item is
// produced in — the batch parts' materials first (the concrete production
// material); if those yield nothing, the SKU's mapped materials.
func itemMaterialValues(item *models.OrderItem, field func(*models.Material) string) []string {
	seen := map[string]bool{}
	var vals []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		vals = append(vals, v)
	}
	for _, bi := range item.BatchItems {
		if bi.Material != nil {
			add(field(bi.Material))
		}
	}
	if len(vals) == 0 && item.SKU != nil {
		for _, sm := range item.SKU.Materials {
			add(field(&sm.Material))
		}
	}
	return vals
}

// QCDecisionInput confirms a QC outcome for an item.
type QCDecisionInput struct {
	ScanRef
	BatchItemID *uint  `json:"batch_item_id"` // optional: QC a single material part
	DefectCode  string `json:"defect_code"`
	Note        string `json:"note"`
	// ReworkRoute decides where a failed product goes back to: "PRODUCTION" (the
	// file is fine, just re-make it) or "DESIGN" (the file itself is wrong, so it
	// must be fixed before anyone prints it again). Blank = derive from the defect
	// code; the QC screen sends the derived value so the operator can override it.
	ReworkRoute string `json:"rework_route"`
}

// Rework routes.
const (
	ReworkToProduction = "PRODUCTION"
	ReworkToDesign     = "DESIGN"
)

// designDefects are the defect codes whose cause is the design file, not the
// making of it. Re-printing the same file would reproduce the same defect, so
// these send the item back to the design queue instead of straight to a batch.
var designDefects = map[string]bool{
	"ENGRAVE_WRONG": true, // khắc sai nội dung — nội dung đến từ file
	"WRONG_MOCKUP":  true, // thành phẩm không khớp mockup seller duyệt
}

// reworkRoute picks where a failed item goes back to, honouring an explicit
// choice from the QC operator and falling back to the defect code.
func reworkRoute(explicit, defectCode string) string {
	switch strings.ToUpper(strings.TrimSpace(explicit)) {
	case ReworkToDesign:
		return ReworkToDesign
	case ReworkToProduction:
		return ReworkToProduction
	}
	if designDefects[strings.ToUpper(strings.TrimSpace(defectCode))] {
		return ReworkToDesign
	}
	return ReworkToProduction
}

// liveParts trả các phần sản xuất CÒN SỐNG của item đã preload.
//
// item.BatchItems giữ cả phần đã huỷ — đó là chủ đích, màn hình lịch sử cần thấy
// tấm đã vứt đi. Nhưng mọi quyết định QC thì không: phần đã huỷ ở lần sản xuất
// trước không phải thứ đang nằm trên bàn QC, và tính nó vào sẽ đóng dấu "đã QC"
// lên một tấm đã ở thùng rác, đồng thời làm cửa "sản phẩm đã cắt xong chưa" mở
// nhầm bằng trạng thái của lần sản xuất cũ.
func liveParts(item *models.OrderItem) []models.BatchItem {
	out := make([]models.BatchItem, 0, len(item.BatchItems))
	for _, bi := range item.BatchItems {
		if bi.ScrappedAt == nil {
			out = append(out, bi)
		}
	}
	return out
}

// targetBatchItems returns the batch items a decision applies to.
func (s *QCService) targetBatchItems(item *models.OrderItem, batchItemID *uint) ([]models.BatchItem, error) {
	live := liveParts(item)
	if batchItemID != nil {
		for _, bi := range live {
			if bi.ID == *batchItemID {
				return []models.BatchItem{bi}, nil
			}
		}
		return nil, apperr.BadRequest("Mã batch item không thuộc item này (hoặc đã bị huỷ trước đó)")
	}
	if len(live) == 0 {
		if len(item.BatchItems) > 0 {
			return nil, apperr.Unprocessable("Phần sản xuất của sản phẩm này đã bị huỷ — đang chờ làm lại ở batch mới, chưa QC được.")
		}
		return nil, apperr.Unprocessable("Item chưa được đưa vào batch sản xuất nên chưa thể QC")
	}
	return live, nil
}

// stagePhraseVN describes how far a material part has got, for QC-block messages.
func stagePhraseVN(s models.InternalStatus, batched bool) string {
	if !batched {
		return "chưa vào sản xuất"
	}
	switch s {
	case models.StatusPending:
		return "chưa sản xuất"
	case models.StatusPrinted:
		return "mới in, chưa cắt"
	default:
		return string(s)
	}
}

// assertProductComplete verifies the finished product is ready for its single QC
// check: EVERY material the SKU requires must have a part that has reached CUT
// ("Đã cắt"), the last fabrication stage. A material still unbatched, PENDING or
// only PRINTED (in-progress, not cut) blocks QC — you QC the fully-finished
// product as a whole, once, never a half-produced one. Falls back to the item's
// own parts when the SKU's bill of materials is unknown (legacy items).
func assertProductComplete(item *models.OrderItem) error {
	// Highest fabrication stage reached per material — trên các phần CÒN SỐNG.
	// Một tấm đã huỷ vẫn ghi "đã cắt", nên nếu tính cả nó thì sản phẩm đang chờ
	// làm lại trông như đã hoàn thiện và lọt qua cửa này.
	live := liveParts(item)
	best := map[uint]models.InternalStatus{}
	for _, bi := range live {
		if cur, ok := best[bi.MaterialID]; !ok || bi.Status.Rank() > cur.Rank() {
			best[bi.MaterialID] = bi.Status
		}
	}
	cutRank := models.StatusCut.Rank()
	done := func(materialID uint) bool {
		s, ok := best[materialID]
		return ok && s.Rank() >= cutRank
	}

	if item.SKU != nil && len(item.SKU.Materials) > 0 {
		var pending []string
		for _, sm := range item.SKU.Materials {
			if done(sm.MaterialID) {
				continue
			}
			name := strings.TrimSpace(sm.Material.Name)
			if name == "" {
				name = fmt.Sprintf("NVL #%d", sm.MaterialID)
			}
			s, batched := best[sm.MaterialID]
			pending = append(pending, name+" ("+stagePhraseVN(s, batched)+")")
		}
		if len(pending) > 0 {
			return apperr.Unprocessable("Sản phẩm chưa cắt xong — còn NVL chưa đạt 'Đã cắt': " +
				strings.Join(pending, ", ") + ". Cắt xong toàn bộ NVL rồi mới QC.")
		}
		return nil
	}

	// Unknown BOM: gate on the item's own parts — all must have reached CUT.
	if len(live) == 0 {
		return apperr.Unprocessable("Item chưa được đưa vào batch sản xuất nên chưa thể QC")
	}
	for _, bi := range live {
		if bi.Status.Rank() < cutRank {
			return apperr.Unprocessable("Sản phẩm chưa cắt xong (còn phần " +
				stagePhraseVN(bi.Status, true) + ") — cắt xong hết rồi mới QC.")
		}
	}
	return nil
}

// Pass records a QC PASS: the produced item matches the seller's mockup. The
// targeted batch part(s) move to QC_PASSED and the item status is recomputed.
func (s *QCService) Pass(actor Actor, in QCDecisionInput) (*models.OrderItem, error) {
	item, err := s.resolveItem(in.ScanRef)
	if err != nil {
		return nil, err
	}
	if item.Order != nil && item.Order.ReviewStatus != models.ReviewApproved {
		return nil, apperr.Unprocessable("Đơn chưa được duyệt để sản xuất nên chưa thể QC")
	}
	if item.MockupURL == "" {
		return nil, apperr.Unprocessable("Item chưa có link mockup của seller để đối chiếu khi QC")
	}
	// QC is a single product-level gate: it checks the whole assembled product, so
	// it always targets every production part of the item — never a single material
	// part. This is what makes QC happen once per product, not once per NVL batch.
	targets, err := s.targetBatchItems(item, nil)
	if err != nil {
		return nil, err
	}
	// Already fully QC-passed → refuse a re-scan so we don't write a duplicate QC
	// record; the station surfaces this as "đã QC rồi".
	allPassed := len(targets) > 0
	for i := range targets {
		if targets[i].Status != models.StatusQCPassed {
			allPassed = false
			break
		}
	}
	if allPassed {
		return nil, apperr.Unprocessable("Item này đã QC PASS rồi — không cần quét lại")
	}
	// The whole product must be finished before QC: every material the SKU needs
	// must have a produced (non-pending) part. A combo whose material is still
	// unbatched or unstarted is not QC-ready — you QC the finished product, once.
	if err := assertProductComplete(item); err != nil {
		return nil, err
	}

	// Everything the decision depends on is already in memory, so validate first and
	// let the transaction be nothing but writes. A QC station is a tight loop — the
	// operator cannot scan the next item until this answers — so the whole pass is
	// spent in a fixed THREE statements no matter how many materials the product has,
	// instead of three per part.
	records := make([]models.QCRecord, 0, len(targets))
	var (
		toPass  []uint
		history []models.StatusHistory
	)
	for i := range targets {
		bi := &targets[i]
		if bi.Status == models.StatusPending {
			return nil, apperr.Unprocessable("Item này còn phần sản xuất chưa hoàn thành (chưa in/cắt) — sản xuất xong mới QC được")
		}
		bid := bi.ID
		records = append(records, models.QCRecord{
			OrderItemID: item.ID, BatchItemID: &bid, Result: models.QCPass,
			MockupURL: item.MockupURL, Note: in.Note, CheckedByID: actor.IDPtr(),
		})
		if bi.Status != models.StatusQCPassed {
			history = append(history, models.StatusHistory{
				EntityType: models.EntityBatchItem, EntityID: bi.ID,
				FromStatus: string(bi.Status), ToStatus: string(models.StatusQCPassed),
				ChangedByID: actor.IDPtr(), Note: "QC pass",
			})
			toPass = append(toPass, bi.ID)
		}
	}

	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		// Khoá dòng sản phẩm trước khi ghi: huỷ batch / QC fail / hạ QC cũng khoá
		// đúng dòng này, nên ba luồng xếp hàng thay vì cùng ghi lên một sản phẩm.
		if err := txRepo.OrderItem.LockForUpdate([]uint{item.ID}); err != nil {
			return apperr.Internal("Không khoá được sản phẩm để ghi QC").Wrap(err)
		}
		if err := txRepo.QC.CreateBulk(records); err != nil {
			return err
		}
		// A targeted status update, not a Save of the whole row: a full save writes
		// every column back (and re-upserts the Batch association), so it can undo a
		// concurrent edit to a field QC never touched.
		//
		// Lệnh này chỉ chạm phần CÒN SỐNG (scrapped_at IS NULL). Thiếu vế đó, một
		// lần huỷ batch chạy song song sẽ bị đóng dấu "đã QC" đè lên đúng tấm vừa
		// vứt đi, và roll-up sau đó dựng lại kết luận QC vừa bị gỡ. Đụng hụt dòng
		// nghĩa là có người xử lý trước — bỏ cả transaction, đừng ghi nửa vời.
		moved, err := txRepo.Batch.PassBatchItemStatuses(toPass)
		if err != nil {
			return err
		}
		if int(moved) != len(toPass) {
			return apperr.Conflict("Có phần sản xuất của sản phẩm này vừa bị huỷ ở nơi khác — quét lại để xem trạng thái mới.")
		}
		return txRepo.Status.CreateBulk(history)
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, ae
		}
		return nil, apperr.Internal("Không ghi được kết quả QC PASS").Wrap(err)
	}
	// targets là bản LỌC (chỉ phần còn sống) chứ không alias item.BatchItems, nên
	// phải cập nhật lại chính item — câu trả lời cho trạm QC dựng từ item, và trả
	// về trạng thái cũ ở đây là một sai lệch âm thầm, không phải chậm.
	passed := make(map[uint]bool, len(toPass))
	for _, id := range toPass {
		passed[id] = true
	}
	for i := range item.BatchItems {
		if passed[item.BatchItems[i].ID] {
			item.BatchItems[i].Status = models.StatusQCPassed
		}
	}

	if final, err := recomputeOrderItemStatus(s.repo, item.ID, actor); err == nil && final != "" {
		item.InternalStatus = final
	}
	// Roll the change up to each affected batch: a batch follows its items, so a
	// batch whose items are all QC_PASSED becomes QC_PASSED too. A combo item's
	// parts live in different (per-material) batches, and they roll up together —
	// one scan must not cost a round-trip set per material.
	seenBatch := map[uint]bool{}
	var batchIDs []uint
	for _, bi := range targets {
		if bi.BatchID != 0 && !seenBatch[bi.BatchID] {
			seenBatch[bi.BatchID] = true
			batchIDs = append(batchIDs, bi.BatchID)
		}
	}
	_ = recomputeBatchStatuses(s.repo, batchIDs, actor)
	// A product that failed QC, was re-made and now passes has nothing left to
	// chase: close the rework notes automatically. Left open they pile up on the
	// Ghi chú screen and stop meaning "cần xử lý".
	if n, err := s.repo.Note.ResolveOpenForEntity(models.EntityOrderItem, item.ID, actor.IDPtr(),
		"Đã làm lại và QC pass", time.Now()); err == nil && n > 0 {
		s.audit.Log(actor, "NOTE_AUTO_RESOLVE", "order_item", &item.ID,
			fmt.Sprintf("QC pass sau khi làm lại — tự đóng %d ghi chú", n), nil)
	}

	s.audit.Log(actor, "QC_PASS", "order_item", &item.ID, "QC pass for item "+item.InternalCode, nil)
	// The item is answered from memory, already updated in place. Re-reading it here
	// would re-run every association preload resolveItem just did — a dozen round
	// trips to tell the station what it is about to be told anyway.
	return item, nil
}

// QCFailResult is what a QC fail produced: the note raised, plus what the system
// did with the product so the station can show it in one line.
type QCFailResult struct {
	Note *models.Note `json:"note"`
	// ScrappedBatchItemID is the production part written off (nil when the item
	// had not been produced yet).
	ScrappedBatchItemID *uint  `json:"scrapped_batch_item_id,omitempty"`
	BatchCode           string `json:"batch_code,omitempty"`
	MaterialName        string `json:"material_name,omitempty"`
	// Route is where the product went back to: PRODUCTION (chờ gom batch làm lại)
	// or DESIGN (chờ sửa file rồi mới batch lại).
	Route string `json:"route"`
	// Attempt is which production run just failed (1 = the first).
	Attempt int `json:"attempt"`
}

// Fail records a QC FAIL and sends the product back to be re-made.
//
// A failed piece cannot simply be "marked failed" and left in its batch: the
// batch's status is the least-advanced of its parts, so the batch would never
// close, and the item counts as "already batched", so it could never be produced
// again. So the failed part is SCRAPPED — it stays in the batch that produced it
// (that is where the defective piece really came from, and the QC record points
// at it), but stops counting anywhere else. The item then re-enters the pipeline:
//
//   - PRODUCTION route: it is design-ready and no longer has a live part for that
//     material, so it reappears in the create-batch bucket and gets grouped into
//     the next batch of that material — a genuinely new batch, without spending a
//     whole sheet on a single re-made piece.
//   - DESIGN route: the file is the problem, so design_status goes back and the
//     item lands in the design queue first; batching only becomes possible again
//     once a designer sets it ready.
//
// Only the failed material's part is scrapped. A combo whose wood is fine and
// whose mica is scratched re-makes the mica alone.
func (s *QCService) Fail(actor Actor, in QCDecisionInput) (*QCFailResult, error) {
	item, err := s.resolveItem(in.ScanRef)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.DefectCode)
	if reason == "" {
		reason = "QC_FAILED_MINOR"
	}
	route := reworkRoute(in.ReworkRoute, reason)

	var (
		note          *models.Note
		batchCode     string
		matName       string
		attempt       int
		failedID      uint
		failedBatchID uint
		failedStatus  models.InternalStatus
		unQCedBatches []uint
	)
	now := time.Now()
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		// Khoá sản phẩm trước, rồi mới đọc phần sản xuất của nó. Đọc ngoài
		// transaction thì một QC pass hoặc một lần huỷ batch chen vào giữa sẽ làm
		// mọi quyết định dưới đây dựa trên ảnh chụp đã cũ.
		if err := txRepo.OrderItem.LockForUpdate([]uint{item.ID}); err != nil {
			return apperr.Internal("Không khoá được sản phẩm để ghi QC").Wrap(err)
		}
		// Đơn đã lên bàn đóng gói thì đã quá muộn để trả sản phẩm về "chờ làm lại".
		if err := assertPackingNotStarted(txRepo, []uint{item.OrderID}, "ghi QC fail"); err != nil {
			return err
		}

		// Which part failed? Live parts only — a part scrapped by an earlier fail is
		// not something QC can fail again.
		live, err := txRepo.Batch.LiveBatchItemsForOrderItem(item.ID)
		if err != nil {
			return apperr.Internal("Không đọc được phần sản xuất của item").Wrap(err)
		}
		var failed *models.BatchItem
		switch {
		case in.BatchItemID != nil:
			for i := range live {
				if live[i].ID == *in.BatchItemID {
					failed = &live[i]
					break
				}
			}
			if failed == nil {
				return apperr.BadRequest("Phần sản xuất được chọn không thuộc item này (hoặc đã bị huỷ trước đó)")
			}
		case len(live) == 1:
			failed = &live[0]
		case len(live) > 1:
			// Combo: the operator must say which material is defective, otherwise we
			// would re-make parts that are perfectly fine.
			return apperr.Unprocessable(
				"Sản phẩm gồm nhiều phần NVL (" + itemMaterialNames(item) + ") — chọn phần bị lỗi để làm lại")
		}
		// len(live) == 0 → chưa sản xuất: vẫn ghi nhận fail + note, không có gì để huỷ.

		var failedIDPtr *uint
		if failed != nil {
			failedID, failedBatchID, failedStatus = failed.ID, failed.BatchID, failed.Status
			attempt = failed.Attempt
			if failed.Batch != nil {
				batchCode = failed.Batch.Code
			}
			if failed.Material != nil {
				matName = failed.Material.Name
			}
			id := failed.ID
			failedIDPtr = &id
		}

		if err := txRepo.QC.Create(&models.QCRecord{
			OrderItemID: item.ID, BatchItemID: failedIDPtr, Result: models.QCFail,
			MockupURL: item.MockupURL, DefectCode: reason, Note: in.Note, CheckedByID: actor.IDPtr(),
		}); err != nil {
			return err
		}

		if failed != nil {
			n, err := txRepo.Batch.ScrapBatchItems([]uint{failed.ID}, reason, actor.IDPtr(), now)
			if err != nil {
				return err
			}
			if n == 0 {
				return apperr.Conflict("Phần sản xuất này vừa được huỷ ở nơi khác — quét lại để xem trạng thái mới.")
			}
			_ = recordStatus(txRepo, models.EntityBatchItem, failed.ID, string(failed.Status), "SCRAPPED", actor,
				"QC fail ("+reason+") — huỷ phần đã sản xuất, chờ làm lại")

			// Hàng combo: QC pass đẩy MỌI phần lên QC_PASSED một lượt, nên huỷ
			// riêng phần gỗ mà để phần mica nguyên "đã QC" thì roll-up của sản phẩm
			// (min của các phần còn sống) vẫn ra QC_PASSED và sản phẩm đi thẳng sang
			// đóng gói trong lúc phần gỗ đang làm lại.
			unQCedBatches, err = unQCSiblingParts(txRepo, []uint{item.ID}, []uint{failed.ID}, actor,
				"QC fail phần "+matName+" — mở lại QC cho cả sản phẩm")
			if err != nil {
				return err
			}

			// Gỡ link in/cắt của tấm cũ khỏi ĐÚNG sản phẩm này (các sản phẩm khác
			// trong tấm vẫn đang được làm từ file đó): nó sắp được làm lại ở batch
			// mới với file mới, mang theo link cũ là chỉ thợ in vào file đã hỏng.
			if err := unstampProductionFiles(txRepo, []uint{failed.BatchID}, []uint{item.ID}); err != nil {
				return err
			}
		}

		// Count the rework on the item so the bucket/list can label it, and — for a
		// design defect — push it back to the design queue. A production defect
		// leaves design_status alone: the file is fine, only the piece is not.
		// rework_count cộng bằng biểu thức SQL, không phải đọc-rồi-ghi: hai lần fail
		// gần nhau trên hai phần khác nhau đều phải được đếm.
		fields := map[string]any{"rework_count": gorm.Expr("rework_count + 1")}
		if route == ReworkToDesign {
			fields["design_status"] = models.DesignMissing
		}
		if err := tx.Model(&models.OrderItem{}).Where("id = ?", item.ID).Updates(fields).Error; err != nil {
			return err
		}

		body := strings.TrimSpace(in.Note)
		if body == "" {
			body = "Sản phẩm không đạt QC, cần làm lại."
		}
		body += "\n\n— Lần sản xuất: " + strconv.Itoa(maxInt(attempt, 1))
		if batchCode != "" {
			body += "\n— Batch đã làm hỏng: " + batchCode
		}
		if matName != "" {
			body += "\n— Phần NVL phải làm lại: " + matName
		}
		if len(unQCedBatches) > 0 {
			body += "\n— Các phần NVL khác của sản phẩm đã được hạ khỏi 'đã QC', QC lại cả sản phẩm sau khi làm lại."
		}
		if route == ReworkToDesign {
			body += "\n— Hướng xử lý: lỗi từ file design → item đã được trả về hàng chờ thiết kế, sửa file rồi set ready lại."
		} else {
			body += "\n— Hướng xử lý: làm lại sản xuất → item đã quay lại danh sách chờ gom batch của NVL này."
		}
		owner := models.RoleProduction
		if route == ReworkToDesign {
			owner = models.RoleDesigner
		}
		note = &models.Note{
			Title:               "QC Fail: " + item.InternalCode,
			Body:                body,
			ReasonCode:          reason,
			Severity:            models.SeverityHigh,
			Status:              models.NoteOpen,
			IsRequiredAttention: true,
			EntityType:          models.EntityOrderItem,
			EntityID:            &item.ID,
			OwnerRole:           owner,
			CreatedByID:         actor.IDPtr(),
		}
		if err := txRepo.Note.Create(note); err != nil {
			return err
		}
		// Batch của các phần anh em vừa bị hạ QC phải tính lại ngay trong cùng
		// transaction, không để lọt khoảnh khắc batch nói "đã QC" mà phần thì không.
		return recomputeBatchStatuses(txRepo, unQCedBatches, actor)
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, ae
		}
		return nil, apperr.Internal("Không ghi được kết quả QC FAIL").Wrap(err)
	}

	// The scrapped part no longer counts, so both roll-ups can move: the item drops
	// back to "needs producing", and the batch that made it is free to finish.
	_, _ = recomputeOrderItemStatus(s.repo, item.ID, actor)
	if failedBatchID != 0 {
		_ = recomputeBatchStatus(s.repo, failedBatchID, actor)
		// If that was the batch's last live part, the run is over: nothing will ever
		// come out of it again (the re-make happens in a new batch). Close it, or it
		// sits on the production board forever with zero items at whatever status it
		// had reached — and a product failed ten times would leave ten such ghosts.
		if closed, err := s.repo.Batch.CloseIfNothingLeft(failedBatchID,
			"Toàn bộ sản phẩm đã huỷ do QC fail", now); err == nil && closed {
			_ = recordStatus(s.repo, models.EntityBatch, failedBatchID, string(failedStatus), string(failedStatus),
				actor, "Đóng batch — toàn bộ sản phẩm đã huỷ, làm lại ở batch mới")
		}
	}

	s.audit.Log(actor, "QC_FAIL", "order_item", &item.ID,
		"QC fail for item "+item.InternalCode+" ("+reason+") → làm lại theo hướng "+route, nil)
	return &QCFailResult{
		Note: note, ScrappedBatchItemID: func() *uint {
			if failedID == 0 {
				return nil
			}
			id := failedID
			return &id
		}(),
		BatchCode: batchCode, MaterialName: matName, Route: route, Attempt: maxInt(attempt, 1),
	}, nil
}
