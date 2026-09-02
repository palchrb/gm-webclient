package web

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiter_AllowsUpToLimitThenBlocks(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !rl.allow("k") {
			t.Fatalf("hit %d should be allowed", i+1)
		}
	}
	if rl.allow("k") {
		t.Fatal("4th hit should be blocked")
	}
	// Other keys are independent.
	if !rl.allow("other") {
		t.Fatal("different key should be allowed")
	}
}

func TestRateLimiter_WindowExpiry(t *testing.T) {
	rl := newRateLimiter(2, time.Minute)
	rl.allow("k")
	rl.allow("k")
	if rl.allow("k") {
		t.Fatal("should be blocked at limit")
	}
	// Age the recorded hits past the window.
	old := time.Now().Add(-2 * time.Minute)
	rl.mu.Lock()
	for i := range rl.hits["k"] {
		rl.hits["k"][i] = old
	}
	rl.mu.Unlock()
	if !rl.allow("k") {
		t.Fatal("should be allowed again after window")
	}
}

func TestRateLimiter_PrunesQuietKeys(t *testing.T) {
	rl := newRateLimiter(1, time.Minute)
	rl.allow("stale")
	rl.mu.Lock()
	rl.hits["stale"] = []time.Time{time.Now().Add(-3 * time.Minute)}
	rl.last = time.Now().Add(-3 * time.Minute) // force a global prune on next call
	rl.mu.Unlock()
	rl.allow("fresh")
	rl.mu.Lock()
	_, stillThere := rl.hits["stale"]
	rl.mu.Unlock()
	if stillThere {
		t.Fatal("expected quiet key to be pruned")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:4321"
	if got := clientIP(r); got != "10.0.0.5" {
		t.Fatalf("RemoteAddr: got %q", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("XFF first hop: got %q", got)
	}
	r.Header.Set("X-Forwarded-For", " 198.51.100.7 ")
	if got := clientIP(r); got != "198.51.100.7" {
		t.Fatalf("XFF single trimmed: got %q", got)
	}
}

func TestValidPhone(t *testing.T) {
	cases := map[string]bool{
		"+4712345678":       true,
		"+12125550123":      true,
		"+123456789012345":  true,  // 15 digits
		"+1234567":          true,  // 7 digits
		"+123456":           false, // too short
		"+1234567890123456": false, // 16 digits
		"4712345678":        false, // no plus
		"+0712345678":       false, // leading zero
		"+47 123 45 678":    false, // spaces
		"../../etc/passwd":  false,
		"":                  false,
	}
	for in, want := range cases {
		if got := validPhone(in); got != want {
			t.Errorf("validPhone(%q) = %v, want %v", in, got, want)
		}
	}
}
