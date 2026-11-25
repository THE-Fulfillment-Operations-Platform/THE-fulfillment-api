package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Dịch vụ THE theo loại hàng (#7783, 25/11/2026). THE has one line per kind of
// goods for the factory's account: postcards go on "Postcard US" (PCU), mica
// and wood on "Standard Plus US" (SPU). FFM picks the line per parcel from what
// the parcel holds instead of sending everything with one service.
//
// A SKU's kind ("loại hàng") is read from its materials (NVL) first — the
// factory sets them for production, every orderable SKU has one — and only
// when no material says anything, from its HS code. The table lives in
// Settings → Kết nối THE so THE's side can change it without a deploy.

// Unclassified policies: what a parcel holding a SKU of no kind does.
const (
	// THEUnclassifiedDefault ships it with the connection's own service
	// (CarrierConfig.ServiceCode). On the factory's account THE keeps such a
	// parcel pending for its staff when its HS code is not on the account's list.
	THEUnclassifiedDefault = "default"
	// THEUnclassifiedBlock refuses the order with the reason.
	THEUnclassifiedBlock = "block"
)

// THEGoodsGroup is one row of the table: a kind of goods and its THE service.
type THEGoodsGroup struct {
	Name string `json:"name"`
	// ServiceCode is the THE service of this kind; "" = the connection's own.
	ServiceCode string `json:"service_code"`
	// ServiceName is THE's name of the service, for display only.
	ServiceName string `json:"service_name,omitempty"`
	// Materials are keywords matched against the material's code and name, as
	// whole words, without accents: "MICA" matches "MICA-3-LY" and "Mica Holo
	// thường"; "GO" matches "PHOI-GO-TU-NHIEN" and "Đế gỗ" but not "LOGO".
	Materials []string `json:"materials"`
	// HSPrefixes classify a SKU none of whose materials matched any row: its HS
	// code (own, else its parent's) starting with one of them.
	HSPrefixes []string `json:"hs_prefixes"`
}

// THEServiceRules is the whole table. Groups are in priority order: a parcel
// holding goods of several kinds ships with the service of the kind listed
// first ("đơn trộn"). The default order puts postcards last, so a parcel with
// a postcard and anything else does not go on the postcard line.
type THEServiceRules struct {
	// Auto off = every parcel ships with the connection's own service, as before.
	Auto         bool            `json:"auto"`
	Groups       []THEGoodsGroup `json:"groups"`
	Unclassified string          `json:"unclassified"`
}

// DefaultTHEServiceRules is Nghĩa's rule of 25/11/2026: thiệp → Postcard US,
// mica and gỗ → Standard Plus US; anything else keeps the connection's service.
func DefaultTHEServiceRules() THEServiceRules {
	return THEServiceRules{
		Auto:         true,
		Unclassified: THEUnclassifiedDefault,
		Groups: []THEGoodsGroup{
			{Name: "Mica", ServiceCode: "SPU", ServiceName: "Standard Plus US",
				Materials: []string{"MICA", "ACRYLIC"}, HSPrefixes: []string{"3926"}},
			{Name: "Gỗ", ServiceCode: "SPU", ServiceName: "Standard Plus US",
				Materials: []string{"BASSWOOD", "GO", "WOOD", "WD", "PLYWOOD"},
				// 9503000090301 is THE's own code for "Đồ trang trí gỗ" (other 9503
				// codes on THE are toys of other materials).
				HSPrefixes: []string{"4420", "4421", "9503000090301"}},
			{Name: "Thiệp", ServiceCode: "PCU", ServiceName: "Postcard US",
				Materials: []string{"GIAY IN ANH", "THIEP", "POSTCARD"}, HSPrefixes: []string{"4909"}},
		},
	}
}

// ParseTHEServiceRules reads the stored rules; nothing stored = the defaults.
func ParseTHEServiceRules(raw JSONB) (THEServiceRules, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return DefaultTHEServiceRules(), false
	}
	var r THEServiceRules
	if err := json.Unmarshal(raw, &r); err != nil {
		return DefaultTHEServiceRules(), false
	}
	if r.Unclassified != THEUnclassifiedBlock {
		r.Unclassified = THEUnclassifiedDefault
	}
	return r, true
}

// Limits of the table, so a runaway request cannot store a novel.
const (
	maxTHEGroups       = 20
	maxTHEGroupName    = 40
	maxTHEKeywords     = 30
	maxTHEKeywordRunes = 40
)

