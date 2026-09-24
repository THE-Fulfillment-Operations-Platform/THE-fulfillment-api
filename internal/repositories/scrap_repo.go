package repositories

import (
	"time"

	"gorm.io/gorm/clause"

	"the-fulfillment/backend/internal/models"
)

// ---------- Row locks ----------
//
// Huỷ batch, QC pass/fail và quét đóng gói đều đọc trạng thái một sản phẩm rồi
// ghi dựa trên cái vừa đọc. Không khoá thì hai trạm chạy cùng lúc sẽ dẫm lên
// nhau: QC pass đặt QC_PASSED lên đúng phần vừa bị huỷ, hoặc đóng gói quét được
// một món mà bên kia vừa hạ QC. Khoá dòng order_items là điểm hẹn chung của cả
// ba luồng — mọi luồng khoá cùng một bảng, theo thứ tự id tăng dần, nên không
// bao giờ khoá chéo nhau.

// LockForUpdate khoá các dòng order_items cho tới hết transaction. Phải gọi
// TRONG transaction (r.db là tx) và gọi TRƯỚC khi đọc dữ liệu dùng để quyết
// định. Thứ tự id tăng dần là để hai transaction khoá cùng một tập không kẹt
// deadlock. SQLite (driver test) bỏ qua FOR UPDATE — nó khoá cả file khi ghi.
func (r *OrderItemRepository) LockForUpdate(ids []uint) error {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return nil
	}
	var locked []uint
	return r.db.Model(&models.OrderItem{}).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", ids).
		Order("id").
		Pluck("id", &locked).Error
}

// dedupeIDs giữ nguyên thứ tự xuất hiện và bỏ trùng — các tập id ở đây thường
// gom từ nhiều phần sản xuất của cùng một sản phẩm.
func dedupeIDs(ids []uint) []uint {
	if len(ids) < 2 {
		return ids
	}
	seen := make(map[uint]bool, len(ids))
	out := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// InternalCodesByIDs trả mã nội bộ của các đơn — để thông báo lỗi gọi tên đúng
// đơn đang chặn thay vì bắt người dùng tra id.
func (r *OrderRepository) InternalCodesByIDs(ids []uint) (map[uint]string, error) {
	out := map[uint]string{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out, nil
	}
	type row struct {
		ID           uint
		InternalCode string
	}
	var rows []row
	if err := r.db.Model(&models.Order{}).Select("id, internal_code").
		Where("id IN ?", ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.ID] = x.InternalCode
	}
	return out, nil
}

// ---------- Đóng gói ----------

// PackingStartedOrderIDs trả về những đơn đã MỞ kiện ở trạm đóng gói. Kiện chỉ
// được tạo ở lần quét đầu tiên (getOrCreateOpenPackage nằm trong đúng
// transaction của lần quét đó), nên "có kiện" nghĩa là "đơn đã bắt đầu đóng
// gói" — và vì lần quét đầu tiên tạo dòng cho MỌI món của đơn, việc đã bắt đầu
// là chuyện của cả đơn chứ không của riêng món được quét.
func (r *PackageRepository) PackingStartedOrderIDs(orderIDs []uint) ([]uint, error) {
	orderIDs = dedupeIDs(orderIDs)
	if len(orderIDs) == 0 {
		return nil, nil
	}
	var ids []uint
	err := r.db.Model(&models.Package{}).Distinct("order_id").
		Where("order_id IN ?", orderIDs).Pluck("order_id", &ids).Error
	return ids, err
}

// ---------- Huỷ (scrap) hàng loạt ----------

// LiveBatchItemsDetailed trả các phần còn sống của những batch cho trước, kèm
// sản phẩm/đơn/NVL — đúng thứ luồng huỷ batch cần để gọi tên cái nó sắp ghi bỏ.
func (r *BatchRepository) LiveBatchItemsDetailed(batchIDs []uint) ([]models.BatchItem, error) {
	batchIDs = dedupeIDs(batchIDs)
	if len(batchIDs) == 0 {
		return nil, nil
	}
	var items []models.BatchItem
	err := activeBatchItems(r.db).
		Joins("OrderItem.Order").Joins("Material").
		Where("batch_items.batch_id IN ?", batchIDs).
		Order("batch_items.id asc").
		Find(&items).Error
	return items, err
}

// AllBatchItemsDetailed trả MỌI phần của một batch, kể cả phần đã huỷ, kèm đủ
// association cho export/tem QR. Batch đã đóng không còn phần nào sống, nên
// export của nó chỉ có nội dung khi đọc được cả phần đã huỷ.
func (r *BatchRepository) AllBatchItemsDetailed(batchID uint) ([]models.BatchItem, error) {
	var items []models.BatchItem
	err := r.db.
		Joins("OrderItem").Joins("OrderItem.Order").Joins("OrderItem.Order.Seller").Joins("Material").
		Where("batch_items.batch_id = ?", batchID).
		Order("batch_items.id asc").
		Find(&items).Error
	return items, err
}

// ScrapBatchItems ghi bỏ nhiều phần trong một câu lệnh. Điều kiện
// scrapped_at IS NULL khiến lệnh này idempotent: hai người bấm huỷ cùng lúc thì
// người thứ hai đụng 0 dòng và biết là mình tới sau, thay vì ghi đè lý do huỷ.
func (r *BatchRepository) ScrapBatchItems(ids []uint, reason string, byID *uint, at time.Time) (int64, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return 0, nil
	}
	res := r.db.Model(&models.BatchItem{}).
		Where("id IN ? AND scrapped_at IS NULL", ids).
		Updates(map[string]any{"scrapped_at": at, "scrap_reason": reason, "scrapped_by_id": byID})
	return res.RowsAffected, res.Error
}

