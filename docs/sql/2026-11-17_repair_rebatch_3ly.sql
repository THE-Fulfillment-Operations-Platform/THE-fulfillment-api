-- =============================================================================
-- SỬA DỮ LIỆU 17/11/2026 — gỡ 81 phần Basswood 3 ly gom nhầm (#102595, #102597)
-- Postgres (Supabase). Nghĩa tự chạy. API không đọc hay chạy file này.
-- =============================================================================
--
-- CHUYỆN GÌ XẢY RA
--   12:34–12:38  anh Kiên đổi NVL 10 SKU WD-PBH-5L-* / WD-PBH-6L-* từ 5 ly sang 3 ly.
--   Hàng chờ Tạo batch (code cũ) coi mọi sản phẩm của các SKU đó là "chưa có batch
--   3 ly", kể cả hàng đã làm xong trên 5 ly và đã bàn giao.
--   12:40:01  BATCH_CREATE #102595 (50 phần) · 12:43:26 BATCH_AUTO_CREATE #102597
--   (31 phần) · rồi bấm nộp file, "Đã in", "Đã cắt" trong vài giây — không in/cắt thật.
--   Hệ quả: 81 sản phẩm có 2 phần còn sống (5 ly thật + 3 ly ảo). Trạng thái sản
--   phẩm = phần kém tiến nhất (status.go deriveItemStatusesFromRanks) nên đang là
--   CUT, kể cả 65 sản phẩm đơn đã HANDED_OFF vốn đã QC_PASSED. Link in/cắt trên sản
--   phẩm bị đóng dấu đè bằng link của batch 3 ly (SetBatchLinkPair).
--
-- SCRIPT LÀM GÌ (y như BatchService.Delete, chỉ bỏ qua chốt "batch đã vào sản xuất"
-- vì #102595/#102597 không sản xuất thật)
--   1. Xoá HẲN đúng 81 phần trong #102595/#102597 mà sản phẩm của nó còn phần sống ở
--      NVL khác. App cũng xoá hẳn (HardDeleteBatchItems): idx_item_material_attempt
--      không có điều kiện deleted_at, xoá mềm sẽ làm hỏng lần gom 3 ly sau này của
--      chính sản phẩm đó (NextAttempts bỏ qua dòng xoá mềm → trùng attempt 1).
--      Không mất gì: 81 dòng được chép nguyên vào ffm_repair.* trước khi xoá.
--   2. Xoá mềm link in/cắt và header của 2 batch (SoftDeleteLinks / SoftDeleteBatches).
--   3. Link in/cắt trên sản phẩm: chỗ nào còn đúng link batch 3 ly thì trả về link
--      của batch còn lại (5 ly) — đúng thứ SetBatchLinkPair của batch 5 ly đã đóng
--      dấu; batch 5 ly không có link thì để trống như ClearProductionFileForBatch.
--   4. Trạng thái sản phẩm tính lại đúng recomputeOrderItemStatuses: phần còn sống
--      (scrapped_at IS NULL, deleted_at IS NULL) kém tiến nhất theo hạng
--      PENDING < PRINTED < CUT < QC_PASSED; không còn phần nào → PENDING; dòng đã
--      duyệt huỷ (cancellation_status = APPROVED) giữ nguyên. Trạng thái nội bộ chỉ
--      có 4 giá trị đó; đóng gói / bàn giao nằm ở orders.seller_status, không đụng.
--      Ghi status_histories cho dòng nào đổi (như app, note "derived from batch parts").
--   5. Ghi audit_logs BATCH_DELETE cho từng batch, metadata như batchDeleteAudit.
--
-- ĐIỀU KIỆN: API phải đang chạy bản có commit ed9f46f (hàng chờ + QC luật mới).
--   Chạy trên code cũ thì ngay sau khi gỡ, 81 sản phẩm hiện lại ở thẻ Basswood 3 ly
--   (code cũ chỉ hỏi "đã có batch 3 ly chưa") và hàng CUT trên 5 ly bị QC đòi phần
--   3 ly. Kiểm: GitHub Actions deploy xong, trang Tạo batch tải lại thấy thẻ 3 ly chỉ
--   còn hàng đơn mới. Báo anh Kiên đừng bấm gom / in / cắt batch 3 ly trong lúc chạy.
--
-- CÁCH CHẠY — DBeaver (kết nối prod, Supabase cổng 5432 "session", không dùng pooler 6543)
--   a. Thanh công cụ: chuyển giao dịch sang "Manual Commit" (tắt Auto-Commit).
--   b. Bôi đen PHẦN 1, Execute Script (Alt+X). Chỉ đọc. Xem các tab kết quả, lưu
--      tab 1B và 1C ra file (chuột phải → Export data → CSV) làm bằng chứng.
--      Đúng như mong đợi: 1A có #102595 = 50, #102597 = 31 ở cả 3 cột phan_song /
--      dinh / dinh_5ly, các batch 3 ly khác (#102599…#102603) dinh_5ly = 0; 1B đủ 81
--      dòng, cột phan_con_lai toàn BASSWOOD-5-LY; 1D cộng lại 81, lech_lich_su = 0.
--   c. Bôi đen PHẦN 2 + PHẦN 3, Alt+X. Có lỗi "Dừng: …" → bấm Rollback, gửi trợ lý.
--   d. Xem PHẦN 3: 3A rỗng; 3B đúng số; 3C hàng chờ 3 ly không còn hàng cũ; 3E = 0.
--   e. Ổn → bấm Commit (hoặc chạy COMMIT;). Có gì lạ → Rollback, không mất gì.
--   f. Sau Commit chạy lại PHẦN 3 (giờ là dữ liệu thật đã lưu).
--
-- CÁCH CHẠY — psql
--   psql "postgresql://postgres.<ref>:<mật khẩu>@<host>:5432/postgres?sslmode=require"
--   \set ON_ERROR_STOP on
--   \i 2026-11-17_repair_rebatch_3ly.sql
--   File dừng ở cuối PHẦN 3 với giao dịch còn mở. Gõ COMMIT; hoặc ROLLBACK;.
--   (Chạy kiểu psql -f thì psql thoát và giao dịch tự ROLLBACK — an toàn nhưng vô ích.)
--
-- CHẠY HAI LẦN: không được. PHẦN 2 tạo bảng ffm_repair.r20261117_* bằng CREATE TABLE
-- (không IF NOT EXISTS): lần hai báo "already exists" và dừng trước khi ghi gì.
-- =============================================================================


-- =============================================================================
-- PHẦN 1. XEM TRƯỚC — chỉ đọc
-- =============================================================================

-- 1A. Mọi batch Basswood 3 ly tạo hôm nay: số phần còn sống, số phần "dính" (sản phẩm
--     còn phần sống ở NVL khác) và "dính 5 ly" (phần kia là Basswood 5 ly). Mong:
--     #102595 50/50/50, #102597 31/31/31; các batch khác (đơn mới 13:10: #102599…
--     #102603) dinh_5ly = 0 → không đụng. Batch khác có dinh > 0 mà dinh_5ly = 0 là
--     hàng combo thật (3 ly + NVL khác), cũng không đụng: script chỉ chọn theo mã batch.
SELECT b.code, b.status, b.deleted_at IS NOT NULL AS da_xoa,
       b.created_at AT TIME ZONE 'Asia/Ho_Chi_Minh' AS tao_luc,
       count(bi.id) FILTER (WHERE bi.scrapped_at IS NULL) AS phan_song,
       count(bi.id) FILTER (WHERE bi.scrapped_at IS NOT NULL) AS phan_ghi_bo,
       count(bi.id) FILTER (WHERE bi.scrapped_at IS NULL AND EXISTS (
           SELECT 1 FROM batch_items o JOIN batches ob ON ob.id = o.batch_id AND ob.deleted_at IS NULL
           WHERE o.order_item_id = bi.order_item_id AND o.material_id <> bi.material_id
             AND o.deleted_at IS NULL AND o.scrapped_at IS NULL)) AS dinh,
       count(bi.id) FILTER (WHERE bi.scrapped_at IS NULL AND EXISTS (
           SELECT 1 FROM batch_items o JOIN batches ob ON ob.id = o.batch_id AND ob.deleted_at IS NULL
           JOIN materials om ON om.id = o.material_id AND om.code = 'BASSWOOD-5-LY'
           WHERE o.order_item_id = bi.order_item_id
             AND o.deleted_at IS NULL AND o.scrapped_at IS NULL)) AS dinh_5ly
FROM batches b
JOIN materials m ON m.id = b.material_id AND m.code = 'BASSWOOD-3-LY'
LEFT JOIN batch_items bi ON bi.batch_id = b.id AND bi.deleted_at IS NULL
WHERE b.created_at >= TIMESTAMPTZ '2026-11-17 00:00:00+07'
GROUP BY b.id ORDER BY b.code;

-- 1B. ĐÚNG các dòng batch_items sẽ bị xoá (mong 81 dòng), kèm phần còn lại của sản phẩm.
WITH tb AS (SELECT id, code FROM batches WHERE code IN ('#102595', '#102597') AND deleted_at IS NULL)
SELECT bi.id AS batch_item_id, tb.code AS batch_3ly, oi.internal_code AS item, oi.sku_code,
       bi.status AS phan_3ly, bi.attempt,
       string_agg(ob.code || ' ' || om.code || ' ' || o.status, ', ' ORDER BY o.id) AS phan_con_lai
FROM batch_items bi
JOIN tb ON tb.id = bi.batch_id
JOIN order_items oi ON oi.id = bi.order_item_id
JOIN batch_items o ON o.order_item_id = bi.order_item_id AND o.material_id <> bi.material_id
                  AND o.deleted_at IS NULL AND o.scrapped_at IS NULL
JOIN batches ob ON ob.id = o.batch_id AND ob.deleted_at IS NULL
JOIN materials om ON om.id = o.material_id
WHERE bi.deleted_at IS NULL AND bi.scrapped_at IS NULL
GROUP BY bi.id, tb.code, oi.internal_code, oi.sku_code
ORDER BY tb.code, oi.internal_code;

-- 1C. Từng sản phẩm: trạng thái hiện tại → sẽ về (luật của app), đối chiếu trạng thái
--     ngay trước khi bị gom 3 ly (status_histories), link in/cắt hiện tại → sẽ về.
WITH tb AS (SELECT id, code, created_at FROM batches WHERE code IN ('#102595', '#102597') AND deleted_at IS NULL),
tgt AS (
  SELECT bi.* FROM batch_items bi JOIN tb ON tb.id = bi.batch_id
  WHERE bi.deleted_at IS NULL AND bi.scrapped_at IS NULL
    AND EXISTS (SELECT 1 FROM batch_items o JOIN batches ob ON ob.id = o.batch_id AND ob.deleted_at IS NULL
                WHERE o.order_item_id = bi.order_item_id AND o.material_id <> bi.material_id
                  AND o.deleted_at IS NULL AND o.scrapped_at IS NULL)
)
SELECT oi.internal_code AS item, ord.internal_code AS don, ord.seller_status, oi.cancellation_status,
       oi.internal_status AS hien_tai,
       CASE WHEN oi.cancellation_status = 'APPROVED' THEN oi.internal_status
            ELSE COALESCE((SELECT r.status FROM batch_items r
                           WHERE r.order_item_id = oi.id AND r.deleted_at IS NULL AND r.scrapped_at IS NULL
                             AND r.id NOT IN (SELECT id FROM tgt)
                           ORDER BY CASE r.status WHEN 'PENDING' THEN 0 WHEN 'PRINTED' THEN 1
                                                  WHEN 'CUT' THEN 2 WHEN 'QC_PASSED' THEN 3 ELSE 0 END, r.id
                           LIMIT 1), 'PENDING') END AS se_ve,
       (SELECT sh.from_status FROM status_histories sh
         WHERE sh.entity_type = 'ORDER_ITEM' AND sh.entity_id = oi.id AND sh.deleted_at IS NULL
           AND sh.created_at >= tb.created_at - interval '5 seconds'
         ORDER BY sh.created_at, sh.id LIMIT 1) AS truoc_khi_gom_3ly,
       oi.print_file_url AS link_in_hien_tai,
       CASE WHEN oi.print_file_url = (SELECT url FROM batch_links WHERE batch_id = t.batch_id AND kind = 'PRINT' AND deleted_at IS NULL)
            THEN COALESCE((SELECT bl.url FROM batch_items r
                           JOIN batches rb ON rb.id = r.batch_id AND rb.deleted_at IS NULL
                           JOIN batch_links bl ON bl.batch_id = r.batch_id AND bl.kind = 'PRINT' AND bl.deleted_at IS NULL
                           WHERE r.order_item_id = oi.id AND r.deleted_at IS NULL AND r.scrapped_at IS NULL
                             AND r.id NOT IN (SELECT id FROM tgt)
                           ORDER BY r.id DESC LIMIT 1), '')
            ELSE oi.print_file_url END AS link_in_se_ve,
       (SELECT count(*) FROM qc_records q WHERE q.batch_item_id = t.id AND q.deleted_at IS NULL) AS qc_tren_phan_3ly
FROM tgt t
JOIN tb ON tb.id = t.batch_id
JOIN order_items oi ON oi.id = t.order_item_id
JOIN orders ord ON ord.id = oi.order_id
ORDER BY ord.seller_status, oi.internal_code;

-- 1D. Tóm tắt 1C: theo trạng thái đơn × trạng thái sẽ về. lech_lich_su = số sản phẩm
--     mà "sẽ về" khác trạng thái trước khi bị gom 3 ly (mong 0; khác 0 thì xem 1C).
WITH tb AS (SELECT id, created_at FROM batches WHERE code IN ('#102595', '#102597') AND deleted_at IS NULL),
tgt AS (
  SELECT bi.* FROM batch_items bi JOIN tb ON tb.id = bi.batch_id
  WHERE bi.deleted_at IS NULL AND bi.scrapped_at IS NULL
    AND EXISTS (SELECT 1 FROM batch_items o JOIN batches ob ON ob.id = o.batch_id AND ob.deleted_at IS NULL
                WHERE o.order_item_id = bi.order_item_id AND o.material_id <> bi.material_id
                  AND o.deleted_at IS NULL AND o.scrapped_at IS NULL)
),
x AS (
  SELECT ord.seller_status, oi.internal_status AS hien_tai,
         CASE WHEN oi.cancellation_status = 'APPROVED' THEN oi.internal_status
              ELSE COALESCE((SELECT r.status FROM batch_items r
                             WHERE r.order_item_id = oi.id AND r.deleted_at IS NULL AND r.scrapped_at IS NULL
                               AND r.id NOT IN (SELECT id FROM tgt)
                             ORDER BY CASE r.status WHEN 'PENDING' THEN 0 WHEN 'PRINTED' THEN 1
                                                    WHEN 'CUT' THEN 2 WHEN 'QC_PASSED' THEN 3 ELSE 0 END, r.id
                             LIMIT 1), 'PENDING') END AS se_ve,
         (SELECT sh.from_status FROM status_histories sh
           WHERE sh.entity_type = 'ORDER_ITEM' AND sh.entity_id = oi.id AND sh.deleted_at IS NULL
             AND sh.created_at >= tb.created_at - interval '5 seconds'
           ORDER BY sh.created_at, sh.id LIMIT 1) AS truoc
  FROM tgt t JOIN tb ON tb.id = t.batch_id
  JOIN order_items oi ON oi.id = t.order_item_id JOIN orders ord ON ord.id = oi.order_id
)
SELECT seller_status, hien_tai, se_ve, count(*) AS so_san_pham,
       count(*) FILTER (WHERE truoc IS DISTINCT FROM se_ve) AS lech_lich_su
FROM x GROUP BY 1, 2, 3 ORDER BY 1, 3;


-- =============================================================================
-- PHẦN 2. GHI — một giao dịch. KHÔNG có COMMIT trong file: tự COMMIT ở PHẦN 4.
-- =============================================================================
BEGIN;
SET LOCAL lock_timeout = '5s';        -- bảng đang bị app khoá thì dừng, không treo xưởng
SET LOCAL statement_timeout = '120s';

-- Sao lưu (bảng thật, ở schema riêng ffm_repair: không lộ qua API dữ liệu của
-- Supabase như schema public). ROLLBACK thì các bảng này cũng biến mất.
CREATE SCHEMA IF NOT EXISTS ffm_repair;

CREATE TABLE ffm_repair.r20261117_batch_items AS
SELECT bi.*
FROM batch_items bi
JOIN batches b ON b.id = bi.batch_id AND b.code IN ('#102595', '#102597') AND b.deleted_at IS NULL
WHERE bi.deleted_at IS NULL AND bi.scrapped_at IS NULL
  AND EXISTS (SELECT 1 FROM batch_items o JOIN batches ob ON ob.id = o.batch_id AND ob.deleted_at IS NULL
              WHERE o.order_item_id = bi.order_item_id AND o.material_id <> bi.material_id
                AND o.deleted_at IS NULL AND o.scrapped_at IS NULL);

CREATE TABLE ffm_repair.r20261117_batches AS
SELECT * FROM batches WHERE code IN ('#102595', '#102597') AND deleted_at IS NULL;

CREATE TABLE ffm_repair.r20261117_batch_links AS
SELECT * FROM batch_links
WHERE batch_id IN (SELECT id FROM ffm_repair.r20261117_batches) AND deleted_at IS NULL;

CREATE TABLE ffm_repair.r20261117_order_items AS
SELECT id, internal_code, internal_status, print_file_url, cut_file_url, cancellation_status, updated_at,
       NULL::varchar(20) AS new_status
FROM order_items
WHERE id IN (SELECT order_item_id FROM ffm_repair.r20261117_batch_items) AND deleted_at IS NULL;

DO $$
DECLARE
  c_parts   CONSTANT int  := 81;  -- số phần phải gỡ (50 ở #102595 + 31 ở #102597)
  c_batches CONSTANT int  := 2;
  c_note    CONSTANT text := 'sửa dữ liệu 17/11: gỡ phần Basswood 3 ly gom nhầm (#102595, #102597)';
  v_parts int; v_items int; v_batches int; v_all int; v_rows int; v_qc int; v_wrong_mat int; v_5ly int;
BEGIN
  -- ---- Chốt chặn trước khi ghi ----------------------------------------------
  SELECT count(*), count(DISTINCT order_item_id) INTO v_parts, v_items FROM ffm_repair.r20261117_batch_items;
  -- phần "kia" của mọi sản phẩm phải là Basswood 5 ly (đúng ca 5 ly → 3 ly, không phải combo thật)
  SELECT count(*) INTO v_5ly FROM ffm_repair.r20261117_batch_items t
   WHERE EXISTS (SELECT 1 FROM batch_items o JOIN batches ob ON ob.id = o.batch_id AND ob.deleted_at IS NULL
                 JOIN materials om ON om.id = o.material_id AND om.code = 'BASSWOOD-5-LY'
                 WHERE o.order_item_id = t.order_item_id AND o.deleted_at IS NULL AND o.scrapped_at IS NULL);
  SELECT count(*) INTO v_batches FROM ffm_repair.r20261117_batches;
  -- mọi dòng (kể cả đã ghi bỏ ở QC) của 2 batch: phải đúng bằng số sẽ gỡ, để 2 batch rỗng hẳn
  SELECT count(*) INTO v_all FROM batch_items
   WHERE batch_id IN (SELECT id FROM ffm_repair.r20261117_batches) AND deleted_at IS NULL;
  SELECT count(*) INTO v_wrong_mat FROM ffm_repair.r20261117_batches b
    JOIN materials m ON m.id = b.material_id WHERE m.code <> 'BASSWOOD-3-LY';
  IF v_parts <> c_parts OR v_items <> c_parts OR v_batches <> c_batches OR v_all <> c_parts
     OR v_wrong_mat <> 0 OR v_5ly <> c_parts THEN
    RAISE EXCEPTION 'Dừng: số liệu lệch — phần sẽ gỡ %, sản phẩm %, batch %, tổng dòng trong 2 batch %, batch không phải 3 ly %, có phần 5 ly % (mong %, %, %, %, 0, %). Không có gì bị ghi. Chạy ROLLBACK rồi gửi kết quả PHẦN 1.',
      v_parts, v_items, v_batches, v_all, v_wrong_mat, v_5ly, c_parts, c_parts, c_batches, c_parts, c_parts;
  END IF;
  SELECT count(*) INTO v_rows FROM ffm_repair.r20261117_order_items;
  IF v_rows <> c_parts THEN
    RAISE EXCEPTION 'Dừng: sao lưu được % sản phẩm, mong %. Chạy ROLLBACK.', v_rows, c_parts;
  END IF;
  -- QC đã chạy trên phần 3 ly (sau 12:43, trước lúc sửa): phiếu QC giữ nguyên — không
  -- màn nào nối qc_records.batch_item_id sang batch_items, và phiếu song song trên phần
  -- 5 ly vẫn còn. Chỉ báo để biết.
  SELECT count(*) INTO v_qc FROM qc_records
   WHERE deleted_at IS NULL AND batch_item_id IN (SELECT id FROM ffm_repair.r20261117_batch_items);
  IF v_qc > 0 THEN
    RAISE NOTICE 'Lưu ý: % phiếu QC trỏ vào phần 3 ly sắp gỡ — giữ nguyên, không ảnh hưởng.', v_qc;
  END IF;

  -- ---- 1. Link in/cắt trên sản phẩm -------------------------------------------
  -- Chỉ chỗ còn đúng link của batch 3 ly (như ClearProductionFileForBatch). Trả về link
  -- của batch còn lại của sản phẩm (5 ly) nếu có, không thì để trống.
  UPDATE order_items oi
     SET print_file_url = COALESCE((
           SELECT bl.url FROM batch_items r
           JOIN batches rb ON rb.id = r.batch_id AND rb.deleted_at IS NULL
           JOIN batch_links bl ON bl.batch_id = r.batch_id AND bl.kind = 'PRINT' AND bl.deleted_at IS NULL
           WHERE r.order_item_id = oi.id AND r.deleted_at IS NULL AND r.scrapped_at IS NULL
             AND r.id NOT IN (SELECT id FROM ffm_repair.r20261117_batch_items)
           ORDER BY r.id DESC LIMIT 1), ''),
         updated_at = now()
    FROM ffm_repair.r20261117_batch_items t
    JOIN ffm_repair.r20261117_batch_links l ON l.batch_id = t.batch_id AND l.kind = 'PRINT'
   WHERE oi.id = t.order_item_id AND oi.print_file_url = l.url;
  UPDATE order_items oi
     SET cut_file_url = COALESCE((
           SELECT bl.url FROM batch_items r
           JOIN batches rb ON rb.id = r.batch_id AND rb.deleted_at IS NULL
           JOIN batch_links bl ON bl.batch_id = r.batch_id AND bl.kind = 'CUT' AND bl.deleted_at IS NULL
           WHERE r.order_item_id = oi.id AND r.deleted_at IS NULL AND r.scrapped_at IS NULL
             AND r.id NOT IN (SELECT id FROM ffm_repair.r20261117_batch_items)
           ORDER BY r.id DESC LIMIT 1), ''),
         updated_at = now()
    FROM ffm_repair.r20261117_batch_items t
    JOIN ffm_repair.r20261117_batch_links l ON l.batch_id = t.batch_id AND l.kind = 'CUT'
   WHERE oi.id = t.order_item_id AND oi.cut_file_url = l.url;

  -- ---- 2. Xoá hẳn 81 phần (như HardDeleteBatchItems) ---------------------------
  DELETE FROM batch_items WHERE id IN (SELECT id FROM ffm_repair.r20261117_batch_items);
  GET DIAGNOSTICS v_rows = ROW_COUNT;
  IF v_rows <> c_parts THEN
    RAISE EXCEPTION 'Dừng: xoá được % phần, mong %. Chạy ROLLBACK.', v_rows, c_parts;
  END IF;

  -- ---- 3. Xoá mềm link + header batch (như SoftDeleteLinks / SoftDeleteBatches) -
  UPDATE batch_links SET deleted_at = now()
   WHERE batch_id IN (SELECT id FROM ffm_repair.r20261117_batches) AND deleted_at IS NULL;
  UPDATE batches b SET deleted_at = now()
   WHERE b.id IN (SELECT id FROM ffm_repair.r20261117_batches) AND b.deleted_at IS NULL
     AND NOT EXISTS (SELECT 1 FROM batch_items x WHERE x.batch_id = b.id AND x.deleted_at IS NULL);
  GET DIAGNOSTICS v_rows = ROW_COUNT;
  IF v_rows <> c_batches THEN
    RAISE EXCEPTION 'Dừng: xoá được % batch, mong % (batch còn phần khác?). Chạy ROLLBACK.', v_rows, c_batches;
  END IF;

  -- ---- 4. Trạng thái sản phẩm = recomputeOrderItemStatuses ----------------------
  UPDATE ffm_repair.r20261117_order_items k
     SET new_status = CASE WHEN k.cancellation_status = 'APPROVED' THEN k.internal_status
                           ELSE COALESCE((SELECT r.status FROM batch_items r
                                          WHERE r.order_item_id = k.id AND r.deleted_at IS NULL AND r.scrapped_at IS NULL
                                          ORDER BY CASE r.status WHEN 'PENDING' THEN 0 WHEN 'PRINTED' THEN 1
                                                                 WHEN 'CUT' THEN 2 WHEN 'QC_PASSED' THEN 3 ELSE 0 END, r.id
                                          LIMIT 1), 'PENDING') END;
  UPDATE order_items oi SET internal_status = k.new_status, updated_at = now()
    FROM ffm_repair.r20261117_order_items k
   WHERE oi.id = k.id AND oi.internal_status IS DISTINCT FROM k.new_status;
  INSERT INTO status_histories (created_at, updated_at, entity_type, entity_id, from_status, to_status, changed_by_id, note)
  SELECT now(), now(), 'ORDER_ITEM', k.id, k.internal_status, k.new_status, NULL,
         'derived from batch parts — ' || c_note
    FROM ffm_repair.r20261117_order_items k
   WHERE k.new_status IS DISTINCT FROM k.internal_status;

  -- ---- 5. Nhật ký BATCH_DELETE (như batchDeleteAudit) ----------------------------
  INSERT INTO audit_logs (created_at, updated_at, actor_id, actor_email, action, entity_type, entity_id, summary, metadata, ip)
  SELECT now(), now(), NULL, 'sql-sua-du-lieu', 'BATCH_DELETE', 'batch', b.id,
         'Deleted batch ' || b.code || ' (' || c_note || ')',
         jsonb_build_object(
           'released_item_ids', (SELECT COALESCE(jsonb_agg(t.order_item_id ORDER BY t.order_item_id), '[]'::jsonb)
                                   FROM ffm_repair.r20261117_batch_items t WHERE t.batch_id = b.id),
           'parts', (SELECT COALESCE(jsonb_agg(jsonb_build_object(
                              'batch_item_id', t.id, 'batch_id', t.batch_id, 'order_item_id', t.order_item_id,
                              'item_code', COALESCE(k.internal_code, ''), 'material_id', t.material_id,
                              'attempt', t.attempt, 'status', t.status, 'scrapped', t.scrapped_at IS NOT NULL,
                              'cancellation_status', COALESCE(k.cancellation_status, '')) ORDER BY t.id), '[]'::jsonb)
                       FROM ffm_repair.r20261117_batch_items t
                       LEFT JOIN ffm_repair.r20261117_order_items k ON k.id = t.order_item_id
                      WHERE t.batch_id = b.id),
           'only_cancelled_items', false,
           'produced_cancelled_parts', 0,
           'status_kept_item_ids', (SELECT COALESCE(jsonb_agg(k.id ORDER BY k.id), '[]'::jsonb)
                                      FROM ffm_repair.r20261117_batch_items t
                                      JOIN ffm_repair.r20261117_order_items k ON k.id = t.order_item_id
                                     WHERE t.batch_id = b.id AND k.cancellation_status = 'APPROVED'),
           'links', (SELECT COALESCE(jsonb_agg(jsonb_build_object('kind', l.kind, 'url', l.url) ORDER BY l.kind), '[]'::jsonb)
                       FROM ffm_repair.r20261117_batch_links l WHERE l.batch_id = b.id),
           'repair', c_note,
           'backup_schema', 'ffm_repair.r20261117_*'),
         ''
    FROM ffm_repair.r20261117_batches b;

  -- ---- Kiểm trong giao dịch -----------------------------------------------------
  SELECT count(*) INTO v_rows FROM batch_items bi
   WHERE bi.order_item_id IN (SELECT id FROM ffm_repair.r20261117_order_items)
     AND bi.deleted_at IS NULL AND bi.scrapped_at IS NULL
     AND bi.batch_id IN (SELECT id FROM ffm_repair.r20261117_batches);
  IF v_rows <> 0 THEN
    RAISE EXCEPTION 'Dừng: còn % phần trong 2 batch sau khi gỡ. Chạy ROLLBACK.', v_rows;
  END IF;
  RAISE NOTICE 'Xong phần ghi (chưa COMMIT): gỡ % phần, xoá mềm % batch. Xem PHẦN 3 rồi COMMIT hoặc ROLLBACK.', c_parts, c_batches;
END
$$;


-- =============================================================================
-- PHẦN 3. KIỂM — chạy ngay sau PHẦN 2 (trong giao dịch), và chạy lại sau COMMIT
-- =============================================================================

-- 3A. Sản phẩm còn phần sống ở CẢ Basswood 5 ly lẫn 3 ly. Mong: 0 dòng.
WITH bw AS (SELECT id FROM materials WHERE code IN ('BASSWOOD-5-LY', 'BASSWOOD-3-LY') AND deleted_at IS NULL)
SELECT bi.order_item_id, count(DISTINCT bi.material_id) AS so_nvl
FROM batch_items bi
WHERE bi.deleted_at IS NULL AND bi.scrapped_at IS NULL AND bi.material_id IN (SELECT id FROM bw)
GROUP BY bi.order_item_id
HAVING count(DISTINCT bi.material_id) >= 2;

-- 3B. 81 sản phẩm theo trạng thái đơn × trạng thái sản phẩm bây giờ.
SELECT o.seller_status, oi.internal_status, count(*) AS so_san_pham
FROM ffm_repair.r20261117_order_items k
JOIN order_items oi ON oi.id = k.id
JOIN orders o ON o.id = oi.order_id
GROUP BY 1, 2 ORDER BY 1, 2;

-- 3C. Hàng chờ Basswood 3 ly theo LUẬT MỚI (designReadyUnbatchedSubquery sau commit
--     ed9f46f). Mong: không còn sản phẩm nào trong 81 (cột trong_81 = 0); số còn lại
--     là đơn mới thật sự chưa gom.
WITH pool AS (
  SELECT oi.id
  FROM order_items oi
  JOIN orders o ON o.id = oi.order_id AND o.deleted_at IS NULL
  JOIN sku_materials sm ON sm.sku_id = oi.sku_id AND sm.deleted_at IS NULL
  JOIN materials m ON m.id = sm.material_id AND m.deleted_at IS NULL AND m.code = 'BASSWOOD-3-LY'
  WHERE oi.deleted_at IS NULL
    AND o.review_status = 'APPROVED'
    AND o.seller_status = 'PRODUCTION'
    AND oi.cancellation_status NOT IN ('SELLER_CANCELLED', 'APPROVED')
    AND NOT EXISTS (SELECT 1 FROM batch_items bi
                    WHERE bi.order_item_id = oi.id AND bi.material_id = sm.material_id
                      AND bi.deleted_at IS NULL AND bi.scrapped_at IS NULL)
    AND NOT ((SELECT count(DISTINCT bp.material_id) FROM batch_items bp
              WHERE bp.order_item_id = oi.id AND bp.deleted_at IS NULL AND bp.scrapped_at IS NULL)
             >= (SELECT count(*) FROM sku_materials sx WHERE sx.sku_id = oi.sku_id AND sx.deleted_at IS NULL))
)
SELECT count(*) AS hang_cho_3ly,
       count(*) FILTER (WHERE id IN (SELECT id FROM ffm_repair.r20261117_order_items)) AS trong_81
FROM pool;

-- 3D. Batch 3 ly hôm nay sau khi sửa: #102595/#102597 đã xoá, các batch khác giữ nguyên.
SELECT b.code, b.status, b.deleted_at IS NOT NULL AS da_xoa,
       (SELECT count(*) FROM batch_items bi WHERE bi.batch_id = b.id AND bi.deleted_at IS NULL) AS so_phan
FROM batches b JOIN materials m ON m.id = b.material_id AND m.code = 'BASSWOOD-3-LY'
WHERE b.created_at >= TIMESTAMPTZ '2026-11-17 00:00:00+07'
ORDER BY b.code;

-- 3E. Sản phẩm còn mang link in/cắt của batch 3 ly đã xoá. Mong: 0.
SELECT count(*) AS con_link_3ly
FROM order_items oi
JOIN ffm_repair.r20261117_batch_links l
  ON (l.kind = 'PRINT' AND oi.print_file_url = l.url) OR (l.kind = 'CUT' AND oi.cut_file_url = l.url)
WHERE oi.id IN (SELECT id FROM ffm_repair.r20261117_order_items);


-- =============================================================================
-- PHẦN 4. CHỐT — gõ tay MỘT trong hai dòng sau (DBeaver: nút Commit / Rollback)
-- =============================================================================
-- COMMIT;     -- PHẦN 3 đúng như mong đợi
-- ROLLBACK;   -- có gì lạ, hoặc PHẦN 2 báo "Dừng: …" (huỷ toàn bộ, kể cả bảng sao lưu)


-- =============================================================================
-- PHẦN 5. HOÀN TÁC SAU KHI ĐÃ COMMIT (chỉ khi thật cần) — đưa dữ liệu về đúng
-- trạng thái ngay trước PHẦN 2, từ ffm_repair.r20261117_*. Nhật ký audit_logs và
-- status_histories là append-only: không xoá, chỉ ghi thêm dòng hoàn tác.
-- =============================================================================
-- BEGIN;
-- INSERT INTO batch_items (id, created_at, updated_at, deleted_at, batch_id, order_item_id, material_id,
--                          status, attempt, scrapped_at, scrap_reason, scrapped_by_id)
-- SELECT id, created_at, updated_at, deleted_at, batch_id, order_item_id, material_id,
--        status, attempt, scrapped_at, scrap_reason, scrapped_by_id
--   FROM ffm_repair.r20261117_batch_items;                                   -- mong 81
-- UPDATE batches b SET deleted_at = NULL FROM ffm_repair.r20261117_batches k WHERE b.id = k.id;          -- mong 2
-- UPDATE batch_links l SET deleted_at = NULL FROM ffm_repair.r20261117_batch_links k WHERE l.id = k.id;  -- mong 4
-- INSERT INTO status_histories (created_at, updated_at, entity_type, entity_id, from_status, to_status, note)
-- SELECT now(), now(), 'ORDER_ITEM', k.id, oi.internal_status, k.internal_status, 'hoàn tác sửa dữ liệu 17/11 (#102595, #102597)'
--   FROM ffm_repair.r20261117_order_items k JOIN order_items oi ON oi.id = k.id
--  WHERE oi.internal_status IS DISTINCT FROM k.internal_status;
-- UPDATE order_items oi
--    SET internal_status = k.internal_status, print_file_url = k.print_file_url,
--        cut_file_url = k.cut_file_url, updated_at = now()
--   FROM ffm_repair.r20261117_order_items k WHERE oi.id = k.id;              -- mong 81
-- INSERT INTO audit_logs (created_at, updated_at, actor_email, action, entity_type, entity_id, summary, ip)
-- SELECT now(), now(), 'sql-sua-du-lieu', 'BATCH_RESTORE', 'batch', id, 'Hoàn tác sửa dữ liệu 17/11: khôi phục batch ' || code, ''
--   FROM ffm_repair.r20261117_batches;
-- COMMIT;


-- =============================================================================
-- PHẦN 6. DỌN SAO LƯU — sau 1–2 tuần, khi chắc không cần hoàn tác
-- =============================================================================
-- DROP SCHEMA ffm_repair CASCADE;
