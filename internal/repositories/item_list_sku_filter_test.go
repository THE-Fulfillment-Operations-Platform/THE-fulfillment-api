package repositories

import (
	"sort"
	"testing"

	"the-fulfillment/backend/internal/models"
)

// Anh Kiên 17/11: gõ "FSH" ở ô SKU màn Sản phẩm ra "Không có item nào khớp bộ
// lọc" vì API so khớp nguyên mã. Ô SKU giờ là "có chứa", không phân biệt hoa
// thường, và % / _ là chữ thường chứ không phải ký tự đại diện.
func TestItemList_SKUFilterIsCaseInsensitiveContains(t *testing.T) {
	db := newQueueTestDB(t)
	codes := map[string]uint{}
	for i, sku := range []string{"FSH-01", "XFSH-2", "WOOD-01", "A_B", "AXB", "50%OFF", "500FF"} {
		it := seedQueueItem(t, db, "10000"+string(rune('0'+i))+"_1/1", models.DesignReady)
		if err := db.Model(&models.OrderItem{}).Where("id = ?", it.ID).Update("sku_code", sku).Error; err != nil {
			t.Fatalf("đặt SKU %s: %v", sku, err)
		}
		codes[sku] = it.ID
	}
	repo := &OrderItemRepository{db: db}

	skusFor := func(term string) []string {
		t.Helper()
		rows, total, err := repo.List(ItemFilter{Page: Page{Page: 1, PageSize: 50}, SKUCode: term})
		if err != nil {
			t.Fatalf("List(sku=%q): %v", term, err)
		}
		if int(total) != len(rows) {
			t.Fatalf("sku=%q: total=%d nhưng trang có %d dòng", term, total, len(rows))
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.SKUCode)
		}
		sort.Strings(out)
		return out
	}
	same := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	cases := []struct {
		term string
		want []string
	}{
		{"FSH", []string{"FSH-01", "XFSH-2"}},   // đúng ảnh anh Kiên gửi
		{"fs", []string{"FSH-01", "XFSH-2"}},    // 2 ký tự, chữ thường
		{" fsh ", []string{"FSH-01", "XFSH-2"}}, // dán kèm khoảng trắng
		{"FSH-01", []string{"FSH-01"}},          // nguyên mã vẫn ra đúng nó
		{"a_b", []string{"A_B"}},                // _ là chữ, không khớp AXB
		{"0%", []string{"50%OFF"}},              // % là chữ, không khớp 500FF
		{"ZZZ", []string{}},                     // không có thì rỗng, không phải cả bảng
	}
	for _, c := range cases {
		if got := skusFor(c.term); !same(got, c.want) {
			t.Errorf("sku=%q: got %v, want %v", c.term, got, c.want)
		}
	}

	// Ô trống = không lọc.
	if got := skusFor(""); len(got) != len(codes) {
		t.Errorf("sku rỗng phải ra cả %d dòng, got %d", len(codes), len(got))
	}
}

func TestLikeContainsPattern(t *testing.T) {
	for in, want := range map[string]string{
		"FSH":  "%fsh%",
		"a_b":  `%a\_b%`,
		"50%":  `%50\%%`,
		`a\b`:  `%a\\b%`,
		"Gỗ-3": "%gỗ-3%",
	} {
		if got := likeContainsPattern(in); got != want {
			t.Errorf("likeContainsPattern(%q) = %q, want %q", in, got, want)
		}
	}
}
