package repositories

import (
	"strings"
	"testing"
	"unicode/utf8"

	"the-fulfillment/backend/internal/models"
)

func TestClipImportErrorFitsColumns(t *testing.T) {
	long := strings.Repeat("Đơn có địa chỉ rất dài ", 30)
	e := ClipImportError(models.ImportError{
		StoreOrderID: strings.Repeat("A", 200),
		SKU:          "WD CS 8in kèm hộp gỗ khắc tên và thiệp chúc mừng giáng sinh",
		Field:        strings.Repeat("f", 80),
		ErrorCode:    strings.Repeat("E", 80),
		Message:      long,
		Suggestion:   long,
	})
	limits := map[string]struct {
		got string
		max int
	}{
		"store_order_id": {e.StoreOrderID, 120},
		"sku":            {e.SKU, 48},
		"field":          {e.Field, 60},
		"error_code":     {e.ErrorCode, 60},
		"message":        {e.Message, 255},
		"suggestion":     {e.Suggestion, 255},
	}
	for name, c := range limits {
		if n := utf8.RuneCountInString(c.got); n > c.max {
			t.Errorf("%s has %d characters, column allows %d", name, n, c.max)
		}
		if !utf8.ValidString(c.got) {
			t.Errorf("%s is not valid UTF-8 after clipping", name)
		}
	}
	if !strings.HasSuffix(e.Message, "…") {
		t.Errorf("a clipped message should end with …, got %q", e.Message)
	}
}

func TestClipImportErrorKeepsShortText(t *testing.T) {
	in := models.ImportError{SKU: "BR-AC-2.5-KEP", Message: "SKU chưa có trong master data", StoreOrderID: "US-0525-001 4240715987"}
	out := ClipImportError(in)
	if out.SKU != in.SKU || out.Message != in.Message || out.StoreOrderID != in.StoreOrderID {
		t.Fatalf("short values changed: %+v", out)
	}
}
