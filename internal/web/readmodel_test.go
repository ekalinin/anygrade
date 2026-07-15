package web

import (
	"testing"
	"time"
)

// Status/score derivation tests moved to internal/gradebook with the code.

func TestCountdown(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	if got := countdown(now.Add(26*time.Hour), now); got != "in 1d 2h" {
		t.Errorf("future: %q", got)
	}
	if got := countdown(now.Add(-42*time.Minute), now); got != "42m overdue" {
		t.Errorf("past: %q", got)
	}
}
