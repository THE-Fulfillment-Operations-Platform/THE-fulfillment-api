package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"the-fulfillment/backend/internal/models"
)

// Mã batch ở màn Đơn hàng (Nghĩa 23/11/2026): cột BATCH in "#1909" — là
// "#" + batch_id (số nội bộ) vì /api/items không trả batch_code. Khách gõ "1909"
// vào ô Batch ID màn Batch thì ra #101909 (id 909): một batch KHÁC hẳn #102909
// (id 1909) thật sự chứa sản phẩm. Test dựng đúng cặp id/mã của prod rồi đi qua
// HTTP thật (router → handler → service → SQL → JSON) như web gọi.

type batchRefJSON struct {
	BatchID   uint   `json:"batch_id"`
	BatchCode string `json:"batch_code"`
	Batch     *struct {
		ID   uint   `json:"id"`
		Code string `json:"code"`
	} `json:"batch"`
}

type itemJSON struct {
	ID           uint           `json:"id"`
	InternalCode string         `json:"internal_code"`
	BatchItems   []batchRefJSON `json:"batch_items"`
}

func TestBatchCodeOnOrdersScreen_HTTP(t *testing.T) {
	h := newMultiHarness(t)
	db := h.db

	seller := &models.Seller{Code: "BC1", Name: "Seller BC"}
	if err := db.Create(seller).Error; err != nil {
		t.Fatalf("seller: %v", err)
	}
	wood := &models.Material{Code: "BW3", Name: "Basswood 3 ly"}
	mica := &models.Material{Code: "MC2", Name: "Mica 2 ly"}
	for _, m := range []*models.Material{wood, mica} {
		if err := db.Create(m).Error; err != nil {
			t.Fatalf("material: %v", err)
		}
	}
	mkItem := func(code string) *models.OrderItem {
		o := &models.Order{InternalCode: code, StoreOrderID: "SO-" + code, SellerID: seller.ID,
			ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
			ShippingName: "Jane", ShippingCountry: "US"}
		if err := db.Create(o).Error; err != nil {
			t.Fatalf("order: %v", err)
		}
		it := &models.OrderItem{OrderID: o.ID, InternalCode: code + "_1/1", SKUCode: "SKU-" + code,
			Quantity: 1, LineNo: 1, DesignStatus: models.DesignReady}
		if err := db.Create(it).Error; err != nil {
			t.Fatalf("item: %v", err)
		}
		return it
	}
	// id / mã như prod: code = "#" + (101000 + id).
	mkBatch := func(id uint, code string, mat *models.Material, items ...*models.OrderItem) {
		b := &models.Batch{Base: models.Base{ID: id}, Code: code, MaterialID: mat.ID, Status: models.StatusPending}
		if err := db.Create(b).Error; err != nil {
			t.Fatalf("batch %s: %v", code, err)
		}
		for _, it := range items {
			if err := db.Create(&models.BatchItem{BatchID: b.ID, OrderItemID: it.ID, MaterialID: mat.ID, Status: models.StatusPending}).Error; err != nil {
				t.Fatalf("batch item: %v", err)
			}
		}
	}
	inReal := mkItem("109703")  // nằm ở batch id 1909 = #102909
	inOther := mkItem("108000") // nằm ở batch id 909 = #101909 (batch "gõ 1909" từng ra)
	combo := mkItem("109702")   // combo 2 NVL: #102909 (gỗ) + #102902 (mica)
	_ = mkItem("109800")        // chưa vào batch nào
	mkBatch(1909, "#102909", wood, inReal, combo)
	mkBatch(909, "#101909", wood, inOther)
	mkBatch(1902, "#102902", mica, combo)

	items := func(path string) (map[string]itemJSON, int64) {
		t.Helper()
		r := h.list(path)
		out := map[string]itemJSON{}
		for _, raw := range r.Data {
			var it itemJSON
			if err := json.Unmarshal(raw, &it); err != nil {
				t.Fatalf("item json: %v", err)
			}
			out[it.InternalCode] = it
		}
		return out, r.Meta.Total
	}
	codesOf := func(it itemJSON) []string {
		out := []string{}
		for _, b := range it.BatchItems {
			out = append(out, b.BatchCode)
		}
		sort.Strings(out)
		return out
	}

	// ---- 1. /api/items trả batch_code = batches.code, giữ batch_id + batch cũ ----
	all, total := items("/api/items?page_size=50")
	if total != 4 {
		t.Fatalf("items: total=%d, want 4", total)
	}
	real := all["109703_1/1"]
	if len(real.BatchItems) != 1 {
		t.Fatalf("109703: %d batch items, want 1", len(real.BatchItems))
	}
	if b := real.BatchItems[0]; b.BatchCode != "#102909" || b.BatchID != 1909 || b.Batch == nil || b.Batch.Code != "#102909" {
		t.Errorf("109703 batch ref = %+v (batch %+v), want batch_code #102909, batch_id 1909, batch.code #102909", b, b.Batch)
	}
	if got := codesOf(all["109702_1/1"]); strings.Join(got, ",") != "#102902,#102909" {
		t.Errorf("combo 109702 batch codes = %v, want [#102902 #102909]", got)
	}
	if got := codesOf(all["108000_1/1"]); strings.Join(got, ",") != "#101909" {
		t.Errorf("108000 batch codes = %v, want [#101909]", got)
	}
	if n := len(all["109800_1/1"].BatchItems); n != 0 {
		t.Errorf("109800 chưa vào batch mà có %d batch item", n)
	}

	// ---- 2. ô Batch ID màn Đơn hàng: ?batch= khớp ĐÚNG mã, có/không # ----
	for _, q := range []string{"102909", "%23102909", "+%23102909+", "%23%23102909"} {
		got, total := items("/api/items?batch=" + q)
		if total != 2 || got["109703_1/1"].ID == 0 || got["109702_1/1"].ID == 0 {
			t.Errorf("?batch=%s: total=%d %v, want 109703 + 109702", q, total, batchItemKeys(got))
		}
	}
	// "1909" là id nội bộ, không phải mã: không được ra #101909 (chuỗi con) nữa.
	if got, total := items("/api/items?batch=1909"); total != 0 {
		t.Errorf("?batch=1909: total=%d %v, want 0 (không khớp chuỗi con)", total, batchItemKeys(got))
	}
	// Lọc theo id nội bộ vẫn chạy (tương thích, và cú gõ "id 1909" của web).
	if got, total := items("/api/items?batch_id=1909"); total != 2 || got["109703_1/1"].ID == 0 {
		t.Errorf("?batch_id=1909: total=%d %v, want 109703 + 109702", total, batchItemKeys(got))
	}

	// ---- 3. ô Batch ID màn Batch: ?batch= khớp đúng mã ----
	batchCodes := func(path string) []string {
		t.Helper()
		r := h.list(path)
		out := []string{}
		for _, raw := range r.Data {
			var b struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(raw, &b)
			out = append(out, b.Code)
		}
		sort.Strings(out)
		return out
	}
	for _, q := range []string{"102909", "%23102909"} {
		if got := batchCodes("/api/batches?batch=" + q); strings.Join(got, ",") != "#102909" {
			t.Errorf("/api/batches?batch=%s = %v, want [#102909]", q, got)
		}
	}
	if got := batchCodes("/api/batches?batch=1909"); len(got) != 0 {
		t.Errorf("/api/batches?batch=1909 = %v, want [] (trước đây ra #101909)", got)
	}
	// Ô tìm chung của bảng sản xuất vẫn tìm theo một phần mã.
	if got := batchCodes("/api/batches?q=2909"); strings.Join(got, ",") != "#102909" {
		t.Errorf("/api/batches?q=2909 = %v, want [#102909]", got)
	}

	// ---- 4. chi tiết sản phẩm và chi tiết đơn cũng có batch_code ----
	w := h.get(fmt.Sprintf("/api/items/%d", inReal.ID), models.RoleOps)
	if w.Code != http.StatusOK {
		t.Fatalf("GET item: %d %s", w.Code, w.Body.String())
	}
	var one struct {
		Data itemJSON `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil {
		t.Fatalf("item json: %v", err)
	}
	if got := codesOf(one.Data); strings.Join(got, ",") != "#102909" {
		t.Errorf("GET /api/items/:id batch codes = %v, want [#102909]", got)
	}
	w = h.get(fmt.Sprintf("/api/orders/%d", combo.OrderID), models.RoleOps)
	if w.Code != http.StatusOK {
		t.Fatalf("GET order: %d %s", w.Code, w.Body.String())
	}
	var ord struct {
		Data struct {
			Items []itemJSON `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ord); err != nil {
		t.Fatalf("order json: %v", err)
	}
	if len(ord.Data.Items) != 1 || strings.Join(codesOf(ord.Data.Items[0]), ",") != "#102902,#102909" {
		t.Errorf("GET /api/orders/:id items = %+v, want 1 item with [#102902 #102909]", ord.Data.Items)
	}

	// ---- 5. "Xuất theo mẫu import" theo cùng bộ lọc mã batch ----
	w = h.get("/api/orders/export/import-template.xlsx?batch=102909", models.RoleTechnician)
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	f, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("export xlsx: %v", err)
	}
	rows, err := f.GetRows(f.GetSheetList()[0])
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	var stores []string
	for _, row := range rows[1:] {
		for _, cell := range row {
			if strings.HasPrefix(cell, "SO-") {
				stores = append(stores, cell)
			}
		}
	}
	sort.Strings(stores)
	if strings.Join(stores, ",") != "SO-109702,SO-109703" {
		t.Errorf("xuất theo mẫu import ?batch=102909: %v, want [SO-109702 SO-109703]", stores)
	}
}

func batchItemKeys(m map[string]itemJSON) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
