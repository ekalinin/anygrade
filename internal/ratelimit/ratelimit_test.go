package ratelimit

import (
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
