package web

import (
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// rateLimiter is a small fixed-window limiter keyed by an arbitrary string
// (phone number, client IP). It exists to stop the unauthenticated OTP
// endpoints from being used to spam SMS codes or brute-force confirmations;
// it is not a general traffic shaper.
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
	last   time.Time // last global prune
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{hits: make(map[string][]time.Time), limit: limit, window: window}
}

// allow records a hit for key and reports whether it is within the limit.
func (rl *rateLimiter) allow(key string) bool {
	now := time.Now()
	cutoff := now.Add(-rl.window)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Occasionally drop keys that have gone quiet so the map can't grow unbounded.
	if now.Sub(rl.last) > rl.window {
		for k, ts := range rl.hits {
			if len(ts) == 0 || ts[len(ts)-1].Before(cutoff) {
				delete(rl.hits, k)
			}
		}
		rl.last = now
	}

	ts := rl.hits[key]
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	ts = ts[i:]
	if len(ts) >= rl.limit {
		rl.hits[key] = ts
		return false
	}
	rl.hits[key] = append(ts, now)
	return true
}

// clientIP returns the caller's IP, honouring the first X-Forwarded-For hop
// when we sit behind a reverse proxy.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// phoneRe is E.164: leading +, 7–15 digits, no leading zero.
var phoneRe = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// validPhone reports whether p is a plausible E.164 number. Phone numbers are
// used as path segments in the per-user stores, so this is also what keeps
// "../" out of the data directory.
func validPhone(p string) bool {
	return phoneRe.MatchString(p)
}
