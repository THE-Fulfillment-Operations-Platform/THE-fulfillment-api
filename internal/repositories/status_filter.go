package repositories

import (
	"strings"

	"the-fulfillment/backend/internal/models"
)

// Lọc chọn nhiều (chủ xưởng 22/11: tick "Chờ xử lý" + "Đã có file SX" + "Đã in"
// một lượt thay vì lọc từng cái).
//
// Ô Trạng thái của màn Batch và màn Đơn hàng có những mục KHÔNG phải trạng thái
// lưu trong DB mà là nhãn suy ra (đủ link in + cắt, đơn đã đóng gói / bàn giao…).
// Một lựa chọn đơn vẫn gửi bằng tham số cũ (?status= + ?files= / ?handover= /
// ?packed= / ?handed_over=, AND với nhau). Chọn nhiều thì mỗi mục là một "token"
// trong ?status=A,B,C (hoặc ?status=A&status=B) và các token OR với nhau — một
// dòng khớp bất kỳ token nào là ra. Token là trạng thái thật thì nghĩa y như
// ?status= đơn cũ (status=PENDING vẫn là MỌI batch / sản phẩm PENDING).
const (
	// "Đã có file SX": PENDING và đã đủ file sản xuất (batch: filesReadyCond;
	// sản phẩm: itemFilesReadyCond). = ?status=PENDING&files=ready.
	StatusTokenFilesReady = "FILES_READY"
	// "Chờ xử lý" như nhãn trên bảng: PENDING mà CHƯA đủ file. = ?status=PENDING&files=missing.
	StatusTokenFilesMissing = "FILES_MISSING"
	// Batch: QC_PASSED và mọi phần còn sống đã rời xưởng (= ?status=QC_PASSED&handover=done).
	// Sản phẩm: đơn đã bàn giao THE / đã gửi / đã giao (= ?handed_over=true).
	StatusTokenHandedOver = "HANDED_OVER"
	// Chỉ batch — "Đã QC" như nhãn: QC_PASSED chưa bàn giao hết (= ?status=QC_PASSED&handover=pending).
	StatusTokenHandoverPending = "HANDOVER_PENDING"
	// Chỉ sản phẩm — "Đã đóng gói": đơn PACKED, đã duyệt, chưa huỷ (= ?packed=true).
	StatusTokenPacked = "PACKED"
)

// normalizeTokens bỏ ô trống, bỏ trùng, giữ thứ tự.
func normalizeTokens(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// orGroup nối các vế thành MỘT điều kiện "((a) OR (b) …)", luôn có ngoặc, để
// OR không tràn ra chuỗi AND của các bộ lọc khác.
func orGroup(clauses []string) string {
	return "((" + strings.Join(clauses, ") OR (") + "))"
}

// batchStatusCond dựng điều kiện cho một nhóm token trạng thái của màn Batch.
// ok=false khi không có token nào (= không lọc).
func batchStatusCond(tokens []string) (sql string, args []interface{}, ok bool) {
	tokens = normalizeTokens(tokens)
	if len(tokens) == 0 {
		return "", nil, false
	}
	var clauses []string
	var plain []string
	for _, t := range tokens {
		switch t {
		case StatusTokenFilesReady:
			clauses = append(clauses, filesReadyCond)
		case StatusTokenFilesMissing:
			clauses = append(clauses, "batches.status = 'PENDING' AND NOT ("+filesReadyCond+")")
		case StatusTokenHandedOver:
			clauses = append(clauses, handoverDoneCond)
		case StatusTokenHandoverPending:
			clauses = append(clauses, "batches.status = 'QC_PASSED' AND NOT ("+handoverDoneCond+")")
		default:
			// Trạng thái thật (hoặc giá trị lạ: không khớp dòng nào, như ?status= cũ).
			plain = append(plain, t)
		}
	}
	if len(plain) > 0 {
		clauses = append(clauses, "batches.status IN ?")
		args = append(args, plain)
	}
	return orGroup(clauses), args, true
}

// itemChipActiveSQL là chipActiveOrder viết thành một vế SQL để đặt trong nhóm OR.
const itemChipActiveSQL = "orders.review_status = ? AND orders.cancellation_status <> ?"

// itemStatusCond dựng điều kiện cho một nhóm token trạng thái của màn Đơn hàng /
// Sản phẩm (câu truy vấn đã join orders). Mỗi token khớp đúng nhóm dòng mà lựa
// chọn đơn tương ứng trả về (xem OrderItemRepository.baseQuery):
//   - QC_PASSED: "Đã QC" — chỉ dòng của đơn còn trong xưởng, đã duyệt, chưa huỷ;
//   - FILES_READY / FILES_MISSING: tách PENDING theo file sản xuất;
//   - PACKED / HANDED_OVER: theo seller_status của đơn;
//   - trạng thái thật khác: internal_status = X.
func itemStatusCond(tokens []string) (sql string, args []interface{}, ok bool) {
	tokens = normalizeTokens(tokens)
	if len(tokens) == 0 {
		return "", nil, false
	}
	var clauses []string
	var plain []string
	for _, t := range tokens {
		switch t {
		case StatusTokenFilesReady:
			clauses = append(clauses, "order_items.internal_status = ? AND "+itemFilesReadyCond)
			args = append(args, models.StatusPending)
		case StatusTokenFilesMissing:
			clauses = append(clauses, "order_items.internal_status = ? AND NOT ("+itemFilesReadyCond+")")
			args = append(args, models.StatusPending)
		case StatusTokenPacked:
			clauses = append(clauses, "orders.seller_status = ? AND "+itemChipActiveSQL)
			args = append(args, models.SellerStatusPacked, models.ReviewApproved, models.CancellationSeller)
		case StatusTokenHandedOver:
			clauses = append(clauses, "orders.seller_status IN ?")
			args = append(args, models.HandedOverStatuses)
		case string(models.StatusQCPassed):
			clauses = append(clauses, "order_items.internal_status = ? AND orders.seller_status = ? AND "+itemChipActiveSQL)
			args = append(args, models.StatusQCPassed, models.SellerStatusProduction, models.ReviewApproved, models.CancellationSeller)
		default:
			plain = append(plain, t)
		}
	}
	if len(plain) > 0 {
		clauses = append(clauses, "order_items.internal_status IN ?")
		args = append(args, plain)
	}
	return orGroup(clauses), args, true
}
