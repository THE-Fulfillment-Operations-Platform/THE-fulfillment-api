-- TUỲ CHỌN — index trigram cho ô SKU "có chứa" ở màn Đơn hàng / Sản phẩm
-- (commit "Lọc sản phẩm theo SKU: tìm có chứa…", 17/11/2026).
--
-- API KHÔNG tự chạy file này (không nằm trong migrate lúc khởi động). Chỉ chạy
-- tay trên Postgres prod (Supabase) khi bước 1 cho thấy cần.
--
-- Truy vấn mới: LOWER(order_items.sku_code) LIKE '%fsh%' ESCAPE '\'. Có % ở đầu
-- nên index btree hiện có trên sku_code không dùng được: Postgres quét cả bảng
-- order_items. Vài chục nghìn dòng thì quét vẫn chỉ vài ms, KHÔNG cần index.
-- Index trigram chỉ giúp khi chuỗi gõ từ 3 ký tự trở lên (2 ký tự thì pg_trgm
-- không rút được trigram nào, Postgres vẫn quét bảng).

-- ---------------------------------------------------------------------------
-- 1. ĐỌC (chạy trước, không ghi gì)
-- ---------------------------------------------------------------------------
SELECT count(*) AS so_dong_order_items,
       pg_size_pretty(pg_total_relation_size('order_items')) AS dung_luong
FROM order_items;

SELECT extname, extversion FROM pg_extension WHERE extname = 'pg_trgm';

SELECT indexname, indexdef FROM pg_indexes
WHERE tablename = 'order_items' AND indexname = 'idx_order_items_sku_code_trgm';

-- Thời gian thật của câu lọc (EXPLAIN ANALYZE chỉ chạy SELECT, không ghi):
EXPLAIN ANALYZE
SELECT count(*) FROM order_items oi
JOIN orders o ON o.id = oi.order_id AND o.deleted_at IS NULL
WHERE oi.deleted_at IS NULL
  AND oi.cancellation_status NOT IN ('SELLER_CANCELLED', 'APPROVED')
  AND LOWER(oi.sku_code) LIKE '%fsh%' ESCAPE '\';
-- "Execution Time" dưới ~50 ms thì DỪNG Ở ĐÂY, chưa cần index.

-- ---------------------------------------------------------------------------
-- 2. GHI (chỉ khi bước 1 chậm). Bật auto-commit trong DBeaver: CONCURRENTLY
--    không chạy được trong transaction. Không khoá ghi bảng order_items.
-- ---------------------------------------------------------------------------
-- pg_trgm thường đã có (API tạo lúc khởi động cho ô tìm Store Order ID). Bước 1
-- không ra dòng nào thì mới chạy câu dưới:
-- CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_order_items_sku_code_trgm
  ON order_items USING gin (LOWER(sku_code) gin_trgm_ops)
  WHERE deleted_at IS NULL;

-- Kiểm: chạy lại EXPLAIN ANALYZE ở bước 1, mong thấy "Bitmap Index Scan on
-- idx_order_items_sku_code_trgm". Index hỏng giữa chừng (CONCURRENTLY lỗi) thì
-- \d order_items ghi INVALID: xoá rồi tạo lại.

-- ---------------------------------------------------------------------------
-- 3. LÙI
-- ---------------------------------------------------------------------------
-- DROP INDEX CONCURRENTLY IF EXISTS idx_order_items_sku_code_trgm;