// Clean trims, de-duplicates and checks the rules as an operator typed them.
// The error is in the operator's words.
func (r THEServiceRules) Clean() (THEServiceRules, error) {
	out := THEServiceRules{Auto: r.Auto, Unclassified: r.Unclassified, Groups: []THEGoodsGroup{}}
	switch out.Unclassified {
	case "", THEUnclassifiedDefault:
		out.Unclassified = THEUnclassifiedDefault
	case THEUnclassifiedBlock:
	default:
		return out, fmt.Errorf("Cách xử lý hàng chưa xếp loại không hợp lệ: %q", r.Unclassified)
	}
	if len(r.Groups) > maxTHEGroups {
		return out, fmt.Errorf("Tối đa %d loại hàng", maxTHEGroups)
	}
	names := map[string]bool{}
	for i, g := range r.Groups {
		name := strings.TrimSpace(g.Name)
		if name == "" {
			return out, fmt.Errorf("Loại hàng dòng %d chưa có tên", i+1)
		}
		if len([]rune(name)) > maxTHEGroupName {
			return out, fmt.Errorf("Tên loại hàng %q dài quá %d ký tự", name, maxTHEGroupName)
		}
		key := strings.Join(goodsTokens(name), " ")
		if names[key] {
			return out, fmt.Errorf("Loại hàng %q bị trùng", name)
		}
		names[key] = true
		mats, err := cleanKeywords(g.Materials, name, false)
		if err != nil {
			return out, err
		}
		hs, err := cleanKeywords(g.HSPrefixes, name, true)
		if err != nil {
			return out, err
		}
		if len(mats) == 0 && len(hs) == 0 {
			return out, fmt.Errorf("Loại hàng %q chưa có từ khoá NVL hay mã HS nào — không SKU nào thuộc loại này", name)
		}
		svc := strings.TrimSpace(g.ServiceCode)
		if len(svc) > 60 {
			return out, fmt.Errorf("Mã dịch vụ của loại %q quá dài", name)
		}
		svcName := strings.TrimSpace(g.ServiceName)
		if svc == "" {
			svcName = ""
		}
		if len([]rune(svcName)) > 80 {
			svcName = string([]rune(svcName)[:80])
		}
		out.Groups = append(out.Groups, THEGoodsGroup{
			Name: name, ServiceCode: svc, ServiceName: svcName, Materials: mats, HSPrefixes: hs,
		})
	}
	return out, nil
}

func cleanKeywords(in []string, group string, digitsOnly bool) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range in {
		k := strings.TrimSpace(raw)
		if digitsOnly {
			k = onlyDigits(k)
		} else {
			k = strings.Join(goodsTokens(k), " ")
		}
		if k == "" || seen[k] {
			continue
		}
		if len([]rune(k)) > maxTHEKeywordRunes {
			return nil, fmt.Errorf("Từ khoá %q của loại %q dài quá %d ký tự", k, group, maxTHEKeywordRunes)
		}
		seen[k] = true
		out = append(out, k)
	}
	if len(out) > maxTHEKeywords {
		return nil, fmt.Errorf("Loại %q có quá %d từ khoá", group, maxTHEKeywords)
	}
	return out, nil
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// goodsTokens splits a code or a name into upper-case words without accents:
// "PHOI-GO-TU-NHIEN" → [PHOI GO TU NHIEN], "Đế gỗ" → [DE GO].
func goodsTokens(s string) []string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case r == 'đ' || r == 'Đ':
			b.WriteByte('D')
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToUpper(r))
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

