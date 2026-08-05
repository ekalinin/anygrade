package queue

import (
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/store"
)

func policyTask(maxAttempts int, cooldown time.Duration, hard *time.Time) config.ResolvedTask {
	return config.ResolvedTask{
		ID: "t", Score: 100,
		Limits:   config.ResolvedLimits{MaxAttempts: maxAttempts, Cooldown: cooldown},
		Deadline: config.ResolvedDeadline{Hard: hard},
	}
}

func subAt(status string, counts bool, at time.Time) store.Submission {
	return store.Submission{Status: status, Counts: counts, ReceivedAt: at}
}

// retrying is an infra_error still scheduled for another run; without a
// retry_at the same row is terminal and never ran (SPEC §13).
func retrying(at time.Time, retryAt time.Time) store.Submission {
	s := subAt(store.StatusInfraError, true, at)
	s.RetryAt = &retryAt
	return s
}

func TestAdmit(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	pastHard := now.Add(-time.Hour)
	futureHard := now.Add(time.Hour)

	tests := []struct {
		name      string
		task      config.ResolvedTask
		history   []store.Submission
		recheck   bool
		admit     bool
		status    string
		counts    bool
		reasonHas string
	}{
		{
			name:  "no limits, admitted",
			task:  policyTask(0, 0, nil),
			admit: true, counts: true,
		},
		{
			name:      "past hard deadline",
			task:      policyTask(0, 0, &pastHard),
			status:    store.StatusRejectedDeadline,
			reasonHas: "hard deadline",
		},
		{
			name:  "before hard deadline",
			task:  policyTask(0, 0, &futureHard),
			admit: true, counts: true,
		},
		{
			name: "attempt limit reached",
			task: policyTask(2, 0, nil),
			history: []store.Submission{
				subAt(store.StatusDone, true, now.Add(-2*time.Hour)),
				subAt(store.StatusQueued, true, now.Add(-time.Hour)),
			},
			status:    store.StatusRejectedLimit,
			reasonHas: "attempt limit",
		},
		{
			name: "rejected and non-counting rows do not consume attempts",
			task: policyTask(2, 0, nil),
			history: []store.Submission{
				subAt(store.StatusRejectedLimit, true, now.Add(-3*time.Hour)),
				subAt(store.StatusRejectedDeadline, true, now.Add(-2*time.Hour)),
				subAt(store.StatusDone, false, now.Add(-time.Hour)), // teacher recheck
				subAt(store.StatusDone, true, now.Add(-time.Hour)),
			},
			admit: true, counts: true,
		},
		{
			name: "terminal infra_error refunds the attempt",
			task: policyTask(1, 0, nil),
			history: []store.Submission{
				subAt(store.StatusInfraError, true, now.Add(-time.Hour)),
			},
			admit: true, counts: true,
		},
		{
			name: "retrying infra_error still holds its slot",
			task: policyTask(1, 0, nil),
			history: []store.Submission{
				retrying(now.Add(-time.Hour), now.Add(time.Minute)),
			},
			status:    store.StatusRejectedLimit,
			reasonHas: "attempt limit",
		},
		{
			name: "terminal infra_error does not start a cooldown",
			task: policyTask(0, 10*time.Minute, nil),
			history: []store.Submission{
				subAt(store.StatusInfraError, true, now.Add(-time.Minute)),
			},
			admit: true, counts: true,
		},
		{
			name: "retrying infra_error keeps the cooldown running",
			task: policyTask(0, 10*time.Minute, nil),
			history: []store.Submission{
				retrying(now.Add(-time.Minute), now.Add(time.Minute)),
			},
			status:    store.StatusRejectedLimit,
			reasonHas: "cooldown",
		},
		{
			name: "cooldown active",
			task: policyTask(0, 10*time.Minute, nil),
			history: []store.Submission{
				subAt(store.StatusDone, true, now.Add(-5*time.Minute)),
			},
			status:    store.StatusRejectedLimit,
			reasonHas: "cooldown",
		},
		{
			name: "cooldown expired",
			task: policyTask(0, 10*time.Minute, nil),
			history: []store.Submission{
				subAt(store.StatusDone, true, now.Add(-11*time.Minute)),
			},
			admit: true, counts: true,
		},
		{
			name: "teacher recheck bypasses everything",
			task: policyTask(1, time.Hour, &pastHard),
			history: []store.Submission{
				subAt(store.StatusDone, true, now.Add(-time.Minute)),
			},
			recheck: true,
			admit:   true, counts: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Admit(tc.task, tc.history, now, tc.recheck)
			if d.Admit != tc.admit {
				t.Fatalf("admit = %v, want %v (%+v)", d.Admit, tc.admit, d)
			}
			if !tc.admit && d.RejectStatus != tc.status {
				t.Errorf("status = %q, want %q", d.RejectStatus, tc.status)
			}
			if tc.admit && d.Counts != tc.counts {
				t.Errorf("counts = %v, want %v", d.Counts, tc.counts)
			}
			if tc.reasonHas != "" && !strings.Contains(d.RejectReason, tc.reasonHas) {
				t.Errorf("reason %q must contain %q", d.RejectReason, tc.reasonHas)
			}
		})
	}
}

// TestQuotaMatchesAdmit: the task page shows the numbers the policy enforces,
// so a student is never promised an attempt Admit would refuse.
func TestQuotaMatchesAdmit(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	history := []store.Submission{
		subAt(store.StatusDone, true, now.Add(-time.Hour)),               // consumed
		subAt(store.StatusInfraError, true, now.Add(-30*time.Minute)),    // terminal: refunded
		subAt(store.StatusRejectedLimit, true, now.Add(-20*time.Minute)), // never ran
		subAt(store.StatusDone, false, now.Add(-15*time.Minute)),         // teacher recheck
		retrying(now.Add(-time.Minute), now.Add(time.Minute)),            // in flight
	}
	if n := CountAttempts(history); n != 2 {
		t.Fatalf("CountAttempts = %d, want 2", n)
	}
	left, unlimited, cooldown := Quota(policyTask(3, 10*time.Minute, nil), history, now)
	if unlimited || left != 1 {
		t.Errorf("attempts left = %d (unlimited = %v), want 1", left, unlimited)
	}
	// Anchored at the in-flight row, not at the terminal infra_error.
	if want := now.Add(9 * time.Minute); cooldown == nil || !cooldown.Equal(want) {
		t.Errorf("cooldown until = %v, want %v", cooldown, want)
	}
}
