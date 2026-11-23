package repositories

import "strings"

// Ô "Batch ID" của màn Đơn hàng (?batch= trên /api/items) và của màn Batch
// (?batch= trên /api/batches) tra MỘT batch theo đúng mã người dùng thấy
// (batches.code, vd "#102909"), gõ có hay không có dấu # đều được.
//
// Khớp ĐÚNG mã, không khớp chuỗi con (đổi 23/11/2026). Trước đó là "chứa chuỗi":
// màn Đơn hàng in id nội bộ dạng "#1909", khách gõ "1909" vào ô này ở màn Batch
// và ra #101909 (id 909, tạo 31/10) — một batch khác hẳn #102909 (id 1909) thật
// sự chứa sản phẩm. Mã batch do hệ thống sinh ("#" + (101000 + id)), người dùng
// chép/quét nguyên mã, nên khớp đúng không làm mất gì; tìm theo một phần mã vẫn
// có ở ô tìm chung của bảng sản xuất (BatchFilter.Q).

// batchCodeKey chuẩn hoá mã batch người dùng gõ: bỏ khoảng trắng hai đầu, bỏ
// (các) dấu # ở đầu, chữ thường. "#102909", "102909", " # 102909 " → "102909".
// Rỗng = không lọc.
func batchCodeKey(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "#")))
}

// batchCodeEqualsSQL so batches.code với một batchCodeKey: cùng bỏ # đầu, cùng
// chữ thường ở cả hai phía. LTRIM(x, '#') chạy trên cả Postgres lẫn SQLite (test).
// Không dùng được unique index của code, nhưng bảng batches chỉ vài nghìn dòng.
const batchCodeEqualsSQL = "LOWER(LTRIM(batches.code, '#')) = ?"
