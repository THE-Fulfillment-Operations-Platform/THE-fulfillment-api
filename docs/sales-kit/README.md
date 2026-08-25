# Sales kit — bộ tài liệu thương mại hoá

Bộ tài liệu bán hàng cho nền tảng vận hành xưởng fulfillment (tên thương mại tạm: **FFM Ops** — đổi khi chốt brand).

**Đọc trước:** `00-huong-dan-bo-tai-lieu.pdf` — giải thích vì sao cần từng tài liệu, ưu tiên, thứ tự làm.

| # | File | Gửi ai |
|---|---|---|
| 00 | Hướng dẫn bộ tài liệu | Nội bộ |
| 01 | Sales one-pager | Khách |
| 02 | Pitch deck (slide ngang) | Khách |
| 03 | Case study xưởng tham chiếu (mẫu, cần điền số) | Khách sau khi điền |
| 04 | Kịch bản demo 30 phút | Nội bộ |
| 05 | Sơ đồ quy trình vận hành | Khách |
| 06 | Danh mục tính năng theo vai trò | Khách |
| 07 | Bảng giá & gói dịch vụ (phần B nội bộ) | Khách (chỉ phần A) |
| 08 | Mẫu đề xuất giải pháp & lộ trình | Khách |
| 09 | Tổng quan kỹ thuật & bảo mật | IT của khách |
| 10 | FAQ & xử lý phản đối | Nội bộ |
| 11 | Bảng khảo sát nhu cầu | Nội bộ, dùng tại xưởng khách |
| 12 | SOW & SLA mẫu | Phụ lục hợp đồng |
| 13 | Checklist onboarding & HDSD theo vai trò | Khách sau khi ký |

## Sửa và build lại

Nguồn là HTML trong `html/` (CSS chung: `kit.css` cho tài liệu, `deck.css` cho slide).
Ô nền vàng (`<span class="ph">`) là chỗ cần điền / lãnh đạo chốt.

```bash
bash docs/sales-kit/build.sh          # build tất cả
bash docs/sales-kit/build.sh 07       # chỉ build file có "07" trong tên
```

Cần Chrome hoặc Edge cài sẵn (headless print-to-pdf). Đổi tên thương mại: tìm–thay `FFM Ops` / `FFM OPS` trong `html/` rồi build lại.
