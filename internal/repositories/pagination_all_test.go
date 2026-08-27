package repositories

import (
	"fmt"
	"testing"
)

// TestNormalize_All locks in the "Tất cả" contract: a negative page size is a
// deliberate request for every row, so Normalize must NOT clamp it to the 200
// default cap, and Offset must stay 0 instead of going negative.
func TestNormalize_All(t *testing.T) {
	p := Page{Page: 7, PageSize: PageSizeAll}.Normalize()
	if !p.All() {
		t.Fatalf("expected All(), got %+v", p)
	}
	if p.PageSize != PageSizeAll {
		t.Fatalf("page size clamped: got %d, want %d", p.PageSize, PageSizeAll)
	}
	if p.Page != 1 {
		t.Fatalf("page should pin to 1, got %d", p.Page)
	}
	if got := p.Offset(); got != 0 {
		t.Fatalf("offset: got %d, want 0", got)
	}

	if got := (Page{}).Normalize(); got.PageSize != 20 || got.Page != 1 {
		t.Fatalf("default page: got %+v, want page 1 size 20", got)
	}
	if got := (Page{Page: 2, PageSize: 5000}).Normalize(); got.PageSize != 200 {
		t.Fatalf("positive size must still clamp at 200, got %d", got.PageSize)
	}
}

// TestListAll_ReturnsEveryRow proves the -1 page size actually cancels the LIMIT
// at the GORM layer — the whole reason no repository needs a special case.
func TestListAll_ReturnsEveryRow(t *testing.T) {
	db := newTestDB(t)
	repo := New(db)

	const n = 250 // deliberately above the 200 cap
	for i := 0; i < n; i++ {
		if err := db.Exec("INSERT INTO materials (code, name) VALUES (?, ?)",
			fmt.Sprintf("M%03d", i), fmt.Sprintf("Vật liệu %d", i)).Error; err != nil {
			t.Fatalf("seed material %d: %v", i, err)
		}
	}

	rows, total, err := repo.Material.List(Page{PageSize: PageSizeAll}.Normalize())
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if total != n {
		t.Fatalf("total: got %d, want %d", total, n)
	}
	if len(rows) != n {
		t.Fatalf("rows returned: got %d, want %d (LIMIT was not cancelled)", len(rows), n)
	}

	paged, _, err := repo.Material.List(Page{Page: 1, PageSize: 20}.Normalize())
	if err != nil {
		t.Fatalf("list paged: %v", err)
	}
	if len(paged) != 20 {
		t.Fatalf("paged rows: got %d, want 20", len(paged))
	}
}
