package repositories

import (
	"strings"

	"the-fulfillment/backend/internal/models"
)

// Chặng của batch (Bảng sản xuất, Nghĩa 27/11: "đọc lại cả dự án xem có những
// trạng thái nào thì cho hết vào để lọc").
//
// batches.status chỉ có 4 giá trị (PENDING / PRINTED / CUT / QC_PASSED), nhưng
// nhãn người dùng thấy ở phần còn lại của hệ thống thì nhiều hơn: "Đã có file SX"
// (PENDING đủ link in + cắt), "Đã bàn giao" (QC xong, mọi đơn đã rời xưởng), batch
// "Đã đóng" (huỷ batch / huỷ hết ở QC), và sau QC thì hàng đi theo đơn: Đã đóng gói
// → Đã bàn giao → Đã gửi đi → Đã giao.
//
// Chặng là MỘT giá trị suy ra cho mỗi batch, LOẠI TRỪ NHAU (mỗi batch đúng một
// chặng), theo thứ tự ưu tiên:
//
//  1. CLOSED        "Đã đóng"          closed_at có giá trị (huỷ batch, hoặc mọi phần huỷ ở QC).
//     (trạng thái lưu lạ — xem dưới — đứng ngay sau CLOSED)
//  2. NO_ITEMS      "Đơn đã huỷ hết"   chưa đóng nhưng không còn phần nào sống (mọi dòng đơn đã huỷ).
//  3. FILES_MISSING "Chờ xử lý"        PENDING, chưa đủ link in + cắt.
//     FILES_READY   "Đã có file SX"    PENDING, đủ link in + cắt.
//  4. PRINTED       "Đã in"
//     CUT           "Đã cắt"
//  5. QC_PASSED rồi tách theo đơn CHẬM NHẤT trong các phần còn sống (như item cuộn
//     lên đơn: một đơn còn ở xưởng thì cả batch còn ở khâu đó):
//     QC_PASSED     "Đã QC"            còn đơn chưa đóng gói (PRODUCTION).
//     PACKED        "Đã đóng gói"      đơn chậm nhất đã đóng gói, chưa bàn giao.
//     HANDED_OFF    "Đã bàn giao"      đơn chậm nhất đã bàn giao THE.
//     SHIPPED       "Đã gửi đi"
//     DELIVERED     "Đã giao"          mọi đơn còn sống đã giao xong.
//
// Trạng thái lưu lạ (không phải 4 giá trị trên) trả nguyên giá trị đó làm chặng,
// trước cả NO_ITEMS, để bảng đếm/lọc vẫn thấy nó thay vì nuốt mất (prod 27/11 chưa
// có giá trị lạ nào).
//
// "Phần còn sống" = liveBatchPartsSQL (chưa huỷ ở QC, dòng đơn không bị huỷ, đơn
// không CANCELLED) — cùng quy tắc với nhãn "Đã bàn giao" (handoverDoneCond), nên
// HANDED_OFF + SHIPPED + DELIVERED = đúng tập ?status=HANDED_OVER.
const (
	BatchStageClosed       = "CLOSED"
	BatchStageNoItems      = "NO_ITEMS"
	BatchStageFilesMissing = "FILES_MISSING"
	BatchStageFilesReady   = "FILES_READY"
	BatchStagePrinted      = "PRINTED"
	BatchStageCut          = "CUT"
	BatchStageQCPassed     = "QC_PASSED"
	BatchStagePacked       = "PACKED"
	BatchStageHandedOff    = "HANDED_OFF"
	BatchStageShipped      = "SHIPPED"
	BatchStageDelivered    = "DELIVERED"
)

// AllBatchStages theo vòng đời (thứ tự cột của bảng).
var AllBatchStages = []string{
	BatchStageFilesMissing, BatchStageFilesReady, BatchStagePrinted, BatchStageCut,
	BatchStageQCPassed, BatchStagePacked, BatchStageHandedOff, BatchStageShipped, BatchStageDelivered,
	BatchStageClosed, BatchStageNoItems,
}

// IsBatchStage: s có phải một chặng đã biết không.
func IsBatchStage(s string) bool {
	for _, x := range AllBatchStages {
		if x == s {
			return true
		}
	}
	return false
}

// liveWithSellerStatus: có phần còn sống thuộc đơn ở (một trong) các seller_status.
func liveWithSellerStatus(not bool, statuses ...models.SellerStatus) string {
	quoted := make([]string, len(statuses))
	for i, s := range statuses {
		quoted[i] = "'" + string(s) + "'"
	}
	op := "IN"
	if not {
		op = "NOT IN"
	}
	return "EXISTS (" + liveBatchPartsSQL + " AND o.seller_status " + op + " (" + strings.Join(quoted, ", ") + "))"
}