// PassBatchItemStatuses đẩy các phần lên QC_PASSED, nhưng CHỈ những phần còn
// sống. Không có vế scrapped_at IS NULL thì một QC pass chạy song song với một
// lần huỷ sẽ đóng dấu "đã QC" lên đúng tấm vừa bị vứt đi. Trả RowsAffected để
// caller phát hiện chuyện đó và bỏ cả transaction.
func (r *BatchRepository) PassBatchItemStatuses(ids []uint) (int64, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return 0, nil
	}
	res := r.db.Model(&models.BatchItem{}).
		Where("id IN ? AND scrapped_at IS NULL", ids).
		Update("status", models.StatusQCPassed)
	return res.RowsAffected, res.Error
}

// QCPassedSiblingParts trả các phần CÒN SỐNG đang ở QC_PASSED của những sản
// phẩm cho trước, trừ các phần được loại ra (thường là phần vừa bị huỷ).
//
// Đây là mấu chốt của hàng combo nhiều NVL: QC là cửa mức sản phẩm, một lần pass
// đẩy MỌI phần của sản phẩm lên QC_PASSED. Huỷ riêng phần gỗ mà để phần mica
// nguyên QC_PASSED thì roll-up lấy min của các phần còn sống = QC_PASSED, và sản
// phẩm vẫn lọt sang đóng gói trong khi phần gỗ đang làm lại.
func (r *BatchRepository) QCPassedSiblingParts(orderItemIDs, excludeIDs []uint) ([]models.BatchItem, error) {
	orderItemIDs = dedupeIDs(orderItemIDs)
	if len(orderItemIDs) == 0 {
		return nil, nil
	}
	q := r.db.Where("order_item_id IN ? AND scrapped_at IS NULL AND status = ?",
		orderItemIDs, models.StatusQCPassed)
	if excludeIDs = dedupeIDs(excludeIDs); len(excludeIDs) > 0 {
		q = q.Where("id NOT IN ?", excludeIDs)
	}
	var items []models.BatchItem
	err := q.Order("id asc").Find(&items).Error
	return items, err
}

// CloseBatches đóng một tập batch (chỉ những cái chưa đóng). Batch đã đóng là
// batch không còn gì để sản xuất — hàng làm lại luôn nằm ở batch mới.
func (r *BatchRepository) CloseBatches(ids []uint, reason string, at time.Time) (int64, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return 0, nil
	}
	res := r.db.Model(&models.Batch{}).
		Where("id IN ? AND closed_at IS NULL", ids).
		Updates(map[string]any{"closed_at": at, "close_reason": reason})
	return res.RowsAffected, res.Error
}

// FindLiteManyForUpdate là FindLiteMany có khoá dòng: các guard "batch đã đóng
// chưa / đang ở trạng thái nào" phải đọc trong transaction và giữ được kết quả
// đó tới lúc ghi, nếu không hai lần huỷ song song đều thấy "chưa đóng".
func (r *BatchRepository) FindLiteManyForUpdate(ids []uint) ([]models.Batch, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []models.Batch
	err := r.db.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", ids).Order("id").Find(&rows).Error
	return rows, err
}

// GateStateByID trả trạng thái nội bộ + trạng thái huỷ của một sản phẩm, không
// kèm association nào. Cửa quét ở trạm đóng gói phải đọc lại đúng hai thứ này
// TRONG transaction, sau khi đã khoá dòng: đọc trước transaction thì một lần
// huỷ batch hay hạ QC chen vào giữa vẫn để lọt món hàng qua cửa.
func (r *OrderItemRepository) GateStateByID(id uint) (models.InternalStatus, models.CancellationStatus, error) {
	var row struct {
		InternalStatus     models.InternalStatus
		CancellationStatus models.CancellationStatus
	}
	err := r.db.Model(&models.OrderItem{}).
		Select("internal_status, cancellation_status").
		Where("id = ?", id).Take(&row).Error
	return row.InternalStatus, row.CancellationStatus, err
}

// ClearProductionFileForItems là bản thu hẹp của ClearProductionFileForBatch:
// chỉ gỡ dấu trên những sản phẩm được nêu tên, và chỉ khi giá trị vẫn ĐANG là
// link của batch đó (sản phẩm đã được batch khác đóng dấu lại thì giữ nguyên).
//
// QC fail huỷ một sản phẩm trong tấm, không huỷ cả tấm: những sản phẩm còn lại
// vẫn đang được làm ra từ đúng file đó, nên gỡ dấu của cả batch sẽ cướp mất link
// hợp lệ của chúng. `column` là hằng do caller truyền, không bao giờ là input.
func (r *OrderItemRepository) ClearProductionFileForItems(itemIDs []uint, column, url string) (int64, error) {
	itemIDs = dedupeIDs(itemIDs)
	if len(itemIDs) == 0 || url == "" {
		return 0, nil
	}
	res := r.db.Model(&models.OrderItem{}).
		Where("id IN ?", itemIDs).
		Where(column+" = ?", url).
		Update(column, "")
	return res.RowsAffected, res.Error
}
