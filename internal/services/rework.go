package services

import (
	"sort"
	"strings"
	"unicode/utf8"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// maxReasonRunes giới hạn lý do huỷ. 60 ký tự là đúng sức chứa cột
// batch_items.scrap_reason, và cũng vừa đủ cho một câu người thật viết ("cắt
// lệch cả tấm", "máy in nhoè nửa tấm") — dài hơn thì thuộc về ghi chú, không
// phải nhãn dán trên dòng dữ liệu.
const maxReasonRunes = 60

// normalizeReason kiểm tra lý do huỷ do người dùng nhập. Bắt buộc có: một lần
// huỷ vật lý không có lý do thì tháng sau không ai tra được vì sao mất tấm đó.
func normalizeReason(raw string) (string, error) {
	reason := strings.TrimSpace(raw)
	if reason == "" {
		return "", apperr.BadRequest("Nhập lý do huỷ (tối đa " + itoa(maxReasonRunes) + " ký tự)")
	}
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		return "", apperr.BadRequest("Lý do huỷ tối đa " + itoa(maxReasonRunes) + " ký tự")
	}
	return reason, nil
}

// assertPackingNotStarted chặn mọi thao tác kéo sản phẩm ngược lại (huỷ phần đã
// sản xuất, QC fail, hạ QC) khi ĐƠN đã bắt đầu đóng gói.
//
// Ranh giới là cả ĐƠN chứ không phải từng món: lần quét đầu tiên ở trạm đóng gói
// mở kiện và tạo sẵn dòng cho mọi món của đơn, nên từ giây đó người đóng gói
// đang cầm hàng của cả đơn trên tay. Trả hàng về "chờ làm lại" sau thời điểm đó
// nghĩa là kiện đang mở chứa một món hệ thống coi như chưa làm xong — không có
// cách nào đúng để hoà giải, nên chặn ngay từ đầu.
//
// Phải gọi TRONG transaction, sau khi đã khoá các dòng order_items, nếu không nó
// chỉ là ảnh chụp cũ và trạm đóng gói vẫn có thể quét chen vào giữa.
func assertPackingNotStarted(repo *repositories.Repositories, orderIDs []uint, action string) error {
	started, err := repo.Package.PackingStartedOrderIDs(orderIDs)
	if err != nil {
		return apperr.Internal("Không kiểm tra được trạng thái đóng gói").Wrap(err)
	}
	if len(started) == 0 {
		return nil
	}
	codes, err := repo.Order.InternalCodesByIDs(started)
	if err != nil {
		return apperr.Internal("Không đọc được mã đơn").Wrap(err)
	}
	names := make([]string, 0, len(started))
	for _, id := range started {
		if code := codes[id]; code != "" {
			names = append(names, code)
		} else {
			names = append(names, "#"+itoa(int(id)))
		}
	}
	sort.Strings(names)
	if len(names) > 5 {
		names = append(names[:5], "…")
	}
	return apperr.Unprocessable("Đơn " + strings.Join(names, ", ") +
		" đã bắt đầu đóng gói — không " + action + " được nữa. Việc này xử lý theo luồng huỷ/đổi đơn.")
}

