package models

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"
)

// NormalizeCode canonicalises a user-entered code so every "mã" in the system is
// ALWAYS uppercase, free of Vietnamese diacritics, and free of spaces. Rules:
//   - strip combining diacritical marks (á, ộ, ẩ … → a, o, a)
//   - đ / Đ → D (they don't decompose)
//   - whitespace → hyphen
//   - uppercase everything
//   - keep only [A-Z0-9-_.]; drop any other symbol
//   - collapse repeated hyphens and trim leading/trailing hyphens
//
// e.g. "Mica Trọng 3 Ly" → "MICA-TRONG-3-LY", "Gỗ 5 ly" → "GO-5-LY", "wood" → "WOOD".
func NormalizeCode(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.TrimSpace(s)) {
		switch {
		case unicode.Is(unicode.Mn, r): // combining diacritical mark
			continue
		case r == 'đ' || r == 'Đ':
			b.WriteByte('D')
		case unicode.IsSpace(r):
			b.WriteByte('-')
		default:
			r = unicode.ToUpper(r)
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				b.WriteRune(r)
			}
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

// BeforeSave hooks guarantee the canonical code form no matter how a record is
// written (form, API, file import, seed). This is the single enforcement point —
// any new write path is covered automatically.
func (m *Material) BeforeSave(*gorm.DB) error { m.Code = NormalizeCode(m.Code); return nil }
func (s *SKU) BeforeSave(*gorm.DB) error      { s.Code = NormalizeCode(s.Code); return nil }
func (s *Seller) BeforeSave(*gorm.DB) error   { s.Code = NormalizeCode(s.Code); return nil }
