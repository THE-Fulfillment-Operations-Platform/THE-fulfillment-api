package services

import (
	"strings"
	"testing"

	"the-fulfillment/backend/internal/models"
)

func TestParseParentSKUFile(t *testing.T) {
	rows, err := ParseParentSKUFile("CSV", strings.NewReader("SKU cha,Tên sản phẩm,Mô tả\nHOP-NHUA,Hộp nhựa,nhiều cỡ\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r := rows[0]; r.SKU != "HOP-NHUA" || r.ProductName != "Hộp nhựa" || r.Description != "nhiều cỡ" {
		t.Fatalf("row = %+v", r)
	}
	// A plain "SKU" column is accepted as the parent code too.
	if rows, err = ParseParentSKUFile("CSV", strings.NewReader("SKU,Tên sản phẩm\nHOP-NHUA,Hộp nhựa\n")); err != nil || rows[0].SKU != "HOP-NHUA" {
		t.Fatalf("plain SKU column: %+v (%v)", rows, err)
	}
	// A step-2 children file dropped here would give every parent a child's name.
	_, err = ParseParentSKUFile("CSV", strings.NewReader("SKU cha,SKU,Tên sản phẩm\nHOP-NHUA,HOP-BE,Hộp nhựa bé\n"))
	if err == nil || !strings.Contains(err.Error(), "Bước 2") {
		t.Fatalf("children file in step 1: err = %v, want a pointer to step 2", err)
	}
}

func TestParentImport_CreatesRenamesAndRefusesChildren(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	actor := Actor{ID: 1, Role: models.RoleOwner}
	hop, _, khac, _ := seedParentCatalog(t, db) // KHAC already holds KHAC-1
	if err := db.Create(&models.SKU{Code: "HOP-BE", Name: "HOP-BE", ParentID: &hop.ID}).Error; err != nil {
		t.Fatalf("seed child: %v", err)
	}

	rows := []ParentSKURow{
		{RowNumber: 1, SKU: "HOP-NHUA", ProductName: "Hộp nhựa in UV"},
		{RowNumber: 2, SKU: "ornament mica", ProductName: "Acrylic Ornament"},
		{RowNumber: 3, SKU: "ORNAMENT-MICA", ProductName: "Acrylic Ornament"}, // repeat → folded
		{RowNumber: 4, SKU: "KHAC-1", ProductName: "Không làm cha được"},
		{RowNumber: 5, SKU: "XUNG", ProductName: "A"},
		{RowNumber: 6, SKU: "XUNG", ProductName: "B"},
		{RowNumber: 7, ProductName: "Thiếu mã"},
		{RowNumber: 8, SKU: "KHAC"},
	}
	pv, err := svc.PreviewParents("cha.xlsx", rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if s := pv.Summary; s.New != 1 || s.Updates != 1 || s.Unchanged != 1 || s.DuplicateRows != 2 || s.ErrorRows != 3 {
		t.Fatalf("summary = %+v", s)
	}
	codes := map[string]string{}
	for _, e := range pv.Errors {
		codes[e.SKU] = e.ErrorCode
		if e.ErrorCode == errParentIsChild && !strings.Contains(e.Message, "KHAC") {
			t.Fatalf("child-as-parent error should name its parent: %q", e.Message)
		}
	}
	if codes["KHAC-1"] != errParentIsChild || codes["XUNG"] != errParentRowConflict || codes[""] != errSKUMissing {
		t.Fatalf("errors = %+v", pv.Errors)
	}
	for _, it := range pv.Items {
		if it.Code == "HOP-NHUA" && (it.Action != importActionUpdate || it.ChildCount != 1) {
			t.Fatalf("HOP-NHUA item = %+v, want UPDATE holding 1 child", it)
		}
		if it.Code == "KHAC" && it.ChildCount != 1 {
			t.Fatalf("KHAC item = %+v, want 1 child", it)
		}
	}

	res, err := svc.CommitParents(actor, rows)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Applied.Created != 1 || res.Applied.Updated != 1 {
		t.Fatalf("applied = %+v, want 1 created + 1 renamed", res.Applied)
	}
	if got := loadSKU(t, db, hop.ID); got.ProductName != "Hộp nhựa in UV" || got.ParentID != nil {
		t.Fatalf("HOP-NHUA after commit: %+v", got)
	}
	var orn models.SKU
	if err := db.Where("code = ?", "ORNAMENT-MICA").First(&orn).Error; err != nil {
		t.Fatalf("new parent missing: %v", err)
	}
	if orn.ProductName != "Acrylic Ornament" || orn.ParentID != nil || !orn.IsActive {
		t.Fatalf("new parent = %+v", orn)
	}
	if got := loadSKU(t, db, khac.ID); got.ProductName != "" {
		t.Fatalf("a blank Tên sản phẩm must not write anything, got %q", got.ProductName)
	}

	again, err := svc.CommitParents(actor, rows)
	if err != nil {
		t.Fatalf("re-commit: %v", err)
	}
	if again.Applied.Created != 0 || again.Applied.Updated != 0 {
		t.Fatalf("re-commit applied %+v, want nothing", again.Applied)
	}
}
