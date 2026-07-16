// Package ratelimit provides a small in-process failure limiter guarding
// credential checks (web login, git basic auth) against token brute force.
// Only failures count: normal traffic with valid credentials is never
// throttled, and a success clears the key.
package ratelimit

import (
	"net"
	"sync"
	"time"
)

// AuthKey is the shared failure-limiter key: client IP + login, so throttling
// one attacker/login pair never locks out a NAT neighbour or the login itself
// from elsewhere.
func AuthKey(remoteAddr, login string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return host + "|" + login
}

// Limiter blocks a key once it accumulates Max failures within Window.
// Keys are caller-defined (the servers use "clientIP|login" so one student
// behind a shared NAT cannot lock out the rest, and an attacker cannot lock
// out a victim's login from elsewhere).
type Limiter struct {
	max    int
	window time.Duration
	now    func() time.Time // test seam; time.Now in production

	mu    sync.Mutex
	fails map[string][]time.Time
}

// New returns a limiter allowing max-1 failures per window before blocking.
func New(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, now: time.Now, fails: map[string][]time.Time{}}
}

// Blocked reports whether the key has exhausted its failure budget.
func (l *Limiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key)) >= l.max
}

// Fail records one failed attempt for the key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[key] = append(l.prune(key), l.now())
	// Bound the map: expired keys accumulate only under active abuse from
	// many sources; a full sweep at this size is still cheap.
	if len(l.fails) > 4096 {
		for k := range l.fails {
			l.prune(k)
		}
	}
}

// Clear forgets the key (called after a successful authentication).
func (l *Limiter) Clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// prune drops expired failures for the key; the caller holds mu.
func (l *Limiter) prune(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	kept := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = kept
	return kept
}
