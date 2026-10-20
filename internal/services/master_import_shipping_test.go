package services

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"the-fulfillment/backend/internal/models"
)

// Khách (20/10/2026): file SKU / NVL phải mang luôn cân nặng + kích thước hộp (và,
// nếu cần, giá trị khai báo + mã HS) để khỏi import hai file. Mỗi dòng là một cặp
// (SKU, NVL) nên các giá trị của cùng SKU lặp lại — dòng sau không được lệch dòng trước.

func TestParseLegacyFile_ShippingColumns(t *testing.T) {
	csv := "SKU,Loại VL,D (mm),R (mm),Cân nặng (g),Dài hộp (cm),Rộng hộp (cm),Cao hộp (cm),Giá trị khai báo (USD),Mã HS\n" +
		"A,Mica,10,10,120,20,15,5,24,737373\n"
	rows, err := ParseLegacyFile("CSV", strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	r := rows[0]
	if r.Length != "10" || r.Width != "10" {
		t.Fatalf("D x R của tấm sản phẩm không được đụng: %+v", r)
	}
	if r.ShipWeight != "120" || r.ShipLength != "20" || r.ShipWidth != "15" || r.ShipHeight != "5" || r.ShipValue != "24" || r.ShipHS != "737373" {
		t.Fatalf("cột vận chuyển không được đọc: %+v", r)
	}

	// Cột chỉ tên "Dài"/"Rộng"/"Cao" trong file này là kích thước SẢN PHẨM, không phải hộp.
	rows, err = ParseLegacyFile("CSV", strings.NewReader("SKU,Loại VL,Dài,Rộng,Cao\nA,Mica,10,10,3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Length != "10" || rows[0].Width != "10" || rows[0].ShipLength != "" || rows[0].ShipHeight != "" {
		t.Fatalf("\"Dài/Rộng/Cao\" trơn không được hiểu là hộp: %+v", rows[0])
	}
}

func TestMasterImport_ShippingColumns(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	actor := Actor{ID: 1, Role: models.RoleOwner}
	if err := db.Create(&models.Material{Code: "MICA", Name: "Mica", LengthMM: ptrFloat(300), WidthMM: ptrFloat(300)}).Error; err != nil {
		t.Fatal(err)
	}
	// SKU có sẵn đã khai cân nặng 50 + giá trị 9: file chỉ nói kích thước hộp → cân nặng và giá trị phải còn.
	w, v := 50.0, 9.0
	if err := db.Create(&models.SKU{Code: "CU", Name: "CU", ShipWeightG: &w, DeclaredValue: &v, HSCode: "44209000"}).Error; err != nil {
		t.Fatal(err)
	}

	rows := []LegacyRow{
		{RowNumber: 1, SKU: "MOI", Material: "Mica", ShipWeight: "120", ShipLength: "20", ShipWidth: "15", ShipHeight: "5"},
		{RowNumber: 2, SKU: "CU", Material: "Mica", ShipLength: "30", ShipWidth: "20", ShipHeight: "4"},
		{RowNumber: 3, SKU: "KHONG-HOP", Material: "Mica"}, // không nói gì về vận chuyển → không đụng
		{RowNumber: 4, SKU: "SAI", Material: "Mica", ShipWeight: "nhiều"},
		{RowNumber: 5, SKU: "QUA-NANG", Material: "Mica", ShipWeight: "999999"}, // quá 30 kg
		{RowNumber: 6, SKU: "HS-SAI", Material: "Mica", ShipHS: "12"},
		{RowNumber: 7, SKU: "LECH", Material: "Mica", ShipWeight: "100"},
		{RowNumber: 8, SKU: "LECH", Material: "Mica", ShipWeight: "110"}, // hai dòng cùng SKU lệch nhau
		{RowNumber: 9, SKU: "AM", Material: "Mica", ShipWeight: "0"},     // 0 không phải cân nặng
	}
	pv, err := svc.Preview(actor, "TEST", "ship.csv", rows)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	codes := errorCodes(pv)
	for _, sku := range []string{"SAI", "QUA-NANG", "HS-SAI", "AM"} {
		if codes[sku] != errShipInvalid {
			t.Errorf("%s phải bị %s, got %q (%+v)", sku, errShipInvalid, codes[sku], pv.Errors)
		}
	}
	if codes["LECH"] != errShipConflict {
		t.Errorf("LECH phải bị %s, got %q", errShipConflict, codes["LECH"])
	}
	if pv.Summary.ShippingSKUs != 2 {
		t.Fatalf("shipping_skus = %d, want 2 (MOI, CU)", pv.Summary.ShippingSKUs)
	}

	res, err := svc.Commit(actor, pv.ImportJobID)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Applied == nil || res.Applied.ShippingSet != 2 {
		t.Fatalf("applied shipping_set: %+v", res.Applied)
	}
	var moi, cu, none models.SKU
	db.Where("code = ?", "MOI").First(&moi)
	db.Where("code = ?", "CU").First(&cu)
	db.Where("code = ?", "KHONG-HOP").First(&none)
	if moi.ShipWeightG == nil || *moi.ShipWeightG != 120 || *moi.ShipLengthCM != 20 || *moi.ShipWidthCM != 15 || *moi.ShipHeightCM != 5 {
		t.Fatalf("SKU mới phải mang cân nặng + hộp: %+v", moi)
	}
	if moi.DeclaredValue != nil || moi.HSCode != "" {
		t.Fatalf("giá trị / mã HS để trống thì SKU không tự khai (dùng mặc định bên THE): %+v", moi)
	}
	if cu.ShipLengthCM == nil || *cu.ShipLengthCM != 30 || *cu.ShipHeightCM != 4 {
		t.Fatalf("SKU có sẵn phải nhận kích thước hộp: %+v", cu)
	}
	if cu.ShipWeightG == nil || *cu.ShipWeightG != 50 || cu.DeclaredValue == nil || *cu.DeclaredValue != 9 || cu.HSCode != "44209000" {
		t.Fatalf("ô trống trong file không được xoá giá trị đã có: %+v", cu)
	}
	if none.ShipWeightG != nil || none.ShipLengthCM != nil {
		t.Fatalf("SKU file không nói gì về vận chuyển thì giữ nguyên: %+v", none)
	}
}

// File mẫu và file xuất có sẵn 6 cột vận chuyển; file xuất mang giá trị đã khai để sửa rồi import lại.
func TestMasterTemplateAndExport_CarryShippingColumns(t *testing.T) {
	want := masterShipHeaders
	h := masterTemplateHeaders
	if got := h[len(h)-len(want):]; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("template thiếu cột vận chuyển: %v", h)
	}
	for _, w := range want {
		if masterShipHeaderKey(w) == "" {
			t.Errorf("importer không nhận ra cột %q của chính file mẫu", w)
		}
	}
	for _, row := range masterTemplateSample {
		if len(row) != len(masterTemplateHeaders) {
			t.Fatalf("dòng mẫu %v có %d ô, template có %d cột", row, len(row), len(masterTemplateHeaders))
		}
	}
}

// File xuất mang cân nặng / hộp đã khai của từng SKU, đúng cột của file mẫu — sửa xong import lại là xong.
func TestMasterExport_CarriesDeclaredShipping(t *testing.T) {
	db := newMasterDB(t)
	svc := masterSvc(db)
	w, l, wd, h := 120.0, 20.0, 15.0, 5.0
	if err := db.Create(&models.SKU{Code: "XUAT", Name: "XUAT", ShipWeightG: &w, ShipLengthCM: &l, ShipWidthCM: &wd, ShipHeightCM: &h}).Error; err != nil {
		t.Fatal(err)
	}
	data, _, err := svc.MasterExportXLSX()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	grid, _ := f.GetRows(f.GetSheetList()[0])
	col := map[string]int{}
	for i, name := range grid[0] {
		col[name] = i
	}
	var row []string
	for _, r := range grid[1:] {
		if len(r) > 1 && r[1] == "XUAT" {
			row = r
		}
	}
	get := func(name string) string {
		if i, ok := col[name]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	if get("Cân nặng (g)") != "120" || get("Dài hộp (cm)") != "20" || get("Rộng hộp (cm)") != "15" || get("Cao hộp (cm)") != "5" {
		t.Fatalf("file xuất thiếu thông tin vận chuyển: %v", row)
	}
}
