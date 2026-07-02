# Backend Handoff Report for Frontend

> Tài liệu này mô tả CHÍNH XÁC API backend MVP của THE Fulfillment để Frontend (Nuxt 3) tích hợp.
> Mọi endpoint dưới đây đã được chạy thật trên PostgreSQL và verify end-to-end.

---

## 1. Tổng quan backend

- **Stack đã dùng:** Go 1.26 + Gin + GORM + PostgreSQL. Auth JWT (`golang-jwt/v5`) + bcrypt. Config qua `.env`. OpenAPI/Swagger embed sẵn.
- **Thư mục backend:** repo `THE-fulfillment-api` (backend nằm thẳng ở **root**, không lồng `apps/`). Layering: `handler → service → repository`.
- **Cách chạy backend local:**
  ```bash
  # chạy ngay tại root repo THE-fulfillment-api
  cp .env.example .env      # điền DB creds + JWT_SECRET
  go run ./cmd/server
  ```
- **Base API URL:** `http://localhost:8090` (đổi qua `PORT` trong `.env`). Mọi route nghiệp vụ có prefix `/api`.
- **File env cần có:** `.env` ở root repo (xem `.env.example`). Tối thiểu: `DB_HOST, DB_PORT, DB_USER, DB_PASSWORD, DB_NAME, JWT_SECRET, PORT, CORS_ALLOWED_ORIGINS, SEED_ON_START`.
- **Database dùng:** PostgreSQL (>= 13). Tạo trước: `CREATE DATABASE the_fulfillment;`
- **Migration:** GORM **AutoMigrate** tự chạy mỗi lần khởi động — FE không cần làm gì.
- **Seed data:** tự chạy khi `SEED_ON_START=true` (mặc định). Idempotent. Tạo sẵn user mọi role, materials, SKU (gồm 1 combo), 1 seller + store, 4 đơn demo, 1 batch demo.

### Swagger / OpenAPI
- Swagger UI: `GET http://localhost:8090/docs`
- OpenAPI spec: `GET http://localhost:8090/openapi.yaml`
- Health check (public, không cần token): `GET http://localhost:8090/health`

---

## 2. Auth & phân quyền

- **Cơ chế auth:** **JWT thật** (HS256), KHÔNG phải mock. Token có hạn 72h (đổi qua `JWT_EXPIRES_HOURS`).
- **Login endpoint:** `POST /api/auth/login`
- **Header FE cần gửi sau login:** `Authorization: Bearer <token>` cho **mọi** request `/api/*` (trừ `/api/auth/login`, `/health`, `/docs`, `/openapi.yaml`).
- **Danh sách role:** `OWNER`, `ADMIN`, `OPS`, `DESIGNER`, `PRODUCTION`, `QC`, `PACKING`, `SHIPPING`, `SELLER`.
- **Route nào cần auth:** TẤT CẢ `/api/*` trừ `/api/auth/login`.
- **Route cần role đặc biệt:** (xem bảng chi tiết ở mục 6)
  - `OWNER/ADMIN`: quản lý user, audit-logs, xóa master data.
  - `OWNER/ADMIN/OPS`: tạo/sửa seller/store/material/SKU, import đơn.
  - `+DESIGNER`: design queue, cập nhật design item, tạo batch.
  - `+PRODUCTION`: cập nhật status batch.
  - `+QC`: QC scan/pass/fail.
  - `+PACKING`: packing scan.
  - `+SHIPPING`: tạo handoff.
  - `SELLER`: chỉ `/api/seller/*` (xem trạng thái tổng của chính seller đó).
- **Lỗi auth:** thiếu/sai token → `401 UNAUTHORIZED`. Đúng token nhưng sai role → `403 FORBIDDEN`.

---

## 3. Tài khoản seed/demo để FE test

Mật khẩu chung: **`Password123!`** (đổi qua `SEED_DEMO_PASSWORD`).

