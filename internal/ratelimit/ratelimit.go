// Package ratelimit provides a small in-process failure limiter guarding
// credential checks (web login, git basic auth) against token brute force.
// Only failures count: normal traffic with valid credentials is never
// throttled, and a success clears the key.
package ratelimit

import (
	"container/list"
	"net"
	"strings"
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

// ClientAddr picks the address whose budget an attempt is charged to.
//
// trustForwarded is the operator's --behind-proxy opt-in, and both answers are
// wrong without it. Ignoring the header when a proxy really is in front puts
// every client in the proxy's single per-IP bucket, so a few failed logins lock
// out the whole course. Reading it when no proxy is in front lets anyone who
// can reach the port name their own bucket and take a fresh budget per request
// - the exact bypass the per-IP budget exists to close.
//
// The rightmost entry is the one our own proxy appended, i.e. the address it
// actually saw; everything to its left came from the client and is forgeable.
func ClientAddr(remoteAddr, forwarded string, trustForwarded bool) string {
	if trustForwarded {
		if i := strings.LastIndex(forwarded, ","); i >= 0 {
			forwarded = forwarded[i+1:]
		}
		if host := strings.TrimSpace(forwarded); host != "" {
			return host
		}
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

const (
	// ipFactor sets the per-IP budget as a multiple of the per-key one. The
	// login half of a key is attacker-controlled, so without a second budget
	// varying it buys a fresh allowance on every attempt. The factor is
	// deliberately generous: a shared NAT must survive several students
	// mistyping their token, while an attacker cycling logins still runs into
	// a ceiling instead of an unlimited budget.
	ipFactor = 20
	// maxBuckets caps how many keys are tracked at once. Eviction is strictly
	// least-recently-used, so a flood of unique logins costs O(1) per attempt
	// and bounded memory - the old full-map sweep was O(n) per failure.
	maxBuckets = 8192
	// ipPrefix namespaces the per-IP buckets away from the caller's keys.
	ipPrefix = "\x00ip|"
)

// bucket is one budget: the failures still inside the window plus the attempts
// currently in flight (reservations that have not reported an outcome yet).
type bucket struct {
	key   string
	fails []time.Time
	held  int
}

func (b *bucket) count() int { return len(b.fails) + b.held }

// Limiter blocks a key once it accumulates Max failures within Window, and
// blocks a whole client IP once its own, larger budget is exhausted.
// Keys are caller-defined (the servers use "clientIP|login" so one student
// behind a shared NAT cannot lock out the rest, and an attacker cannot lock
// out a victim's login from elsewhere).
type Limiter struct {
	max     int
	ipMax   int
	window  time.Duration
	maxKeys int
	now     func() time.Time // test seam; time.Now in production

	mu    sync.Mutex
	index map[string]*list.Element
	lru   *list.List // most recently touched at the front
}

// New returns a limiter allowing max-1 failures per window before blocking.
func New(max int, window time.Duration) *Limiter {
	return &Limiter{
		max: max, ipMax: max * ipFactor, window: window, maxKeys: maxBuckets,
		now:   time.Now,
		index: map[string]*list.Element{}, lru: list.New(),
	}
}

// Blocked reports whether the key, or its client IP, has exhausted its failure
// budget. It only reads: use Reserve to make the check and the count atomic.
func (l *Limiter) Blocked(key string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.overBudget(key)
}

// Fail records one failed attempt for the key (and for its client IP).
func (l *Limiter) Fail(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record(key)
}

// Clear forgets the key (called after a successful authentication). The client
// IP's own budget survives on purpose: otherwise one valid credential would let
// its holder reset the IP budget between brute-force bursts.
func (l *Limiter) Clear(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.drop(key)
	l.release(ipBucket(key))
}

// Reservation is one credential check in flight. It holds a slot of the budget
// from the moment the check is allowed until its outcome is known, so a burst
// of simultaneous requests can no longer all pass the check before the first
// failure is recorded.
//
// Exactly one of Fail, Success, or Release applies; the rest are no-ops, which
// makes `defer rv.Release()` safe next to an explicit outcome. A Reservation
// belongs to the one request that made it and is not for concurrent use.
type Reservation struct {
	l   *Limiter
	key string
}

// Reserve checks the budget and takes a slot for the attempt about to run, in
// one atomic step. ok=false means blocked - no slot was taken. A nil Limiter
// never blocks.
func (l *Limiter) Reserve(key string) (*Reservation, bool) {
	if l == nil {
		return nil, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.overBudget(key) {
		return nil, false
	}
	l.touch(key).held++
	l.touch(ipBucket(key)).held++
	return &Reservation{l: l, key: key}, true
}

// Fail turns the reservation into a recorded failure.
func (rv *Reservation) Fail() {
	if l, key, ok := rv.consume(); ok {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.record(key)
	}
}

// Success reports a valid credential: the key is forgotten, like Clear.
func (rv *Reservation) Success() {
	if l, key, ok := rv.consume(); ok {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.drop(key)
		l.release(ipBucket(key))
	}
}

// Release gives the slot back without recording anything - the request ended
// before any credential was compared.
func (rv *Reservation) Release() {
	if l, key, ok := rv.consume(); ok {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.release(key)
		l.release(ipBucket(key))
	}
}

// consume takes the reservation's slot ownership exactly once.
func (rv *Reservation) consume() (*Limiter, string, bool) {
	if rv == nil || rv.l == nil {
		return nil, "", false
	}
	l := rv.l
	rv.l = nil
	return l, rv.key, true
}

// overBudget reports whether the key or its client IP is out of budget; the
// caller holds mu.
func (l *Limiter) overBudget(key string) bool {
	if b := l.lookup(key); b != nil && b.count() >= l.max {
		return true
	}
	b := l.lookup(ipBucket(key))
	return b != nil && b.count() >= l.ipMax
}

// record appends a failure to the key and to its client IP, consuming an
// outstanding reservation when there is one; the caller holds mu.
func (l *Limiter) record(key string) {
	now := l.now()
	for _, k := range [2]string{key, ipBucket(key)} {
		b := l.touch(k)
		if b.held > 0 {
			b.held--
		}
		b.fails = append(b.fails, now)
	}
}

// lookup returns the pruned bucket for key, or nil when it is untracked or has
// nothing left inside the window; the caller holds mu.
func (l *Limiter) lookup(key string) *bucket {
	el, ok := l.index[key]
	if !ok {
		return nil
	}
	b := el.Value.(*bucket)
	l.prune(b)
	if b.count() == 0 {
		l.lru.Remove(el)
		delete(l.index, key)
		return nil
	}
	return b
}

// touch returns the key's bucket, creating it if needed, and marks it as the
// most recently used; the caller holds mu.
func (l *Limiter) touch(key string) *bucket {
	if el, ok := l.index[key]; ok {
		l.lru.MoveToFront(el)
		b := el.Value.(*bucket)
		l.prune(b)
		return b
	}
	b := &bucket{key: key}
	l.index[key] = l.lru.PushFront(b)
	for l.lru.Len() > l.maxKeys {
		back := l.lru.Back()
		l.lru.Remove(back)
		delete(l.index, back.Value.(*bucket).key)
	}
	return b
}

// drop forgets the key entirely; the caller holds mu.
func (l *Limiter) drop(key string) {
	if el, ok := l.index[key]; ok {
		l.lru.Remove(el)
		delete(l.index, key)
	}
}

// release returns one in-flight slot; the caller holds mu.
func (l *Limiter) release(key string) {
	if el, ok := l.index[key]; ok {
		if b := el.Value.(*bucket); b.held > 0 {
			b.held--
		}
	}
}

// prune drops expired failures from the bucket; the caller holds mu.
func (l *Limiter) prune(b *bucket) {
	cutoff := l.now().Add(-l.window)
	kept := b.fails[:0]
	for _, t := range b.fails {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	b.fails = kept
}

// ipBucket is the per-client-IP budget derived from an AuthKey: everything
// before the first separator, namespaced so it cannot collide with a key.
func ipBucket(key string) string {
	host, _, _ := strings.Cut(key, "|")
	return ipPrefix + host
}
