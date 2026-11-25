package models

import (
	"strings"
	"testing"
)

// #7783: the factory's real materials (FFM prod, 25/11/2026) fall in the kinds
// Nghĩa named — thiệp, mica, gỗ — and nothing else is swept in.
func TestTHERules_DefaultKindsOfRealMaterials(t *testing.T) {
	r := DefaultTHEServiceRules()
	name := func(i int) string {
		if i < 0 {
			return ""
		}
		return r.Groups[i].Name
	}
	for _, c := range []struct{ code, name, want string }{
		{"GIAY-IN-ANH", "GIẤY IN ẢNH", "Thiệp"},
		{"MICA-3-LY", "Mica 3 ly", "Mica"},
		{"MICA-2-LY", "Mica 2 ly", "Mica"},
		{"MICA-TRANG-3-LY", "Mica trắng 3 ly", "Mica"},
		{"MICA-HOLO-THUONG", "Mica Holo thường", "Mica"},
		{"MICA-HOLO-START", "Mica Holo start", "Mica"},
		{"MICA-5-LY", "Mica 5 ly", "Mica"},
		{"BASSWOOD-3-LY", "Basswood 3 ly", "Gỗ"},
		{"BASSWOOD-5-LY", "Basswood 5 ly", "Gỗ"},
		{"PHOI-GO-TU-NHIEN", "PHOI-GO-TU-NHIEN", "Gỗ"},
		{"DE-GO", "Đế gỗ", "Gỗ"},
		{"WD-PBH-6L-10IN", "Basswood 5 ly", "Gỗ"},
		{"CERAMIC", "Ceramic", ""},
		{"GLASS", "Glass", ""},
		{"THE-GAI", "Thẻ gài", ""},
		{"THE-KEP", "Thẻ kẹp", ""},
		// whole words only: "GO" must not catch LOGO / GOLD
		{"LOGO-DAN", "Logo dán", ""},
		{"GOLD-FOIL", "Gold foil", ""},
	} {
		if got := name(r.Classify([]GoodsMaterial{{Code: c.code, Name: c.name}}, "")); got != c.want {
			t.Errorf("%s (%s): kind %q, want %q", c.code, c.name, got, c.want)
		}
	}
}

// A SKU without a matching material falls back to its HS code; a material
// always wins over the HS code.
func TestTHERules_HSFallbackAndMaterialFirst(t *testing.T) {
	r := DefaultTHEServiceRules()
	kind := func(mats []GoodsMaterial, hs string) string {
		if i := r.Classify(mats, hs); i >= 0 {
			return r.Groups[i].Name
		}
		return ""
	}
	if got := kind(nil, "4909002000347"); got != "Thiệp" {
		t.Fatalf("no material, postcard HS: %q", got)
	}
	if got := kind(nil, "3926400090256"); got != "Mica" {
		t.Fatalf("no material, acrylic HS: %q", got)
	}
	if got := kind(nil, "9503000090301"); got != "Gỗ" {
		t.Fatalf("no material, THE's wooden-decor code: %q", got)
	}
	// 9503000090335 is a stone dog toy on THE: not wood.
	if got := kind(nil, "9503000090335"); got != "" {
		t.Fatalf("stone toy HS must stay unclassified: %q", got)
	}
	if got := kind([]GoodsMaterial{{Code: "CERAMIC", Name: "Ceramic"}}, "6913902000300"); got != "" {
		t.Fatalf("ceramic: %q", got)
	}
	// Material says mica, HS says postcard (a data slip): the material wins.
	if got := kind([]GoodsMaterial{{Code: "MICA-3-LY", Name: "Mica 3 ly"}}, "4909002000347"); got != "Mica" {
		t.Fatalf("material must win over HS: %q", got)
	}
}

func lines(r THEServiceRules, kinds ...string) []THEGoodsLine {
	var out []THEGoodsLine
	for i, k := range kinds {
		g := -1
		for j := range r.Groups {
			if r.Groups[j].Name == k {
				g = j
			}
		}
		mats := []string{}
		if g < 0 {
			mats = []string{k}
		}
		out = append(out, THEGoodsLine{SKUCode: "SKU" + string(rune('A'+i)), Group: g, Materials: mats})
	}
	return out
}