| Role | Email | Password | Ghi chú |
| --- | --- | --- | --- |
| OWNER | owner@the.local | Password123! | Toàn quyền |
| ADMIN | admin@the.local | Password123! | Quản trị + audit logs |
| OPS | ops@the.local | Password123! | **Dùng test nhiều nhất** — làm được hầu hết flow vận hành |
| DESIGNER | designer@the.local | Password123! | Design queue + tạo batch |
| PRODUCTION | production@the.local | Password123! | Cập nhật status batch |
| QC | qc@the.local | Password123! | QC scan/pass/fail |
| PACKING | packing@the.local | Password123! | Packing scan |
| SHIPPING | shipping@the.local | Password123! | Handoff THE |
| SELLER | seller@the.local | Password123! | Chỉ xem đơn của seller (seller_id=1) |

**Dữ liệu seed sẵn:** seller `SELLER01` (id=1) + store `Etsy-Demo`; materials `WOOD/MICA/ACRYLIC/METAL`; SKU `WOOD-01, WOOD-03, MICA-02, ACR-05, COMBO-01` (COMBO-01 = combo Gỗ+Mica); 4 đơn `ORD-000001..ORD-000004` (item code dạng `ORD-000001_1`); 1 batch `#101001` (WOOD, status PRINTED).

---

## 4. Response format chuẩn

Mọi response (kể cả lỗi) đều dùng **envelope thống nhất**.

**Success (1 object):**
```json
{ "success": true, "data": { "id": 1, "internal_code": "ORD-000001" } }
```

**Success (list — có pagination):**
```json
{
  "success": true,
  "data": [ { "id": 1 }, { "id": 2 } ],
  "meta": { "page": 1, "page_size": 20, "total": 42, "total_pages": 3 }
}
```

**Error:**
```json
{ "success": false, "error": { "code": "NOT_FOUND", "message": "Order not found" } }
```

**Validation error (422):**
```json
{ "success": false, "error": { "code": "VALIDATION_ERROR", "message": "Request validation failed", "details": "..." } }
```

**Mã lỗi (`error.code`) hay gặp:** `BAD_REQUEST` (400), `UNAUTHORIZED` (401), `FORBIDDEN` (403), `NOT_FOUND` (404), `CONFLICT` (409), `VALIDATION_ERROR`/`UNPROCESSABLE` (422), `INTERNAL` (500).

> **FE lưu ý:** luôn đọc `data` cho payload, `meta` cho phân trang, `error` khi `success=false`. Phân trang qua query `?page=1&page_size=20` (mặc định page=1, size=20, max 200).

---

## 5. Enum/giá trị hằng (FE hard-code dropdown/badge theo đây)

| Nhóm | Field | Giá trị |
| --- | --- | --- |
| Trạng thái nội bộ (item & batch) | `internal_status` / batch `status` | `PENDING`, `PRINTED` (Đã in), `CUT` (Đã cắt), `QC_PASSED` (Đã QC) |
| Trạng thái seller thấy | `seller_status` | `PRODUCTION`, `PACKED`, `HANDED_OFF`, `SHIPPED` |
| Trạng thái design | `design_status` | `PENDING`, `IN_PROGRESS`, `READY`, `MISSING` |
| Độ ưu tiên batch | `priority` | `NORMAL`, `HIGH`, `URGENT` |
| QC | `result` | `PASS`, `FAIL` |
| Note severity | `severity` | `LOW`, `NORMAL`, `HIGH`, `CRITICAL` |
| Note status | `status` | `OPEN`, `IN_PROGRESS`, `WAITING`, `RESOLVED` |
| Entity liên kết note/history | `entity_type` | `ORDER`, `ORDER_ITEM`, `BATCH`, `BATCH_ITEM`, `PACKAGE`, `HANDOFF` |
| Import job | `status` | `PREVIEW`, `COMMITTED`, `FAILED`, `CANCELLED` |
| Package | `status` | `OPEN`, `PACKED` |
| Handoff | `status` | `HANDED_OFF`, `SHIPPED` |

---

## 6. Danh mục API chi tiết

> Cột **Auth**: role được phép. "internal" = mọi role trừ SELLER.

### 6.1 Auth

