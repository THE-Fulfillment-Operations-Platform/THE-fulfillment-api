package services

import (
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// The "Seller ID" column names a seller by CODE. Excel drops leading zeros from a
// number typed into a non-text cell, so "6" must find seller "006" — but never
// the seller whose database id happens to be 6, and never by display name.
func TestResolveSellerRef(t *testing.T) {
	sellers := []repositories.SellerIdentity{
		{ID: 1, Code: "005", Name: "I success"},
		{ID: 2, Code: "006", Name: "Six"},
		{ID: 3, Code: "NGHIASELLER", Name: "nghiaseller"},
		{ID: 4, Code: "THUY-LINK", Name: "Thuy Link"},
	}
	cases := []struct {
		ref    string
		wantID uint
		code   string
	}{
		{"006", 2, ""},
		{"6", 2, ""}, // Excel ate the zeros
		{" 6 ", 2, ""},
		{"0006", 2, ""},
		{"nghiaseller", 3, ""},
		{"Thuy Link", 4, ""}, // normalises to the code THUY-LINK
		{"", 0, sellerRefMissing},
		{"   ", 0, sellerRefMissing},
		{"999", 0, sellerRefUnknown},
		{"4", 0, sellerRefUnknown},         // database id of THUY-LINK: not a code
		{"I success", 0, sellerRefUnknown}, // display name: not a code
	}
	for _, c := range cases {
		got, code := resolveSellerRef(c.ref, sellers)
		if code != c.code || got.ID != c.wantID {
			t.Errorf("resolveSellerRef(%q) = (id %d, %q), want (id %d, %q)", c.ref, got.ID, code, c.wantID, c.code)
		}
	}

	// Two codes that are the same number: the exact spelling still resolves, the
	// zero-less one is refused instead of guessed.
	withTwin := append(sellers, repositories.SellerIdentity{ID: 5, Code: "06"})
	if got, code := resolveSellerRef("06", withTwin); code != "" || got.ID != 5 {
		t.Errorf("exact code must win over a leading-zero match, got (%d, %q)", got.ID, code)
	}
	if _, code := resolveSellerRef("6", withTwin); code != sellerRefAmbiguous {
		t.Errorf(`"6" with both "006" and "06" must be ambiguous, got %q`, code)
	}
}

func seedSeller(t *testing.T, db *gorm.DB, code string) uint {
	t.Helper()
	s := &models.Seller{Code: code, Name: "Seller " + code}
	if err := db.Create(s).Error; err != nil {
		t.Fatalf("seed seller %s: %v", code, err)
	}
	return s.ID
}

// One file, several sellers: every row lands on the seller its "Seller ID"
// names, each seller gets its own job, row numbers stay the file's, and rows that
// name no seller are reported instead of silently going somewhere.
func TestPreviewBySellerColumn_SplitsRowsPerSeller(t *testing.T) {
	db := newImportDB(t) // fixture seller id=1 "S1"
	svc := importSvc(db)
	actor := Actor{ID: 1}
	s005 := seedSeller(t, db, "005")
	s006 := seedSeller(t, db, "006")

	badSKU := templateRow("C-6", "2026-08-20", "006")
	badSKU.SKU = "NO-SUCH-SKU"
	rows := []ImportRow{
		templateRow("A-1", "2026-08-20", "005"), // row 1 → 005
		templateRow("B-1", "2026-08-20", "6"),   // row 2 → 006 (Excel dropped the zeros)
		templateRow("A-2", "2026-08-20", "005"), // row 3 → 005
		templateRow("X-1", "2026-08-20", ""),    // row 4 → no seller
		templateRow("X-2", "2026-08-20", "ZZZ"), // row 5 → unknown seller
		badSKU,                                  // row 6 → 006, but fails validation
	}
	res, err := svc.PreviewBySellerColumn(actor, "XLSX", "mixed.xlsx", rows, fullHeader())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	if res.TotalRows != 6 || res.ValidRows != 3 || res.ErrorRows != 3 || res.UnassignedRows != 2 || res.OrderCount != 3 {
		t.Fatalf("counters = total %d valid %d errors %d unassigned %d orders %d, want 6/3/3/2/3",
			res.TotalRows, res.ValidRows, res.ErrorRows, res.UnassignedRows, res.OrderCount)
	}
	if len(res.Sellers) != 2 || res.Sellers[0].SellerCode != "005" || res.Sellers[1].SellerCode != "006" {
		t.Fatalf("sellers = %+v, want 005 then 006", res.Sellers)
	}
	if got := res.Sellers[0]; got.SellerID != s005 || got.TotalRows != 2 || got.ValidRows != 2 {
		t.Errorf("005 share = %+v, want 2 valid rows", got)
	}
	if got := res.Sellers[1]; got.SellerID != s006 || got.TotalRows != 2 || got.ValidRows != 1 || got.ErrorRows != 1 {
		t.Errorf("006 share = %+v, want 2 rows, 1 valid, 1 error", got)
	}

	// Errors in file order, with the file's row numbers and the seller they hit.
	want := []struct {
		row    int
		code   string
		seller string
	}{{4, sellerRefMissing, ""}, {5, sellerRefUnknown, ""}, {6, "SKU_UNMAPPED", "006"}}
	if len(res.Errors) != len(want) {
		t.Fatalf("errors = %+v, want %d", res.Errors, len(want))
	}
	for i, w := range want {
		e := res.Errors[i]
		if e.RowNumber != w.row || e.ErrorCode != w.code || e.SellerCode != w.seller {
			t.Errorf("error %d = row %d %s seller %q, want row %d %s seller %q",
				i, e.RowNumber, e.ErrorCode, e.SellerCode, w.row, w.code, w.seller)
		}
	}

	// Committing each seller's job puts the orders on that seller and no other.
	for _, sh := range res.Sellers {
		if _, err := svc.Commit(actor, sh.ImportJobID); err != nil {
			t.Fatalf("commit %s: %v", sh.SellerCode, err)
		}
	}
	for storeOrder, sellerID := range map[string]uint{"A-1": s005, "A-2": s005, "B-1": s006} {
		var o models.Order
		if err := db.Where("store_order_id = ?", storeOrder).First(&o).Error; err != nil {
			t.Fatalf("order %s not created: %v", storeOrder, err)
		}
		if o.SellerID != sellerID {
			t.Errorf("order %s on seller %d, want %d", storeOrder, o.SellerID, sellerID)
		}
	}
	var stray int64
	db.Model(&models.Order{}).Where("store_order_id IN ?", []string{"X-1", "X-2", "C-6"}).Count(&stray)
	if stray != 0 {
		t.Errorf("%d rows without a valid seller/SKU became orders", stray)
	}
}

// A file with no "Seller ID" column cannot be split — one clear whole-file error,
// not "no Seller ID" on every row.
func TestPreviewBySellerColumn_RequiresSellerColumn(t *testing.T) {
	db := newImportDB(t)
	svc := importSvc(db)
	hdr := HeaderReport{Present: []string{fStoreOrd, fSKU, fQuantity, fShipName, fShipAddr1, fShipCntry, fOrderDate}}

	_, err := svc.PreviewBySellerColumn(Actor{ID: 1}, "XLSX", "f.xlsx",
		[]ImportRow{templateRow("A-1", "2026-08-20", "")}, hdr)
	if ae, ok := apperr.As(err); !ok || ae.Code != "BAD_REQUEST" {
		t.Fatalf("want a BAD_REQUEST for a file without Seller ID, got %v", err)
	}
}
