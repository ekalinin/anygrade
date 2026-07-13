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
