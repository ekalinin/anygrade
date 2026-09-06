package queue

import (
	"testing"
	"time"
)

// TestBackoffDelaySaturates covers SPEC §13's min(base<<retries, cap): once
// retries is large enough that the shift would overflow time.Duration's
// int64 range, the schedule must still be monotone, never negative, and
// pinned at the cap rather than wrapping (issue #129).
func TestBackoffDelaySaturates(t *testing.T) {
	cases := []struct {
		name string
		base time.Duration
		cap  time.Duration
	}{
		{"10s base, 5m cap", 10 * time.Second, 5 * time.Minute},
		{"1m base, 1h cap", time.Minute, time.Hour},
	}
	retries := []int{0, 1, 8, 30, 54, 62, 63, 200}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var prev time.Duration
			for i, r := range retries {
				got := backoffDelay(c.base, c.cap, r)
				if got < 0 {
					t.Fatalf("retries=%d: delay went negative: %s", r, got)
				}
				if got > c.cap {
					t.Fatalf("retries=%d: delay %s exceeds cap %s", r, got, c.cap)
				}
				if i > 0 && got < prev {
					t.Fatalf("retries=%d: delay %s is less than the previous %s (must be non-decreasing)", r, got, prev)
				}
				prev = got
			}
			if prev != c.cap {
				t.Fatalf("retries=%d: expected saturation at cap %s, got %s", retries[len(retries)-1], c.cap, prev)
			}
		})
	}
}