#### `POST /api/auth/login` — public
Request:
```json
{ "email": "ops@the.local", "password": "Password123!" }
```
Response `200`:
```json
{
  "success": true,
  "data": {
    "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
    "expires_at": "2026-07-01T22:00:00+07:00",
    "user": { "id": 3, "email": "ops@the.local", "full_name": "Ops Demo", "role": "OPS", "seller_id": null, "is_active": true }
  }
}
```

#### `GET /api/me` — auth (mọi role)
Response `200`: object `user` như trên.

---

### 6.2 Users — `OWNER/ADMIN`

| Method | Path | Mô tả |
| --- | --- | --- |
| POST | `/api/users` | Tạo user |
| GET | `/api/users` | List (pagination) |
| GET | `/api/users/:id` | Chi tiết |
| PUT | `/api/users/:id` | Cập nhật |
| DELETE | `/api/users/:id` | Xóa (soft delete) |

`POST` body:
```json
{ "email": "qc2@the.local", "password": "secret123", "full_name": "QC 2", "role": "QC", "seller_id": null, "is_active": true }
```
`PUT` body (tất cả optional): `{ "full_name": "...", "password": "...", "role": "QC", "seller_id": 1, "is_active": false }`

---

### 6.3 Sellers — đọc: internal · ghi: `OWNER/ADMIN/OPS` · xóa: `OWNER/ADMIN`

| Method | Path |
| --- | --- |
| GET | `/api/sellers` · `/api/sellers/:id` |
| POST | `/api/sellers` |
| PUT | `/api/sellers/:id` |
| DELETE | `/api/sellers/:id` |

`POST/PUT` body:
```json
{ "code": "SELLER02", "name": "Shopify Store", "contact_email": "a@b.com", "contact_phone": "0900", "status": "active", "note": "" }
```
Response item:
```json
{ "id": 1, "code": "SELLER01", "name": "Etsy Demo Seller", "status": "active", "stores": [ { "id": 1, "name": "Etsy-Demo", "platform": "Etsy" } ] }
```

---

### 6.4 Stores — đọc: internal · ghi: `OWNER/ADMIN/OPS`

| Method | Path |
| --- | --- |
| GET | `/api/stores?seller_id=1` · `/api/stores/:id` |
| POST | `/api/stores` |
| PUT | `/api/stores/:id` |
| DELETE | `/api/stores/:id` (OWNER/ADMIN) |

`POST/PUT` body: `{ "seller_id": 1, "name": "Etsy-Demo", "platform": "Etsy", "external_ref": "" }`

---

### 6.5 Materials — đọc: internal · ghi: `OWNER/ADMIN/OPS`

| Method | Path |
| --- | --- |
| GET | `/api/materials` · `/api/materials/:id` |
| POST/PUT/DELETE | `/api/materials` ... |

`POST/PUT` body: `{ "code": "GLASS", "name": "Kính", "description": "" }`
Response item: `{ "id": 1, "code": "WOOD", "name": "Gỗ", "description": "Gỗ tự nhiên / plywood" }`

---

### 6.6 SKUs — đọc: internal · ghi: `OWNER/ADMIN/OPS`

| Method | Path |
| --- | --- |
| GET | `/api/skus` · `/api/skus/:id` |
| POST/PUT/DELETE | `/api/skus` ... |

`POST/PUT` body (combo = nhiều material):
```json
{
  "code": "COMBO-02", "name": "Combo 02", "product_name": "Combo Wood+Metal",
  "is_active": true,
  "materials": [ { "material_id": 1, "quantity_per_unit": 1 }, { "material_id": 4, "quantity_per_unit": 1 } ]
}
```
> `is_combo` được backend tự set = true nếu có > 1 material. Response trả kèm `materials[].material`.

---

### 6.7 Import đơn — `OWNER/ADMIN/OPS`

