package services

import (
	"net/http"
	"strings"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Link design (mặt trước) + mockup bắt buộc ở mọi cửa tạo đơn (anh Kiên 13/11/2026):
// bỏ màn "Chờ thiết kế" rồi thì sản phẩm sinh ra READY và đi thẳng vào gom batch,
// nên đơn thiếu link phải bị chặn ngay ở cửa vào. Các test dưới đây giữ luật đó ở
// từng cửa: import file, commit job cũ, tạo đơn trực tiếp, sửa đơn, duyệt đơn.
// (Open API: xem open_api_test.go.)

func wantStatus(t *testing.T, err error, status int, contains string) {
	t.Helper()
	ae, ok := apperr.As(err)
	if !ok {
		t.Fatalf("want apperr %d, got %v", status, err)
	}
	if ae.Status != status {
		t.Fatalf("want status %d, got %d (%s)", status, ae.Status, ae.Message)
	}
	if contains != "" && !strings.Contains(ae.Message, contains) {
		t.Fatalf("message %q should contain %q", ae.Message, contains)
	}
}

func TestItemArtProblem(t *testing.T) {
	const mock = "https://m.example.com/1.png"
	cases := []struct {
		name, front, back, mockup, want string
	}{
		{"thiếu design", "", "", mock, codeDesignMissing},
		{"design chỉ có khoảng trắng", "   ", "", mock, codeDesignMissing},
		{"thiếu mockup", "https://d.example.com/1.png", "", "", codeMockupMissing},
		{"thiếu cả hai", "", "", "  ", codeDesignMockupMissing},
		{"design là mã tham chiếu", "design-ref-042", "", mock, ""},
		{"đủ cả hai link", "https://d.example.com/1.png", "", mock, ""},
		{"đủ, có cả mặt sau", "https://d.example.com/f.png", "https://d.example.com/b.png", mock, ""},
		{"mockup không phải link", "design-a", "", "mockup-a", "MOCKUP_INVALID"},
		{"design là link hỏng", "https://", "", mock, "DESIGN_INVALID"},
		{"mặt sau trùng mặt trước", "https://d.example.com/f.png", "https://d.example.com/f.png", mock, "DESIGN_SIDE_DUP"},
	}
	for _, c := range cases {
		p := itemArtProblem(c.front, c.back, c.mockup)
		got := ""
		if p != nil {
			got = p.Code
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// File import (cũng là đường của màn Tạo đơn lẻ và seller tự upload): dòng thiếu
// link là lỗi đỏ, không vào job; dòng đủ link — kể cả design là mã tham chiếu hay
// nằm ở cột "Design" cũ — vẫn qua và commit được.
func TestPreview_DesignAndMockupRequired(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	actor := Actor{ID: 1}

	noDesign := row("NO-DESIGN", "A")
	noDesign.FrontDesign = ""
	noMockup := row("NO-MOCKUP", "B")
	noMockup.Mockup = ""
	noBoth := row("NO-BOTH", "C")
	noBoth.FrontDesign, noBoth.Mockup = "", ""
	refCode := row("REF-CODE", "D")
	refCode.FrontDesign = "design-ref-042"
	full := row("FULL", "E")
	legacyCol := row("LEGACY-COL", "F")
	legacyCol.FrontDesign, legacyCol.Design = "", "https://d.example.com/legacy.png"

	prev, err := svc.Preview(actor, 1, "XLSX", "f.xlsx",
		[]ImportRow{noDesign, noMockup, noBoth, refCode, full, legacyCol}, HeaderReport{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.ValidRows != 3 || prev.ErrorRows != 3 || prev.OrderCount != 3 {
		t.Fatalf("valid=%d errors=%d orders=%d, want 3/3/3 (errors %+v)", prev.ValidRows, prev.ErrorRows, prev.OrderCount, prev.Errors)
	}
	want := map[int]struct{ code, msg string }{
		1: {codeDesignMissing, "Thiếu link design"},
		2: {codeMockupMissing, "Thiếu link mockup"},
		3: {codeDesignMockupMissing, "Thiếu link design và link mockup"},
	}
	for _, e := range prev.Errors {
		w, ok := want[e.RowNumber]
		if !ok {
			t.Errorf("unexpected error on row %d: %+v", e.RowNumber, e)
			continue
		}
		if e.ErrorCode != w.code || e.Message != w.msg || !strings.Contains(e.Suggestion, "Seller gửi lại") {
			t.Errorf("row %d: got %s %q / %q, want %s %q + gợi ý seller gửi lại", e.RowNumber, e.ErrorCode, e.Message, e.Suggestion, w.code, w.msg)
		}
	}

	job, err := svc.Commit(actor, prev.ImportJobID)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if job.CreatedCount != 3 {
		t.Fatalf("created %d orders, want 3", job.CreatedCount)
	}
	byRef := map[string]models.OrderItem{}
	for _, o := range loadOrders(t, db, 1) {
		if len(o.Items) != 1 {
			t.Fatalf("order %s has %d items", o.StoreOrderID, len(o.Items))
		}
		byRef[o.StoreOrderID] = o.Items[0]
	}
	if _, ok := byRef["NO-DESIGN"]; ok {
		t.Fatalf("a row without design became an order")
	}
	if byRef["REF-CODE"].DesignURL != "design-ref-042" || byRef["LEGACY-COL"].DesignURL != "https://d.example.com/legacy.png" {
		t.Errorf("design kept wrong: ref=%q legacy=%q", byRef["REF-CODE"].DesignURL, byRef["LEGACY-COL"].DesignURL)
	}
	var notes int64
	db.Model(&models.Note{}).Where("reason_code = ?", "ART_MISSING").Count(&notes)
	if notes != 0 {
		t.Errorf("%d ART_MISSING notes, want 0 — nothing without a mockup gets in any more", notes)
	}
}

// Job preview từ trước khi có luật mới có thể còn giữ dòng thiếu link: commit
// phải từ chối, không tạo đơn nào, job vẫn ở PREVIEW để preview lại.
func TestCommit_RefusesStoredRowsWithoutLinks(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)

	stale := row("OLD-1", "A")
	stale.FrontDesign = ""
	raw, err := models.ToJSONB([]ImportRow{row("OK-1", "B"), stale})
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	sellerID := uint(1)
	job := &models.ImportJob{SellerID: &sellerID, Filename: "old.xlsx", Source: "XLSX",
		Status: models.ImportPreview, TotalRows: 2, ValidRows: 2, RawRows: raw}
	if err := db.Create(job).Error; err != nil {
		t.Fatalf("seed job: %v", err)
	}

	_, err = svc.Commit(Actor{ID: 1}, job.ID)
	wantStatus(t, err, http.StatusUnprocessableEntity, "thiếu link design hoặc mockup")

	if n := len(loadOrders(t, db, 1)); n != 0 {
		t.Fatalf("refused commit still created %d orders", n)
	}
	var after models.ImportJob
	db.First(&after, job.ID)
	if after.Status != models.ImportPreview {
		t.Fatalf("job status = %s, want PREVIEW", after.Status)
	}
}

func directOrderSvc(db *gorm.DB) *OrderService {
	repo := repositories.New(db)
	return &OrderService{repo: repo, audit: &AuditService{repo: repo}}
}

// POST /orders (tạo đơn trực tiếp) theo đúng luật của import.
func TestCreateOrderDirect_RequiresDesignAndMockup(t *testing.T) {
	db := newImportDB(t)
	svc := directOrderSvc(db)
	actor := Actor{ID: 1, Role: models.RoleOwner}
	in := func(it DirectItemInput) DirectOrderInput {
		it.SKUCode, it.Quantity = "TESTSKU", 1
		return DirectOrderInput{SellerID: 1, StoreOrderID: "DIRECT-1", ShippingName: "Jane",
			ShippingAddress1: "1 Main St", ShippingCountry: "US", Items: []DirectItemInput{it}}
	}

	_, err := svc.CreateOrderDirect(actor, in(DirectItemInput{MockupURL: "https://m.example.com/1.png"}))
	wantStatus(t, err, http.StatusUnprocessableEntity, "Thiếu link design")
	_, err = svc.CreateOrderDirect(actor, in(DirectItemInput{DesignURL: "https://d.example.com/1.png"}))
	wantStatus(t, err, http.StatusUnprocessableEntity, "Thiếu link mockup")
	if n := len(loadOrders(t, db, 1)); n != 0 {
		t.Fatalf("refused create still wrote %d orders", n)
	}

	o, err := svc.CreateOrderDirect(actor, in(DirectItemInput{DesignURL: "design-ref-7", MockupURL: "https://m.example.com/7.png"}))
	if err != nil {
		t.Fatalf("create with both links: %v", err)
	}
	if len(o.Items) != 1 || o.Items[0].DesignURL != "design-ref-7" || o.Items[0].MockupURL != "https://m.example.com/7.png" {
		t.Fatalf("items = %+v", o.Items)
	}
}

// Sửa đơn không được xoá trắng link design / mockup (đổi sang link khác thì được);
// sản phẩm cũ vốn đã trống mà form gửi lại "" thì vẫn sửa được các ô khác.
func TestEditOrder_CannotWipeDesignOrMockup(t *testing.T) {
	db := newImportDB(t)
	imp := importSvc(db)
	svc := directOrderSvc(db)
	actor := Actor{ID: 1, Role: models.RoleOwner}

	prev, err := imp.Preview(actor, 1, "XLSX", "f.xlsx", []ImportRow{row("EDIT-1", "A")}, HeaderReport{})
	if err != nil || prev.ValidRows != 1 {
		t.Fatalf("preview: %v (%+v)", err, prev)
	}
	if _, err := imp.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	order := loadOrders(t, db, 1)[0]
	itemID := order.Items[0].ID
	empty, spaces := "", "   "
	var assetsBefore int64
	db.Model(&models.ItemAsset{}).Count(&assetsBefore)

	// Màn Sửa đơn (PUT /orders/:id).
	_, err = svc.UpdateOrder(actor, order.ID, UpdateOrderInput{Items: []EditOrderItemInput{{ID: itemID, DesignURL: &empty}}})
	wantStatus(t, err, http.StatusUnprocessableEntity, "không được xoá trắng link design")
	_, err = svc.UpdateOrder(actor, order.ID, UpdateOrderInput{Items: []EditOrderItemInput{{ID: itemID, MockupURL: &spaces}}})
	wantStatus(t, err, http.StatusUnprocessableEntity, "không được xoá trắng link mockup")
	// Màn Chờ duyệt → "Chuẩn hoá dữ liệu sản xuất" (PATCH /items/:id/design).
	_, err = svc.UpdateItemDesign(actor, itemID, UpdateDesignInput{MockupURL: &empty})
	wantStatus(t, err, http.StatusUnprocessableEntity, "không được xoá trắng link mockup")
	_, err = svc.UpdateItemDesign(actor, itemID, UpdateDesignInput{DesignURL: &empty})
	wantStatus(t, err, http.StatusUnprocessableEntity, "không được xoá trắng link design")

	var assetsAfter int64
	db.Model(&models.ItemAsset{}).Count(&assetsAfter)
	if assetsAfter != assetsBefore {
		t.Fatalf("a refused edit still recorded assets: %d → %d", assetsBefore, assetsAfter)
	}

	// Replacing a link is fine.
	newDesign := "https://d.example.com/new.png"
	if _, err := svc.UpdateOrder(actor, order.ID, UpdateOrderInput{Items: []EditOrderItemInput{{ID: itemID, DesignURL: &newDesign}}}); err != nil {
		t.Fatalf("replace design: %v", err)
	}
	// An item older than the rule (mockup already empty) stays editable: sending
	// the empty value back is no change.
	if err := db.Model(&models.OrderItem{}).Where("id = ?", itemID).Update("mockup_url", "").Error; err != nil {
		t.Fatalf("seed legacy item: %v", err)
	}
	engrave := "Hello"
	if _, err := svc.UpdateOrder(actor, order.ID, UpdateOrderInput{Items: []EditOrderItemInput{
		{ID: itemID, MockupURL: &empty, DesignURL: &newDesign, EngraveText: &engrave}}}); err != nil {
		t.Fatalf("edit other fields of a legacy item: %v", err)
	}
	var it models.OrderItem
	db.First(&it, itemID)
	if it.DesignURL != newDesign || it.EngraveText != engrave {
		t.Fatalf("item after edits = design %q engrave %q", it.DesignURL, it.EngraveText)
	}
}

// Duyệt đơn là cổng cuối trước gom batch: sản phẩm thiếu link (đơn cũ trước luật,
// đơn bảo hành chép từ đơn cũ) là lỗi CHẶN, không còn là cảnh báo.
func TestReview_MissingLinksBlockApproval(t *testing.T) {
	skuID := uint(1)
	order := &models.Order{ShippingName: "x", ShippingAddress1: "y", ShippingCountry: "US", Items: []models.OrderItem{
		{Base: models.Base{ID: 1}, InternalCode: "100001_1/3", SKUID: &skuID, Quantity: 1, DesignURL: "design-a"},
		{Base: models.Base{ID: 2}, InternalCode: "100001_2/3", SKUID: &skuID, Quantity: 1, MockupURL: "https://m.example.com/2.png"},
		{Base: models.Base{ID: 3}, InternalCode: "100001_3/3", SKUID: &skuID, Quantity: 1, DesignURL: "design-c", MockupURL: "https://m.example.com/3.png"},
	}}
	got := map[uint]string{}
	for _, iss := range reviewIssuesWith(order, map[uint]int64{1: 1}) {
		if iss.Severity != "BLOCKER" {
			t.Errorf("issue %s on item %d is %s, want BLOCKER", iss.Code, iss.ItemID, iss.Severity)
		}
		got[iss.ItemID] += iss.Code
	}
	if got[1] != "MOCKUP_MISSING" || got[2] != "DESIGN_MISSING" || got[3] != "" || len(got) != 2 {
		t.Fatalf("issues = %v", got)
	}

	// Server side too: Approve refuses an order whose item lost its mockup.
	db := newImportDB(t)
	imp := importSvc(db)
	actor := Actor{ID: 1, Role: models.RoleOwner}
	prev, err := imp.Preview(actor, 1, "XLSX", "f.xlsx", []ImportRow{row("REV-1", "A")}, HeaderReport{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := imp.Commit(actor, prev.ImportJobID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	o := loadOrders(t, db, 1)[0]
	db.Model(&models.OrderItem{}).Where("order_id = ?", o.ID).Update("mockup_url", "")
	_, err = reviewSvc(db).Approve(actor, o.ID, "")
	wantStatus(t, err, http.StatusUnprocessableEntity, "Thiếu link mockup")
}
