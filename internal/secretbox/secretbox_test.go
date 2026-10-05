package secretbox

import (
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := New(strings.Repeat("s", 40), "carrier-token")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal("the-token-123")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "the-token-123") {
		t.Fatal("sealed value still contains the plaintext")
	}
	got, err := box.Open(sealed)
	if err != nil || got != "the-token-123" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	// Random nonce: sealing twice never yields the same stored string.
	again, _ := box.Seal("the-token-123")
	if again == sealed {
		t.Fatal("two seals of the same secret produced identical output")
	}
}

// A rotated server secret (or a value sealed for another purpose) must fail
// loudly, never decrypt to garbage that is then sent to a paid API.
func TestOpenWithOtherKeyFails(t *testing.T) {
	a, _ := New(strings.Repeat("a", 40), "carrier-token")
	b, _ := New(strings.Repeat("b", 40), "carrier-token")
	c, _ := New(strings.Repeat("a", 40), "other-purpose")
	sealed, _ := a.Seal("secret")
	if _, err := b.Open(sealed); err != ErrUnreadable {
		t.Fatalf("other server secret: want ErrUnreadable, got %v", err)
	}
	if _, err := c.Open(sealed); err != ErrUnreadable {
		t.Fatalf("other purpose: want ErrUnreadable, got %v", err)
	}
	if _, err := a.Open("not-base64!!"); err != ErrUnreadable {
		t.Fatalf("garbage: want ErrUnreadable, got %v", err)
	}
}
