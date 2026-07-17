package web

import (
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/i18n"
)

// Status/score derivation tests moved to internal/gradebook with the code.

func TestCountdown(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	en := i18n.For("en")
	if got := countdown(now.Add(26*time.Hour), now, en); got != "in 1d 2h" {
		t.Errorf("future: %q", got)
	}
	if got := countdown(now.Add(-42*time.Minute), now, en); got != "42m overdue" {
		t.Errorf("past: %q", got)
	}
	ru := i18n.For("ru")
	if got := countdown(now.Add(26*time.Hour), now, ru); got != "через 1д 2ч" {
		t.Errorf("ru future: %q", got)
	}
}