#### `POST /api/orders/import` — preview hoặc preview+commit
Hỗ trợ 2 kiểu:
- **JSON** (khuyến nghị cho FE):
```json
{
  "seller_id": 1,
  "commit": false,
  "filename": "etsy_export.json",
  "rows": [
    {
      "StoreOrderID": "Etsy-9001", "StoreName": "Etsy-Demo", "ShippingMethod": "Standard",
      "Quantity": 1, "ProductName": "Wood Sign", "VariantCode": "", "SKU": "WOOD-01",
      "Design": "", "Mockup": "https://m.example.com/a.png", "EngraveText": "Happy",
      "ShippingName": "Jane", "ShippingAddress1": "1 St", "ShippingAddress2": "",
      "ShippingCity": "Austin", "ShippingZip": "78701", "ShippingProvince": "TX",
      "ShippingCountry": "US", "ShippingPhone": "0900", "ShippingEmail": "jane@x.com",
      "IOSS": "", "Note": ""
    }
  ]
}
```
- **multipart/form-data:** field `file` (CSV), `seller_id`, `commit` (true/false). Header CSV nhận đúng các cột trên (không phân biệt hoa/thường, bỏ qua dấu cách/`_`).

`commit=false` → **preview** (validate, chưa tạo đơn). Response:
```json
{
  "success": true,
  "data": {
    "import_job_id": 1, "status": "PREVIEW",
    "total_rows": 3, "order_count": 1, "valid_rows": 2, "error_rows": 1,
    "errors": [
      { "row_number": 3, "store_order_id": "Etsy-9002", "sku": "NOPE-99",
        "field": "SKU", "error_code": "SKU_UNMAPPED",
        "message": "SKU is not set up in the master", "suggestion": "Setup SKU/NVL cố định" }
    ]
  }
}
```
`commit=true` → response `{ "data": { "preview": {...}, "commit": { ...import job, "status": "COMMITTED", "created_count": 1 } } }`.

**Quy tắc validate (blocking):** thiếu `StoreOrderID`; `Quantity` < 1; thiếu `SKU` hoặc SKU không có trong master (`SKU_UNMAPPED`); thiếu shipping name/address1/country (`ADDR_INVALID`); `Mockup` có nhưng URL sai (`MOCKUP_INVALID`); trùng đơn cùng seller (`ORD_DUPLICATE`).
**Mockup THIẾU = không chặn:** đơn vẫn import, item `design_status=MISSING` và tự sinh 1 Required Attention note (reason `ART_MISSING`).
**Gom đơn:** các dòng cùng `StoreOrderID` → 1 order nhiều item.

#### `POST /api/orders/import/commit`
Body: `{ "import_job_id": 1 }` → tạo order/item từ job đã PREVIEW.

#### `GET /api/import-jobs` · `GET /api/import-jobs/:id`
List/chi tiết job (kèm `errors[]`).

---

### 6.8 Orders — đọc: internal · tạo trực tiếp: `OWNER/ADMIN/OPS`

#### `GET /api/orders` — filter
Query: `seller_id, store_id, sku, status (=seller_status), store_order_id, date_from, date_to, page, page_size`.
Response item (rút gọn, có `items[]`):
```json
{
  "id": 1, "internal_code": "ORD-000001", "store_order_id": "Etsy-7821",
  "seller_id": 1, "store_name": "Etsy-Demo", "seller_status": "PRODUCTION",
  "shipping_name": "John Doe", "shipping_country": "US",
  "created_at": "2026-06-26T22:15:28+07:00",
  "items": [ { "id": 1, "internal_code": "ORD-000001_1", "sku_code": "WOOD-01", "quantity": 1, "internal_status": "PENDING", "design_status": "READY" } ]
}
```

#### `GET /api/orders/:id` — chi tiết đầy đủ (cho màn Order Detail)
Trả order + `seller` + `items[]` (mỗi item có `sku`, `batch_items[]` kèm `batch` & `material`). Dùng cho timeline + item cards.

#### `POST /api/orders` — tạo đơn thủ công (tiện ích, KHÔNG phải luồng chính — xem mục 7)
Body `DirectOrderInput`:
```json
{
  "seller_id": 1, "store_order_id": "MANUAL-1", "store_name": "Etsy-Demo",
  "shipping_name": "John", "shipping_address1": "1 St", "shipping_country": "US",
  "items": [ { "sku_code": "WOOD-01", "product_name": "Wood Sign", "quantity": 1, "mockup_url": "https://m/x.png", "engrave_text": "" } ]
}
```