// Nghĩa's rule and the mixed-parcel rule: thiệp alone → PCU; mica or gỗ
// anywhere in the parcel → SPU.
func TestTHERules_PickServicePerParcel(t *testing.T) {
	r := DefaultTHEServiceRules()
	for _, c := range []struct {
		kinds []string
		want  string
	}{
		{[]string{"Thiệp"}, "PCU"},
		{[]string{"Thiệp", "Thiệp"}, "PCU"},
		{[]string{"Mica"}, "SPU"},
		{[]string{"Gỗ"}, "SPU"},
		{[]string{"Gỗ", "Mica"}, "SPU"},
		{[]string{"Thiệp", "Mica"}, "SPU"},
		{[]string{"Thiệp", "Gỗ"}, "SPU"},
		// unclassified with something known: the known kind decides
		{[]string{"Ceramic", "Mica"}, "SPU"},
		// unclassified alone: the connection's own service
		{[]string{"Ceramic"}, "DEF"},
	} {
		p, problem := r.Pick("DEF", lines(r, c.kinds...))
		if problem != "" || p.ServiceCode != c.want {
			t.Errorf("%v: service %q (problem %q), want %q", c.kinds, p.ServiceCode, problem, c.want)
		}
	}
	p, _ := r.Pick("DEF", lines(r, "Thiệp", "Gỗ"))
	if p.Goods != "Gỗ + Thiệp" || !strings.Contains(p.Reason, "Đơn trộn") || !strings.Contains(p.Reason, "theo Gỗ (SPU)") {
		t.Fatalf("mixed reason: %+v", p)
	}
	p, _ = r.Pick("DEF", lines(r, "Ceramic", "Mica"))
	if len(p.Unclassified) != 1 || !strings.Contains(p.Reason, "chưa xếp loại") {
		t.Fatalf("partly unclassified must say so: %+v", p)
	}
}

func TestTHERules_BlockAutoOffAndEmptyService(t *testing.T) {
	r := DefaultTHEServiceRules()
	r.Unclassified = THEUnclassifiedBlock
	if _, problem := r.Pick("DEF", lines(r, "Ceramic", "Mica")); !strings.Contains(problem, "SKUA") || !strings.Contains(problem, "NVL Ceramic") {
		t.Fatalf("block must name the SKU and its material: %q", problem)
	}
	if p, problem := r.Pick("DEF", lines(r, "Mica")); problem != "" || p.ServiceCode != "SPU" {
		t.Fatalf("known goods unaffected by block: %+v %q", p, problem)
	}

	off := DefaultTHEServiceRules()
	off.Auto = false
	if p, _ := off.Pick("DEF", lines(off, "Mica")); p.ServiceCode != "DEF" {
		t.Fatalf("auto off must use the connection's service: %+v", p)
	}

	blank := DefaultTHEServiceRules()
	blank.Groups[0].ServiceCode = "" // Mica with no service of its own
	if p, _ := blank.Pick("DEF", lines(blank, "Mica")); p.ServiceCode != "DEF" {
		t.Fatalf("a kind without service uses the connection's: %+v", p)
	}
}

// Priority is the table order: move Thiệp first and a mixed parcel follows it.
func TestTHERules_PriorityIsTableOrder(t *testing.T) {
	r := DefaultTHEServiceRules()
	r.Groups = []THEGoodsGroup{r.Groups[2], r.Groups[0], r.Groups[1]} // Thiệp, Mica, Gỗ
	if p, _ := r.Pick("DEF", lines(r, "Mica", "Thiệp")); p.ServiceCode != "PCU" {
		t.Fatalf("Thiệp listed first must win a mixed parcel: %+v", p)
	}
}

func TestTHERules_CleanAndParse(t *testing.T) {
	in := THEServiceRules{Auto: true, Groups: []THEGoodsGroup{
		{Name: "  Mica ", ServiceCode: " SPU ", Materials: []string{" mica ", "MICA", "Acrylic-3"}, HSPrefixes: []string{"3926.40", "3926 40"}},
	}}
	out, err := in.Clean()
	if err != nil {
		t.Fatal(err)
	}
	g := out.Groups[0]
	if g.Name != "Mica" || g.ServiceCode != "SPU" || strings.Join(g.Materials, "|") != "MICA|ACRYLIC 3" ||
		strings.Join(g.HSPrefixes, "|") != "392640" || out.Unclassified != THEUnclassifiedDefault {
		t.Fatalf("cleaned: %+v", out)
	}
	for _, bad := range []THEServiceRules{
		{Groups: []THEGoodsGroup{{Name: "", Materials: []string{"X"}}}},
		{Groups: []THEGoodsGroup{{Name: "Mica"}}},
		{Groups: []THEGoodsGroup{{Name: "Gỗ", Materials: []string{"GO"}}, {Name: "gỗ", Materials: []string{"WOOD"}}}},
		{Unclassified: "maybe"},
	} {
		if _, err := bad.Clean(); err == nil {
			t.Fatalf("must refuse %+v", bad)
		}
	}

	if r, saved := ParseTHEServiceRules(nil); saved || len(r.Groups) != 3 || !r.Auto {
		t.Fatalf("nothing stored = defaults: %+v %v", r, saved)
	}
	raw, _ := ToJSONB(out)
	if r, saved := ParseTHEServiceRules(raw); !saved || len(r.Groups) != 1 || r.Groups[0].Name != "Mica" {
		t.Fatalf("stored rules: %+v %v", r, saved)
	}
}
