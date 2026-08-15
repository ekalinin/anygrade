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
	RejectReason string // human-readable, for push feedback and the stored note (SPEC §6)
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

	if max := t.Limits.MaxAttempts; max > 0 {
		consumed := CountAttempts(history)
		if consumed >= max {
			return Decision{
				RejectStatus: store.StatusRejectedLimit,
				RejectReason: fmt.Sprintf("attempt limit reached (%d of %d)", consumed, max),
			}
		}
	}

	// Cooldown: measured from the most recent submission that consumed an
	// attempt. Same rule as the limit above, so a submission the student was
	// never charged for does not hold them back either.
	if cd := t.Limits.Cooldown; cd > 0 {
		last := lastAttemptAt(history)
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

// countsAsAttempt reports whether a submission holds one of the task's
// attempt slots. Queued and running rows are in flight: they hold a slot so a
// burst of pushes cannot overshoot max_attempts before anything finishes. An
// infra_error holds its slot only while a retry is scheduled - that is the
// same submission coming back, and releasing the slot would let it be paid
// for twice. Once it is terminal (retries exhausted, or a teacher cancel,
// which also clears Counts) the submission never ran, so per SPEC §13 it
// consumes no attempt and the student gets the slot back.
func countsAsAttempt(s store.Submission) bool {
	if !s.Counts { // teacher recheck, or a canceled row
		return false
	}
	switch s.Status {
	case store.StatusQueued, store.StatusRunning, store.StatusDone:
		return true
	case store.StatusInfraError:
		return s.RetryAt != nil
	default: // rejected_deadline, rejected_limit: never ran
		return false
	}
}

// CountAttempts is how many of the task's attempts a history has consumed:
// the admission rule above, exported so the UI displays the same number the
// policy enforces.
func CountAttempts(history []store.Submission) int {
	n := 0
	for _, s := range history {
		if countsAsAttempt(s) {
			n++
		}
	}
	return n
}

// lastAttemptAt is the receipt time of the most recent attempt-consuming
// submission (the cooldown anchor); nil when there is none.
func lastAttemptAt(history []store.Submission) *time.Time {
	var last *time.Time
	for i := range history {
		s := &history[i]
		if countsAsAttempt(*s) && (last == nil || s.ReceivedAt.After(*last)) {
			last = &s.ReceivedAt
		}
	}
	return last
}

// Quota derives the task-page display numbers from the same rules as Admit
// (single source of truth for the attempt/cooldown math): attempts left
// (unlimited when max_attempts is 0) and when the active cooldown ends.
func Quota(t config.ResolvedTask, history []store.Submission, now time.Time) (attemptsLeft int, unlimited bool, cooldownUntil *time.Time) {
	consumed := CountAttempts(history)
	last := lastAttemptAt(history)
	if cd := t.Limits.Cooldown; cd > 0 && last != nil {
		if until := last.Add(cd); now.Before(until) {
			cooldownUntil = &until
		}
	}
	if t.Limits.MaxAttempts == 0 {
		return 0, true, cooldownUntil
	}
	return max(0, t.Limits.MaxAttempts-consumed), false, cooldownUntil
}