---

### 6.9 Items & Design Queue

#### `GET /api/items` — internal · Item view (màn chính vận hành)
Query: `sku, status (=internal_status), design_status, batch_id, seller_id, store_id, date_from, date_to, page, page_size`.

#### `GET /api/items/:id` — internal · chi tiết item (kèm order, sku, batch_items, assets)

#### `PATCH /api/items/:id/design` — `OWNER/ADMIN/OPS/DESIGNER`
Cập nhật file design; `set_ready=true` để chuyển sang READY (yêu cầu đã có `print_file_url` **và** `mockup_url`).
Body (mọi field optional):
```json
{ "print_file_url": "https://files/p.pdf", "cut_file_url": "https://files/c.svg", "mockup_url": "https://m/x.png", "design_url": "", "set_ready": true }
```
Response: item sau cập nhật (kèm `assets[]` versioned).

#### `GET /api/design-queue` — `OWNER/ADMIN/OPS/DESIGNER`
Item cần design (design_status != READY). Cùng filter như `/api/items`.

#### `GET /api/design-queue/material-buckets` — đếm item design-ready chưa batch theo material
Response:
```json
{ "success": true, "data": [ { "material_id": 1, "material_code": "WOOD", "material_name": "Gỗ", "item_count": 1 }, { "material_id": 2, "material_code": "MICA", "material_name": "Mica", "item_count": 1 } ] }
```

#### `GET /api/design-queue/material/:materialId/items` — item design-ready chưa batch của 1 material (để chọn tạo batch)

---

### 6.10 Batches

| Method | Path | Auth |
| --- | --- | --- |
| GET | `/api/batches` (filter: `material_id, status, priority, date_from, date_to`) | internal |
| GET | `/api/batches/:id` (kèm items, sku, mockup, file, material) | internal |
| POST | `/api/batches` | `OWNER/ADMIN/OPS/DESIGNER` |
| PATCH | `/api/batches/:id/status` | `OWNER/ADMIN/OPS/PRODUCTION/DESIGNER` |

#### `POST /api/batches` — tạo batch cho 1 material
```json
{ "material_id": 1, "order_item_ids": [3], "priority": "NORMAL", "due_date": null, "note": "" }
```
Response:
```json
{ "success": true, "data": { "batch": { "id": 2, "code": "#101002", "material_id": 1, "status": "PENDING", "items": [...] }, "skipped_item_ids": [] } }
```
> **Combo nhiều material:** gọi endpoint này **1 lần cho mỗi material** với cùng `order_item_ids`. Item nào SKU không chứa material đó, hoặc đã batch cho material đó, sẽ nằm trong `skipped_item_ids`.

#### `PATCH /api/batches/:id/status`
```json
{ "status": "CUT", "note": "thợ cắt xong" }
```
Cascade xuống tất cả batch_items và tự tính lại `internal_status` của item liên quan.

---

### 6.11 QC — `OWNER/ADMIN/OPS/QC`

#### `POST /api/qc/scan` — quét tem item, trả info + mockup để đối chiếu
Body: `{ "code": "ORD-000001_1" }` (hoặc `{ "item_id": 1 }`)
Response:
```json
{
  "success": true,
  "data": {
    "item_id": 1, "item_code": "ORD-000001_1", "order_code": "ORD-000001", "store_order_id": "Etsy-7821",
    "sku_code": "WOOD-01", "product_name": "Personalized Wood Sign", "engrave_text": "",
    "mockup_url": "https://mockups.example.com/etsy-7821-1.png", "internal_status": "CUT",
    "batches": [ { "batch_item_id": 1, "batch_code": "#101001", "material_code": "WOOD", "status": "CUT" } ]
  }
}
```

#### `POST /api/qc/pass` — xác nhận khớp mockup → Đã QC
Body: `{ "code": "ORD-000001_1", "batch_item_id": null, "note": "khớp mockup" }`
- `batch_item_id` optional. Bỏ trống = pass **toàn bộ** part của item.
- Yêu cầu item có `mockup_url` và part không còn `PENDING`.
Response: order item sau update (`internal_status` có thể thành `QC_PASSED`).