// unQCSiblingParts hạ các phần CÒN SỐNG đang ở QC_PASSED của cùng sản phẩm về
// CUT, và trả về các batch bị ảnh hưởng để caller roll-up lại.
//
// Đây là lỗ hổng của hàng combo nhiều NVL, và nó có thật với cả QC fail lẫn huỷ
// batch: QC là cửa mức SẢN PHẨM, một lần pass đẩy mọi phần (gỗ, mica, đế…) lên
// QC_PASSED cùng lúc. Nếu chỉ huỷ phần gỗ, phần mica ở batch khác vẫn QC_PASSED,
// mà roll-up của sản phẩm lấy min của các phần CÒN SỐNG — phần gỗ vừa huỷ không
// còn được tính — nên sản phẩm vẫn đứng ở QC_PASSED và đi thẳng sang đóng gói
// trong khi phần gỗ đang làm lại. Sản phẩm chưa nguyên vẹn thì cửa QC phải mở
// lại cho toàn bộ nó.
//
// Hạ về CUT (không phải PENDING): những tấm anh em vẫn là hàng đã sản xuất xong,
// nằm nguyên trong batch của chúng; chỉ có kết luận QC là hết hiệu lực. Hàng một
// NVL — phần lớn đơn — không có phần anh em nào nên hàm này không đụng gì.
//
// Phải gọi TRONG transaction (repo là txRepo).
func unQCSiblingParts(repo *repositories.Repositories, itemIDs, excludePartIDs []uint, actor Actor, note string) ([]uint, error) {
	siblings, err := repo.Batch.QCPassedSiblingParts(itemIDs, excludePartIDs)
	if err != nil {
		return nil, apperr.Internal("Không đọc được các phần đã QC của sản phẩm").Wrap(err)
	}
	if len(siblings) == 0 {
		return nil, nil
	}
	ids := make([]uint, 0, len(siblings))
	history := make([]models.StatusHistory, 0, len(siblings))
	seenBatch := map[uint]bool{}
	var batchIDs []uint
	for i := range siblings {
		p := &siblings[i]
		ids = append(ids, p.ID)
		history = append(history, models.StatusHistory{
			EntityType: models.EntityBatchItem, EntityID: p.ID,
			FromStatus: string(models.StatusQCPassed), ToStatus: string(models.StatusCut),
			ChangedByID: actor.IDPtr(), Note: note,
		})
		if p.BatchID != 0 && !seenBatch[p.BatchID] {
			seenBatch[p.BatchID] = true
			batchIDs = append(batchIDs, p.BatchID)
		}
	}
	if err := repo.Batch.UpdateBatchItemStatuses(ids, models.StatusCut); err != nil {
		return nil, apperr.Internal("Không hạ được trạng thái QC của phần còn lại").Wrap(err)
	}
	if err := repo.Status.CreateBulk(history); err != nil {
		return nil, apperr.Internal("Không ghi được lịch sử trạng thái").Wrap(err)
	}
	return batchIDs, nil
}

// orderIDsOf gom các đơn của một tập phần sản xuất (bỏ trùng), để guard đóng gói
// và thông báo lỗi làm việc trên đúng tập đơn bị ảnh hưởng.
func orderIDsOf(parts []models.BatchItem) []uint {
	seen := map[uint]bool{}
	var ids []uint
	for i := range parts {
		it := parts[i].OrderItem
		if it == nil || it.OrderID == 0 || seen[it.OrderID] {
			continue
		}
		seen[it.OrderID] = true
		ids = append(ids, it.OrderID)
	}
	return ids
}

// unstampProductionFiles gỡ link in/cắt mà một batch đã đóng dấu lên sản phẩm.
//
// Link in/cắt là của TẤM chứ không của sản phẩm: nó được đóng dấu xuống cột
// print_file_url/cut_file_url của từng sản phẩm để export, tem và trạm QC đọc
// được mà không phải nhập lại từng dòng. Khi tấm đó chết, cái dấu ấy phải gỡ —
// nếu không sản phẩm mang theo link của tấm đã vứt đi sang batch mới, và export
// của batch mới (có nhánh dự phòng đọc cột của sản phẩm) sẽ chỉ thợ in đúng cái
// file vừa làm hỏng cả tấm. Đây chính là điều lệnh xoá batch đã làm; huỷ batch
// cũng phải làm, vì lý do y hệt.
//
// itemIDs rỗng = mọi sản phẩm trong batch (huỷ cả tấm). Có itemIDs = chỉ những
// sản phẩm đó (QC fail lẻ — các sản phẩm còn lại vẫn đang được làm từ tấm này).
// Phải gọi TRONG transaction, và TRƯỚC khi phần sản xuất bị xoá khỏi batch (tập
// sản phẩm được suy ra từ batch_items).
func unstampProductionFiles(repo *repositories.Repositories, batchIDs, itemIDs []uint) error {
	for _, batchID := range batchIDs {
		links, err := repo.Batch.LinksForBatch(batchID)
		if err != nil {
			return apperr.Internal("Không đọc được link sản xuất của batch").Wrap(err)
		}
		for _, link := range links {
			column := "print_file_url"
			if link.Kind == models.BatchLinkCut {
				column = "cut_file_url"
			}
			if len(itemIDs) == 0 {
				_, err = repo.OrderItem.ClearProductionFileForBatch(batchID, column, link.URL)
			} else {
				_, err = repo.OrderItem.ClearProductionFileForItems(itemIDs, column, link.URL)
			}
			if err != nil {
				return apperr.Internal("Không gỡ được link sản xuất khỏi sản phẩm").Wrap(err)
			}
		}
	}
	return nil
}
