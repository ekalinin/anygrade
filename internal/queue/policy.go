// Package queue implements the submission queue: admission policy, worker
// pool, and the infra-error retry schedule (SPEC §5, §6, §13).
package queue

import (
	"fmt"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/store"
)

// Decision is the admission verdict for one incoming submission.
type Decision struct {
	Admit        bool
	RejectStatus string // StatusRejectedDeadline | StatusRejectedLimit when !Admit
	RejectReason string // human-readable, for push feedback (SPEC §6)
	Counts       bool   // false = does not consume an attempt or start a cooldown
}

// Admit applies the deadline/attempts/cooldown policy (SPEC §6 step 4).
// Pure over (task, history, now): the task metadata lives in the course repo,
// never in the DB, and history is the student's submissions for this task.
// Teacher rechecks bypass every limit and never count (SPEC §6).
func Admit(t config.ResolvedTask, history []store.Submission, now time.Time, teacherRecheck bool) Decision {
	if teacherRecheck {
		return Decision{Admit: true, Counts: false}
	}

	// Hard deadline (SPEC §9: past hard -> not graded).
	if hard := t.Deadline.Hard; hard != nil && now.After(*hard) {
		return Decision{
			RejectStatus: store.StatusRejectedDeadline,
			RejectReason: fmt.Sprintf("hard deadline passed (%s)", hard.Format("2006-01-02 15:04 -07")),
		}
	}

	// Attempts: infra_error stays in flight (the same submission retries, it
	// never double-consumes); rejected_* never ran and do not count.
	if max := t.Limits.MaxAttempts; max > 0 {
		consumed := 0
		for _, s := range history {
			if s.Counts && countsAsAttempt(s.Status) {
				consumed++
			}
		}
		if consumed >= max {
			return Decision{
				RejectStatus: store.StatusRejectedLimit,
				RejectReason: fmt.Sprintf("attempt limit reached (%d of %d)", consumed, max),
			}
		}
	}

	// Cooldown: measured from the most recent counting submission.
	if cd := t.Limits.Cooldown; cd > 0 {
		var last *time.Time
		for i := range history {
			s := &history[i]
			if s.Counts && countsAsAttempt(s.Status) && (last == nil || s.ReceivedAt.After(*last)) {
				last = &s.ReceivedAt
			}
		}
		if last != nil && now.Before(last.Add(cd)) {
			wait := last.Add(cd).Sub(now).Round(time.Second)
			return Decision{
				RejectStatus: store.StatusRejectedLimit,
				RejectReason: fmt.Sprintf("cooldown active, retry in %s", wait),
			}
		}
	}

	return Decision{Admit: true, Counts: true}
}

func countsAsAttempt(status string) bool {
	switch status {
	case store.StatusQueued, store.StatusRunning, store.StatusDone, store.StatusInfraError:
		return true
	default: // rejected_deadline, rejected_limit
		return false
	}
}