#### `POST /api/qc/fail` — không khớp → tạo Required Attention/rework
Body: `{ "code": "ORD-000001_1", "defect_code": "QC_FAILED_MINOR", "note": "lệch màu" }`
Response `201`: note vừa tạo (`severity=HIGH`, `is_required_attention=true`).

---

### 6.12 Packing — `OWNER/ADMIN/OPS/PACKING`

#### `POST /api/packing/scan` — quét item đã QC vào package
Body: `{ "order_id": 1, "code": "ORD-000001_1" }` (hoặc `item_id`). `order_id` optional.
- **Chặn** nếu item chưa `QC_PASSED` (`422`), hoặc quét vượt số lượng (`409`).
Response:
```json
{
  "success": true,
  "data": {
    "package_id": 1, "package_code": "PKG-000001", "order_id": 1, "order_code": "ORD-000001",
    "fully_packed": true,
    "lines": [ { "order_item_id": 1, "item_code": "ORD-000001_1", "sku_code": "WOOD-01", "expected": 1, "scanned": 1 } ]
  }
}
```
> Khi `fully_packed=true`, order `seller_status` chuyển `PRODUCTION → PACKED`.

#### `GET /api/packing/order/:id` — trạng thái package của 1 order (expected vs scanned)

---

### 6.13 Handoffs (THE) — `OWNER/ADMIN/OPS/PACKING/SHIPPING`

#### `POST /api/handoffs` — bàn giao THE (yêu cầu package đủ item)
Body: `{ "order_id": 1, "box_type": "Box-S", "weight_grams": 300, "note": "" }` (hoặc `package_id`).
- **Chặn** (`422`) nếu package chưa đủ item.
Response `201`:
```json
{
  "success": true,
  "data": {
    "id": 1, "code": "THE-HO-000001", "order_id": 1, "package_id": 1,
    "carrier": "THE", "status": "HANDED_OFF",
    "tracking_number": "", "label_url": "", "handed_off_at": "2026-06-26T22:20:00+07:00"
  }
}
```
> `tracking_number`/`label_url` **luôn rỗng ở MVP** (chưa nối API THE thật — xem mục 7). Sau handoff, order `seller_status → HANDED_OFF`.

#### `GET /api/handoffs` — list handoff.

---

### 6.14 Notes / Required Attention — internal (mọi role nội bộ)

| Method | Path |
| --- | --- |
| POST | `/api/notes` |
| GET | `/api/notes?status=&severity=&entity_type=&entity_id=&required_attention=true` |
| GET/PUT/DELETE | `/api/notes/:id` |

`POST` body:
```json
{
  "title": "Thiếu file cắt", "body": "...", "reason_code": "ART_MISSING",
  "severity": "HIGH", "status": "OPEN", "is_required_attention": true,
  "entity_type": "BATCH", "entity_id": 1, "owner_role": "DESIGNER", "due_date": null
}
```
Response item:
```json
{ "id": 5, "title": "QC Fail: ORD-000003_1", "severity": "HIGH", "status": "OPEN", "reason_code": "QC_FAILED_MINOR", "is_required_attention": true, "entity_type": "ORDER_ITEM", "entity_id": 3, "owner_role": "PRODUCTION" }
```

---

### 6.15 Seller View — `SELLER` (chỉ đơn của chính seller, trạng thái tổng)

#### `GET /api/seller/orders?status=&store_order_id=&date_from=&date_to=`
Response (đã **lọc bỏ** mọi chi tiết nội bộ — không có internal_status/print/cut/QC):
```json
{
  "success": true,
  "data": [ { "id": 1, "internal_code": "ORD-000001", "store_order_id": "Etsy-7821", "store_name": "Etsy-Demo", "status": "PRODUCTION", "item_count": 1, "created_at": "..." } ],
  "meta": { "total": 5 }
}
```

#### `GET /api/seller/orders/:id`
Như trên + `items[]` chỉ gồm `{ sku_code, product_name, variant_code, quantity, mockup_url }`. Trả `403` nếu order không thuộc seller.

---

