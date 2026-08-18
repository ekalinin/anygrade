package ratelimit

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestBlocksAfterMaxFailures(t *testing.T) {
	l := New(3, time.Minute)
	for range 2 {
		l.Fail("k")
		if l.Blocked("k") {
			t.Fatal("blocked before reaching max failures")
		}
	}
	l.Fail("k")
	if !l.Blocked("k") {
		t.Fatal("not blocked after max failures")
	}
	if l.Blocked("other") {
		t.Fatal("unrelated key blocked")
	}
}

func TestWindowExpiryUnblocks(t *testing.T) {
	now := time.Now()
	l := New(2, time.Minute)
	l.now = func() time.Time { return now }
	l.Fail("k")
	l.Fail("k")
	if !l.Blocked("k") {
		t.Fatal("expected blocked")
	}
	now = now.Add(time.Minute + time.Second)
	if l.Blocked("k") {
		t.Fatal("still blocked after the window expired")
	}
}

func TestClearOnSuccess(t *testing.T) {
	l := New(1, time.Minute)
	l.Fail("k")
	if !l.Blocked("k") {
		t.Fatal("expected blocked")
	}
	l.Clear("k")
	if l.Blocked("k") {
		t.Fatal("blocked after Clear")
	}
}

// TestPerIPBudgetSurvivesVaryingLogin: the login half of the key is
// attacker-controlled, so a fresh login must not buy a fresh budget.
func TestPerIPBudgetSurvivesVaryingLogin(t *testing.T) {
	l := New(2, time.Minute) // per-IP budget = 2 * ipFactor
	for i := range l.ipMax {
		key := AuthKey("10.0.0.1:1234", "login"+strconv.Itoa(i))
		if l.Blocked(key) {
			t.Fatalf("attempt %d blocked before the IP budget was spent", i)
		}
		l.Fail(key)
	}
	if !l.Blocked(AuthKey("10.0.0.1:1234", "yet-another")) {
		t.Fatal("a never-seen login still has a budget after the IP's was spent")
	}
	if l.Blocked(AuthKey("10.0.0.2:1234", "login0")) {
		t.Fatal("another client IP was blocked too")
	}
}

// TestBucketsStayBounded: a stream of failures with unique keys must not grow
// the tracked set without limit, and must not cost a full sweep per failure.
func TestBucketsStayBounded(t *testing.T) {
	l := New(10, time.Minute)
	l.maxKeys = 64
	for i := range 10_000 {
		l.Fail(AuthKey("10.0.0.1:1234", "login"+strconv.Itoa(i)))
	}
	l.mu.Lock()
	n, lru := len(l.index), l.lru.Len()
	l.mu.Unlock()
	if n > l.maxKeys || lru != n {
		t.Fatalf("tracked buckets: index=%d lru=%d, want <= %d and equal", n, lru, l.maxKeys)
	}
	// The IP bucket is touched on every attempt, so LRU eviction can never
	// drop the budget that makes the flood self-limiting.
	if !l.Blocked(AuthKey("10.0.0.1:1234", "fresh")) {
		t.Fatal("the client IP is not blocked after a flood of unique logins")
	}
}

// TestReserveIsAtomic: a simultaneous burst must not all pass the check before
// the first failure is recorded - the slot is taken at check time.
func TestReserveIsAtomic(t *testing.T) {
	const max, burst = 5, 200
	l := New(max, time.Minute)
	var mu sync.Mutex
	admitted := 0
	var wg sync.WaitGroup
	for range burst {
		wg.Go(func() {
			rv, ok := l.Reserve("k")
			if !ok {
				return
			}
			mu.Lock()
			admitted++
			mu.Unlock()
			rv.Fail()
		})
	}
	wg.Wait()
	if admitted != max {
		t.Fatalf("admitted %d of %d concurrent attempts, want exactly %d", admitted, burst, max)
	}
}

func TestReservationOutcomes(t *testing.T) {
	l := New(2, time.Minute)

	// Release gives the slot back: nothing was compared, nothing is counted.
	rv, ok := l.Reserve("k")
	if !ok {
		t.Fatal("first Reserve refused")
	}
	rv.Release()
	rv.Release() // second call is a no-op, so `defer rv.Release()` is safe
	if l.Blocked("k") {
		t.Fatal("blocked after Release only")
	}

	// Fail counts once, even when Release runs afterwards.
	for range 2 {
		rv, ok := l.Reserve("k")
		if !ok {
			t.Fatal("Reserve refused inside the budget")
		}
		rv.Fail()
		rv.Release()
	}
	if !l.Blocked("k") {
		t.Fatal("not blocked after max reserved failures")
	}

	// Success forgets the key like Clear.
	l.Clear("k")
	rv, ok = l.Reserve("k")
	if !ok {
		t.Fatal("Reserve refused after Clear")
	}
	rv.Success()
	if l.Blocked("k") {
		t.Fatal("blocked after Success")
	}
}

func TestNilLimiterNeverBlocks(t *testing.T) {
	var l *Limiter
	l.Fail("k")
	l.Clear("k")
	if l.Blocked("k") {
		t.Fatal("nil limiter blocked")
	}
	rv, ok := l.Reserve("k")
	if !ok {
		t.Fatal("nil limiter refused a reservation")
	}
	rv.Fail()
	rv.Success()
	rv.Release()
}
