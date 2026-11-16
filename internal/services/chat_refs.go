package services

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Mã đơn / mã batch trong tin tự thành link (#5453). Định dạng mã nội bộ:
//   - đơn: 6–7 chữ số ("106788" = 100000 + id đơn);
//   - sản phẩm: mã đơn + "_vị trí/tổng" ("106788_1/2") → mở đơn của nó;
//   - batch: "#" + số ("#101558"; batch cũ có thể ngắn hơn, "#1558").
// Chỉ mã CÓ THẬT trong DB mới thành link (dò lúc gửi và lúc sửa, lưu vào cột
// refs của tin), nên một con số bất kỳ trùng định dạng không bị gắn link bừa.

var chatCodeRe = regexp.MustCompile(`#?\d{3,7}(?:_\d{1,3}/\d{1,3})?`)

const chatMaxRefs = 20

// isCodeRune: ký tự dính liền với mã thì mã đó là một phần của chuỗi khác (URL,
// số dài hơn, chữ) — bỏ qua.
func isCodeRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '/' || r == '#' || r == '-' || r == '.' || r == '=' || r == '&' || r == '?'
}

// extractChatCodes tách các mã ứng viên trong body (chưa tra DB).
func extractChatCodes(body string) (orders, items, batches []string) {
	seen := map[string]bool{}
	for _, loc := range chatCodeRe.FindAllStringIndex(body, -1) {
		code := body[loc[0]:loc[1]]
		if loc[0] > 0 {
			if r, _ := utf8.DecodeLastRuneInString(body[:loc[0]]); isCodeRune(r) {
				continue
			}
		}
		if loc[1] < len(body) {
			r, _ := utf8.DecodeRuneInString(body[loc[1]:])
			// Dấu chấm cuối câu ("đơn 106788.") vẫn được; chấm nối chữ/số thì không.
			if r == '.' {
				if loc[1]+1 < len(body) {
					if n, _ := utf8.DecodeRuneInString(body[loc[1]+1:]); unicode.IsLetter(n) || unicode.IsDigit(n) {
						continue
					}
				}
			} else if isCodeRune(r) {
				continue
			}
		}
		if seen[code] {
			continue
		}
		seen[code] = true
		switch {
		case strings.HasPrefix(code, "#"):
			if !strings.Contains(code, "_") {
				batches = append(batches, code)
			}
		case strings.Contains(code, "_"):
			if len(code[:strings.IndexByte(code, '_')]) >= 6 {
				items = append(items, code)
			}
		case len(code) >= 6:
			orders = append(orders, code)
		}
		if len(orders)+len(items)+len(batches) >= chatMaxRefs {
			break
		}
	}
	return orders, items, batches
}

// resolveRefs: JSON các mã có thật trong body ("" nếu không có). Lỗi tra cứu
// chỉ làm mất link, không bao giờ chặn việc gửi tin.
func (s *ChatService) resolveRefs(body string) string {
	orders, items, batches := extractChatCodes(body)
	if len(orders)+len(items)+len(batches) == 0 {
		return ""
	}
	hits, err := s.repo.Chat.ResolveCodes(orders, items, batches)
	if err != nil {
		chatLog("dò mã đơn/batch lỗi: %v", err)
		return ""
	}
	if len(hits) == 0 {
		return ""
	}
	refs := make([]ChatRef, 0, len(hits))
	for _, h := range hits {
		refs = append(refs, ChatRef{Code: h.Code, Kind: h.Kind, ID: h.ID})
	}
	b, _ := json.Marshal(refs)
	return string(b)
}
