package web

import (
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/store"
)

func sub(status string, final *float64, counts bool) store.Submission {
	return store.Submission{Status: status, FinalScore: final, Counts: counts}
}

func TestDeriveStatus(t *testing.T) {
	retryAt := time.Now().Add(time.Minute)
	tests := []struct {
		name    string
		history []store.Submission
		want    string
	}{
		{"empty", nil, StatusNotStarted},
		{"queued", []store.Submission{sub(store.StatusQueued, nil, true)}, "queued"},
		{"running", []store.Submission{sub(store.StatusRunning, nil, true)}, "running"},
		{"passed", []store.Submission{sub(store.StatusDone, new(float64(100)), true)}, "passed"},
		{"partial", []store.Submission{sub(store.StatusDone, new(float64(60)), true)}, "partial"},
		{"failed", []store.Submission{sub(store.StatusDone, new(float64(0)), true)}, "failed"},
		{"rejected", []store.Submission{sub(store.StatusRejectedDeadline, nil, true)}, "rejected"},
		{"latest wins", []store.Submission{
			sub(store.StatusDone, new(float64(100)), true),
			sub(store.StatusQueued, nil, true),
		}, "queued"},
		{"retrying", []store.Submission{{Status: store.StatusInfraError, RetryAt: &retryAt}}, StatusRetrying},
		{"terminal infra", []store.Submission{{Status: store.StatusInfraError}}, StatusError},
	}
	for _, tc := range tests {
		if got := deriveStatus(tc.history, 100); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDisplayScore(t *testing.T) {
	history := []store.Submission{
		sub(store.StatusDone, new(float64(80)), true),
		sub(store.StatusRejectedLimit, nil, true),
		sub(store.StatusDone, new(float64(50)), true),
	}
	if got := displayScore(history, "best"); got == nil || *got != 80 {
		t.Errorf("best: %v", got)
	}
	if got := displayScore(history, "latest"); got == nil || *got != 50 {
		t.Errorf("latest: %v", got)
	}
	if got := displayScore(nil, "best"); got != nil {
		t.Errorf("empty: %v", got)
	}
}

func TestCountdown(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	if got := countdown(now.Add(26*time.Hour), now); got != "in 1d 2h" {
		t.Errorf("future: %q", got)
	}
	if got := countdown(now.Add(-42*time.Minute), now); got != "42m overdue" {
		t.Errorf("past: %q", got)
	}
}