### 6.16 Audit Logs — `OWNER/ADMIN`
`GET /api/audit-logs` → list hành động (`action, actor_email, summary, entity_type, entity_id, created_at`).

---

## 7. Phần MOCK / SKELETON / TODO (FE cần biết)

| Hạng mục | Trạng thái | Ảnh hưởng FE |
| --- | --- | --- |
| **THE label/tracking API** | **Placeholder (NoopCarrier)** — handoff chỉ lưu nội bộ | `tracking_number` & `label_url` **luôn rỗng**; đừng build UI tracking thật. `status` dừng ở `HANDED_OFF`. |
| **Seller status `SHIPPED`** | Chưa tự đạt tới (cần tracking phase sau) | Badge `SHIPPED` có enum nhưng hiện chưa xuất hiện qua flow. |
| **`POST /api/orders` (direct create)** | Tiện ích, **chưa chạy full pipeline dedup/validate** như import | Ưu tiên dùng `/api/orders/import` cho luồng chính. |
| **Upload file design/print/cut/mockup** | **Không có endpoint upload binary** | FE tự upload lên storage (S3/Cloudinary/...) rồi gửi **URL** vào `PATCH /items/:id/design`. Backend lưu URL + version. |
| **Refresh token** | Chưa có | Token hết hạn 72h → FE bắt 401 và cho login lại. |
| **Filter `seller_id`/`store_id` trên `/api/items`** | Có nhưng chưa join sâu để filter theo store ở mọi nhánh | Dùng được; nếu cần lọc phức tạp hơn thì báo BE. |
| **WMS/Finance/CS/RMA, BI dashboard** | Ngoài scope MVP — **chưa có** | Không có API. |

**Đã hoàn thành & verify chạy thật:** auth/RBAC, CRUD seller/store/material/SKU, import preview+commit (JSON & CSV), orders/items/order-detail, design queue + material buckets + update design, batch theo material (gồm combo tách nhiều batch), production status + lịch sử, QC scan/pass/fail, packing scan (expected vs scanned, block), handoff THE (lưu log), notes/required attention, seller view (sanitized), audit logs.

---

## 8. Ghi chú tích hợp Nuxt 3

- **Base URL:** đặt `runtimeConfig.public.apiBase = "http://localhost:8090"`. Mọi call thêm `/api/...`.
- **CORS:** backend mặc định `CORS_ALLOWED_ORIGINS=*`, đã bật `Access-Control-Allow-Credentials`. Nuxt dev (`http://localhost:3000`) gọi trực tiếp được.
- **Gửi token:** lưu `token` sau login (cookie/`useState`/pinia), set header `Authorization: Bearer <token>` qua `$fetch`/`useFetch` (dùng `onRequest` interceptor).
- **Unwrap envelope:** viết composable đọc `res.data` (và `res.meta` cho list); nếu `res.success === false` thì throw theo `res.error`.
- **Phân trang:** truyền `?page=&page_size=`; đọc `meta.total` / `meta.total_pages`.
- **Ngày:** mọi timestamp là RFC3339 (`2026-06-26T22:15:28+07:00`).
- **Item code = mã scan QC/packing:** field `internal_code` của order_item (vd `ORD-000001_1`) chính là giá trị quét ở màn QC/Packing.

---

## 9. Smoke test mẫu (đã chạy thật, FE tham chiếu thứ tự flow)

1. `POST /api/auth/login` (ops) → lấy token.
2. `GET /api/orders` → 4 đơn seed.
3. `GET /api/design-queue` + `GET /api/design-queue/material-buckets`.
4. `PATCH /api/batches/{id}/status {status:"CUT"}`.
5. `POST /api/qc/scan {code:"ORD-000001_1"}` → `POST /api/qc/pass {code:"ORD-000001_1"}`.
6. `POST /api/packing/scan {code:"ORD-000001_1"}` → `fully_packed:true`.
7. `POST /api/handoffs {order_id:1}` → `THE-HO-000001`, order `seller_status=HANDED_OFF`.
8. Login seller → `GET /api/seller/orders` (không lộ internal status). Seller gọi `/api/orders` → `403`.
