package middleware

import (
	"testing"
	"time"
)

func TestRateLimiter_BlocksAfterMax(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// First 3 within the window are allowed.
	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4", base.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	// 4th within the window is blocked.
	if rl.allow("1.2.3.4", base.Add(4*time.Second)) {
		t.Fatal("4th attempt within window should be blocked")
	}
}

func TestRateLimiter_WindowRollsOff(t *testing.T) {
	rl := newRateLimiter(2, time.Minute)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	rl.allow("ip", base)
	rl.allow("ip", base.Add(time.Second))
	if rl.allow("ip", base.Add(2*time.Second)) {
		t.Fatal("3rd within window should be blocked")
	}
	// After the window passes, the old hits expire and requests are allowed again.
	if !rl.allow("ip", base.Add(2*time.Minute)) {
		t.Fatal("request after window should be allowed again")
	}
}

func TestRateLimiter_IsolatesKeys(t *testing.T) {
	rl := newRateLimiter(1, time.Minute)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	if !rl.allow("a", base) {
		t.Fatal("first for a should pass")
	}
	if !rl.allow("b", base) {
		t.Fatal("first for b should pass (separate key)")
	}
	if rl.allow("a", base.Add(time.Second)) {
		t.Fatal("second for a should be blocked")
	}
}