// containsWords: the keyword's words appear one after the other in words.
func containsWords(words, keyword []string) bool {
	if len(keyword) == 0 || len(keyword) > len(words) {
		return false
	}
	for i := 0; i+len(keyword) <= len(words); i++ {
		ok := true
		for j := range keyword {
			if words[i+j] != keyword[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// MatchesMaterial reports whether one of the group's keywords names the material.
func (g THEGoodsGroup) MatchesMaterial(code, name string) bool {
	cw, nw := goodsTokens(code), goodsTokens(name)
	for _, k := range g.Materials {
		kw := goodsTokens(k)
		if containsWords(cw, kw) || containsWords(nw, kw) {
			return true
		}
	}
	return false
}

// MatchesHS reports whether the HS code starts with one of the group's prefixes.
func (g THEGoodsGroup) MatchesHS(hs string) bool {
	hs = onlyDigits(hs)
	if hs == "" {
		return false
	}
	for _, p := range g.HSPrefixes {
		if p = onlyDigits(p); p != "" && strings.HasPrefix(hs, p) {
			return true
		}
	}
	return false
}

// GoodsMaterial is one material of a SKU, as the classifier needs it.
type GoodsMaterial struct {
	Code string
	Name string
}

// Classify returns the index of the SKU's kind in Groups, or -1. A material
// matching several rows takes the first; the HS code only speaks when no
// material matched any row.
func (r THEServiceRules) Classify(materials []GoodsMaterial, hs string) int {
	for i, g := range r.Groups {
		for _, m := range materials {
			if g.MatchesMaterial(m.Code, m.Name) {
				return i
			}
		}
	}
	for i, g := range r.Groups {
		if g.MatchesHS(hs) {
			return i
		}
	}
	return -1
}

// THEGoodsLine is one SKU of a parcel, classified.
type THEGoodsLine struct {
	SKUCode string
	// Group is the index in Groups, -1 = no kind.
	Group int
	// Materials are the SKU's material names, for the refusal sentence.
	Materials []string
}

// THEServicePick is the service a parcel ships with, and why — in the
// operator's words, shown before the label is bought.
type THEServicePick struct {
	ServiceCode string `json:"service_code"`
	ServiceName string `json:"service_name,omitempty"`
	// Goods names the kinds the parcel holds ("Mica + Gỗ"), "" when none known.
	Goods string `json:"goods,omitempty"`
	// Reason is one short sentence: "Mica → SPU", "Đơn trộn Gỗ + Thiệp → theo Gỗ (SPU)".
	Reason string `json:"reason"`
	// Unclassified lists the parcel's SKUs of no kind.
	Unclassified []string `json:"unclassified,omitempty"`
}

// Pick chooses the parcel's service. defaultService is the connection's own
// service. problem != "" = the order must not go (unclassified goods under
// THEUnclassifiedBlock).
func (r THEServiceRules) Pick(defaultService string, lines []THEGoodsLine) (pick THEServicePick, problem string) {
	if !r.Auto {
		return THEServicePick{ServiceCode: defaultService,
			Reason: "Tự chọn dịch vụ theo loại hàng đang tắt → dịch vụ mặc định " + defaultService}, ""
	}
	best := -1
	present := map[int]bool{}
	var unknown []string
	unknownMats := map[string]bool{}
	var unknownMatList []string
	seenSKU := map[string]bool{}
	for _, l := range lines {
		if l.Group < 0 || l.Group >= len(r.Groups) {
			if !seenSKU[l.SKUCode] {
				seenSKU[l.SKUCode] = true
				unknown = append(unknown, l.SKUCode)
			}
			for _, m := range l.Materials {
				if m = strings.TrimSpace(m); m != "" && !unknownMats[m] {
					unknownMats[m] = true
					unknownMatList = append(unknownMatList, m)
				}
			}
			continue
		}
		present[l.Group] = true
		if best < 0 || l.Group < best {
			best = l.Group
		}
	}
	// Kinds in priority order, for the sentence.
	var kinds []string
	for i := range r.Groups {
		if present[i] {
			kinds = append(kinds, r.Groups[i].Name)
		}
	}
	pick.Goods = strings.Join(kinds, " + ")
	pick.Unclassified = unknown

	if len(unknown) > 0 && r.Unclassified == THEUnclassifiedBlock {
		what := "SKU " + strings.Join(unknown, ", ")
		if len(unknownMatList) > 0 {
			what += " (NVL " + strings.Join(unknownMatList, ", ") + ")"
		} else {
			what += " (chưa có NVL)"
		}
		return pick, what + " chưa thuộc loại hàng nào ở Cài đặt → Kết nối THE nên chưa chọn được dịch vụ THE — báo quản trị xếp loại, hoặc chỉ ghi nhận bàn giao"
	}
	if best < 0 {
		pick.ServiceCode = defaultService
		pick.Reason = "Hàng chưa xếp loại → dịch vụ mặc định " + defaultService
		return pick, ""
	}
	g := r.Groups[best]
	pick.ServiceCode, pick.ServiceName = g.ServiceCode, g.ServiceName
	if pick.ServiceCode == "" {
		pick.ServiceCode, pick.ServiceName = defaultService, ""
	}
	switch {
	case len(kinds) > 1:
		pick.Reason = fmt.Sprintf("Đơn trộn %s → theo %s (%s)", pick.Goods, g.Name, pick.ServiceCode)
	default:
		pick.Reason = fmt.Sprintf("%s → %s", g.Name, pick.ServiceCode)
	}
	if len(unknown) > 0 {
		pick.Reason += " · có hàng chưa xếp loại: " + strings.Join(unknown, ", ")
	}
	return pick, ""
}