// batchStageSQL là biểu thức SQL (một cột) cho chặng của dòng `batches` hiện tại.
// Chạy được trên cả Postgres (prod) lẫn SQLite (test): chỉ CASE + EXISTS.
var batchStageSQL = `CASE
	WHEN batches.closed_at IS NOT NULL THEN '` + BatchStageClosed + `'
	WHEN batches.status NOT IN ('PENDING', 'PRINTED', 'CUT', 'QC_PASSED') THEN batches.status
	WHEN NOT EXISTS (` + liveBatchPartsSQL + `) THEN '` + BatchStageNoItems + `'
	WHEN batches.status = 'PENDING' THEN CASE WHEN ` + filesReadyCond + ` THEN '` + BatchStageFilesReady + `' ELSE '` + BatchStageFilesMissing + `' END
	WHEN batches.status = 'PRINTED' THEN '` + BatchStagePrinted + `'
	WHEN batches.status = 'CUT' THEN '` + BatchStageCut + `'
	WHEN batches.status = 'QC_PASSED' THEN CASE
		WHEN ` + liveWithSellerStatus(true, models.SellerStatusPacked, models.SellerStatusHandedOff, models.SellerStatusShipped, models.SellerStatusDelivered) + ` THEN '` + BatchStageQCPassed + `'
		WHEN ` + liveWithSellerStatus(false, models.SellerStatusPacked) + ` THEN '` + BatchStagePacked + `'
		WHEN ` + liveWithSellerStatus(false, models.SellerStatusHandedOff) + ` THEN '` + BatchStageHandedOff + `'
		WHEN ` + liveWithSellerStatus(false, models.SellerStatusShipped) + ` THEN '` + BatchStageShipped + `'
		ELSE '` + BatchStageDelivered + `' END
END`

// BatchStageCount là một ô của bảng đếm: bao nhiêu batch (trạng thái lưu, chặng).
type BatchStageCount struct {
	Status string `json:"status"`
	Stage  string `json:"stage"`
	Count  int64  `json:"count"`
}

// StageCounts đếm batch theo (trạng thái lưu, chặng) dưới bộ lọc f. Bỏ qua mọi
// bộ lọc trạng thái/chặng/open của f — đếm là để biết MỖI chặng có bao nhiêu.
// Một câu SELECT … GROUP BY.
func (r *BatchRepository) StageCounts(f BatchFilter) ([]BatchStageCount, error) {
	f.Status, f.Statuses, f.Stages, f.Files, f.Handover = "", nil, nil, "", ""
	f.ExcludeClosed = false
	var rows []BatchStageCount
	err := r.baseQuery(f).
		Select("batches.status AS status, (" + batchStageSQL + ") AS stage, COUNT(*) AS count").
		Group("1, 2").Order("1, 2").
		Scan(&rows).Error
	return rows, err
}

// stagesByID trả chặng của từng batch trong ids (một câu).
func (r *BatchRepository) stagesByID(ids []uint) (map[uint]string, error) {
	out := map[uint]string{}
	if len(ids) == 0 {
		return out, nil
	}
	type row struct {
		ID    uint
		Stage string
	}
	var rows []row
	if err := r.db.Model(&models.Batch{}).
		Select("batches.id AS id, ("+batchStageSQL+") AS stage").
		Where("batches.id IN ?", ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.ID] = x.Stage
	}
	return out, nil
}

// ---------- Danh sách chọn của bộ lọc batch, lấy từ dữ liệu thật ----------

// BatchFacet là một lựa chọn của ô lọc (chất liệu / seller) kèm số batch có nó.
// Deleted: NVL / seller đã xoá mềm nhưng vẫn còn batch — vẫn phải lọc được.
type BatchFacet struct {
	ID      uint   `json:"id"`
	Code    string `json:"code"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
	Count   int64  `json:"count"`
}

// BatchValueFacet là một giá trị (ưu tiên) kèm số batch.
type BatchValueFacet struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// BatchFilterOptions là mọi giá trị thật đang có trong batch (chưa xoá).
type BatchFilterOptions struct {
	Materials  []BatchFacet      `json:"materials"`
	Sellers    []BatchFacet      `json:"sellers"`
	Priorities []BatchValueFacet `json:"priorities"`
}

// FilterOptions đọc chất liệu, seller, ưu tiên từ CHÍNH các batch (không từ danh
// mục): NVL/seller không có batch nào thì không cần lọc; NVL/seller đã xoá mềm mà
// còn batch thì vẫn có mặt. Đếm seller khớp đúng cách ?seller_id= lọc (batch có
// một dòng của đơn thuộc seller đó).
func (r *BatchRepository) FilterOptions() (*BatchFilterOptions, error) {
	out := &BatchFilterOptions{Materials: []BatchFacet{}, Sellers: []BatchFacet{}, Priorities: []BatchValueFacet{}}
	if err := r.db.Table("batches").
		Select("m.id AS id, m.code AS code, m.name AS name, (m.deleted_at IS NOT NULL) AS deleted, COUNT(*) AS count").
		Joins("JOIN materials m ON m.id = batches.material_id").
		Where("batches.deleted_at IS NULL").
		Group("m.id, m.code, m.name, m.deleted_at").
		Order("m.name, m.code, m.id").
		Scan(&out.Materials).Error; err != nil {
		return nil, err
	}
	if err := r.db.Table("batch_items").
		Select("s.id AS id, s.code AS code, s.name AS name, (s.deleted_at IS NOT NULL) AS deleted, COUNT(DISTINCT batch_items.batch_id) AS count").
		Joins("JOIN batches b ON b.id = batch_items.batch_id AND b.deleted_at IS NULL").
		Joins("JOIN order_items oi ON oi.id = batch_items.order_item_id").
		Joins("JOIN orders o ON o.id = oi.order_id").
		Joins("JOIN sellers s ON s.id = o.seller_id").
		Where("batch_items.deleted_at IS NULL").
		Group("s.id, s.code, s.name, s.deleted_at").
		Order("s.name, s.code, s.id").
		Scan(&out.Sellers).Error; err != nil {
		return nil, err
	}
	if err := r.db.Model(&models.Batch{}).
		Select("priority AS value, COUNT(*) AS count").
		Group("priority").Order("priority").
		Scan(&out.Priorities).Error; err != nil {
		return nil, err
	}
	return out, nil
}
