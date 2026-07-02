# THE Fulfillment Operations API (Backend MVP)

Backend cho **THE Fulfillment Operations Platform** — hệ thống quản lý đơn hàng và
vận hành xưởng fulfillment: import đơn → design → batch theo nguyên vật liệu →
in/cắt → QC đối chiếu mockup → packing → handoff THE.

## Stack

- **Go** + **Gin** (HTTP)
- **PostgreSQL** + **GORM** (ORM, AutoMigrate cho MVP)
- **JWT** auth (`golang-jwt`) + **bcrypt** password hashing
- Config qua **.env** (`godotenv`)
- JSON REST API với **response envelope** thống nhất
- **OpenAPI/Swagger** docs tại `/docs`

## Kiến trúc

Tách lớp rõ ràng: **handler → service → repository**. Handler chỉ bind/validate
+ build response; service chứa business logic; repository chứa truy vấn DB.

```
THE-fulfillment-api/           # repo root (backend nằm thẳng ở root, không lồng apps/)
├── cmd/server/main.go          # entrypoint: connect DB, migrate, seed, serve
├── internal/
│   ├── config/                 # đọc .env / env vars (không hardcode DB)
│   ├── database/               # GORM connect + AutoMigrate
│   ├── models/                 # GORM models (19 nhóm bảng) + enums
│   ├── repositories/           # data-access layer
│   ├── services/               # business logic + status machine
│   ├── handlers/               # HTTP layer (DTO bind, response)
│   ├── middleware/             # logger, recovery, CORS, JWT auth, RBAC
│   ├── routes/                 # wiring route + phân quyền theo role
│   ├── auth/                   # JWT issue/parse + bcrypt
│   ├── shipping/               # carrier adapter (NoopCarrier cho MVP)
│   ├── docs/                   # OpenAPI yaml (embed) + Swagger UI
│   ├── apperr/ + response/     # error type + envelope JSON thống nhất
│   └── seed/                   # seed demo data
└── .env.example
```

## Yêu cầu

- Go >= 1.24 (đã test trên 1.26)
- PostgreSQL >= 13

## Cấu hình PostgreSQL

1. Tạo database:

```sql
CREATE DATABASE the_fulfillment;
-- (tuỳ chọn) tạo role riêng:
-- CREATE USER the_app WITH PASSWORD 'your_password';
-- GRANT ALL PRIVILEGES ON DATABASE the_fulfillment TO the_app;
```

2. Copy file env và điền thông tin kết nối:

```bash
cp .env.example .env
```

Sửa `.env`:

```
DB_HOST=localhost
DB_PORT=5432
DB_USER=postgres
DB_PASSWORD=your_password
DB_NAME=the_fulfillment
JWT_SECRET=một-chuỗi-bí-mật-dài
```

## Chạy backend local

```bash
# chạy ngay tại root repo THE-fulfillment-api
go mod download
go run ./cmd/server
```

Hoặc build binary:

```bash
go build -o bin/server ./cmd/server
./bin/server            # Windows: .\bin\server.exe
```

Khi khởi động, app sẽ:

1. Kết nối PostgreSQL theo `.env`.
2. **AutoMigrate** toàn bộ bảng.
3. **Seed** demo data (nếu `SEED_ON_START=true`).
4. Lắng nghe tại `http://localhost:8090`.

- Health check: `GET http://localhost:8090/health`
- API docs (Swagger UI): `http://localhost:8090/docs`
- OpenAPI spec: `http://localhost:8090/openapi.yaml`

## Migration & Seed

- **Migration:** dùng GORM `AutoMigrate`, chạy tự động mỗi lần khởi động (MVP).
- **Seed:** chạy tự động khi `SEED_ON_START=true`. Idempotent — master data dùng
  find-or-create, demo orders chỉ tạo khi DB chưa có order nào.

Seed tạo sẵn:

- 9 role + **1 user demo cho mỗi role**
- 4 material (Gỗ/Mica/Acrylic/Metal), 5 SKU (gồm 1 combo Gỗ+Mica)
- 1 seller + store, vài đơn demo ở các trạng thái khác nhau, 1 batch demo

### Tài khoản demo

Mật khẩu mặc định: `Password123!` (đổi qua `SEED_DEMO_PASSWORD`).

| Email | Role |
|---|---|
| owner@the.local | OWNER |
| admin@the.local | ADMIN |
| ops@the.local | OPS |
| designer@the.local | DESIGNER |
| production@the.local | PRODUCTION |
| qc@the.local | QC |
| packing@the.local | PACKING |
| shipping@the.local | SHIPPING |
| seller@the.local | SELLER |

## Luồng thử nhanh (curl)

```bash
# 1. Login
curl -s -X POST localhost:8090/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"ops@the.local","password":"Password123!"}'

# 2. Lấy token rồi gọi API (thay $TOKEN)
curl -s localhost:8090/api/orders -H "Authorization: Bearer $TOKEN"
curl -s localhost:8090/api/design-queue -H "Authorization: Bearer $TOKEN"
curl -s localhost:8090/api/batches -H "Authorization: Bearer $TOKEN"
```

## Response format

Mọi response dùng envelope thống nhất:

```jsonc
// success
{ "success": true, "data": { ... }, "meta": { "page": 1, "total": 42 } }
// error
{ "success": false, "error": { "code": "NOT_FOUND", "message": "Order not found" } }
```

## Phân quyền (RBAC)

JWT bắt buộc cho mọi route `/api/*` (trừ `/api/auth/login`). Middleware kiểm tra
role theo từng nhóm route. Seller chỉ truy cập `/api/seller/*` và **chỉ thấy
trạng thái tổng** (PRODUCTION/PACKED/HANDED_OFF/SHIPPED), không thấy chi tiết nội
bộ (Đã in/Đã cắt/Đã QC).

## Master Data / SKU–NVL Setup

Nhóm route giúp setup **Materials, SKUs và mapping SKU → nguyên vật liệu** — điều
kiện để gom batch và import đơn. Xem chi tiết ở `../THE-fulfillment-web/MASTER_DATA.md`.

- CRUD: `/api/materials`, `/api/skus` (materials của SKU là **tuỳ chọn** — có thể
  tạo SKU chưa map rồi gán NVL sau).
- Import file vận hành cũ (đọc 2 cột `SKU` + `Loại VL`, preview → commit):
  - `POST /api/master-data/import/preview` — multipart `file` (CSV/XLSX) hoặc JSON
    `{ "rows": [ { "sku": "...", "material": "..." } ] }`. Trả về plan: material/SKU/mapping
    mới, danh sách **Needs Review** (1 SKU nhiều Loại VL) và **Missing Material**.
  - `POST /api/master-data/import/commit` — `{ "import_job_id": <id> }`, find-or-create
    material/SKU và thêm mapping cho SKU chỉ có đúng 1 Loại VL (cộng thêm, không xoá).
  - `GET /api/master-data/import-jobs`, `GET /api/master-data/import-jobs/:id`.

Import đơn (`/api/orders/import`) phân biệt 2 lỗi setup: `SKU_UNMAPPED` (SKU chưa có
trong master) và `SKU_NO_MATERIAL` (SKU có nhưng chưa gán NVL).

## Ghi chú phase sau

- Carrier/THE label & tracking: hiện dùng `shipping.NoopCarrier` (không gọi API
  thật). Khi có API THE, implement `shipping.Carrier` và thay trong `main.go` —
  không cần sửa service.
- Migration: MVP dùng AutoMigrate; có thể chuyển sang công cụ migration (golang-migrate)
  mà không đổi model.
