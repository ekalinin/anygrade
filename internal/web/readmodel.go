package web

import (
	"fmt"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/store"
)

// Task display statuses (SPEC §10). "retrying"/"error" refine infra_error:
// students should not see a permanently-"queued" task when the queue gave up.
const (
	StatusNotStarted = "not started"
	StatusRetrying   = "retrying"
	StatusError      = "error"
)

// TaskView is one dashboard row / task-page header.
type TaskView struct {
	Task     config.ResolvedTask
	Status   string
	Score    *float64 // displayed per scoring policy; nil = nothing graded yet
	Attempts int      // counting submissions used
	Latest   *store.Submission
}

// deriveStatus maps a task's submission history to its display status.
// history is ordered by received_at ascending (store list order).
func deriveStatus(history []store.Submission, taskScore int) string {
	if len(history) == 0 {
		return StatusNotStarted
	}
	last := history[len(history)-1]
	switch last.Status {
	case store.StatusQueued:
		return store.StatusQueued
	case store.StatusRunning:
		return store.StatusRunning
	case store.StatusInfraError:
		if last.RetryAt == nil {
			return StatusError // terminal: surfaced to the teacher (M6 queue view)
		}
		return StatusRetrying
	case store.StatusRejectedDeadline, store.StatusRejectedLimit:
		return "rejected"
	}
	// done: passed / partial / failed by the final score.
	final := 0.0
	if last.FinalScore != nil {
		final = *last.FinalScore
	}
	switch {
	case final >= float64(taskScore):
		return "passed"
	case final > 0:
		return "partial"
	default:
		return "failed"
	}
}

// displayScore picks the shown score per the course scoring policy
// (SPEC §9: best|latest over done submissions).
func displayScore(history []store.Submission, policy string) *float64 {
	var score *float64
	for i := range history {
		s := &history[i]
		if s.Status != store.StatusDone || s.FinalScore == nil {
			continue
		}
		if policy == "latest" || score == nil || *s.FinalScore > *score {
			score = s.FinalScore
		}
	}
	return score
}

// countAttempts mirrors the queue policy's counting rule for display.
func countAttempts(history []store.Submission) int {
	n := 0
	for _, s := range history {
		if s.Counts && s.Status != store.StatusRejectedDeadline && s.Status != store.StatusRejectedLimit {
			n++
		}
	}
	return n
}

// buildTaskView assembles one row from a task and its history.
func buildTaskView(t config.ResolvedTask, history []store.Submission, policy string) TaskView {
	v := TaskView{
		Task:     t,
		Status:   deriveStatus(history, t.Score),
		Score:    displayScore(history, policy),
		Attempts: countAttempts(history),
	}
	if len(history) > 0 {
		v.Latest = &history[len(history)-1]
	}
	return v
}

// buildDashboard groups one user's submissions by task, in course order.
// Submissions of deleted tasks are omitted (historical results stay
// reachable via their submission pages, SPEC §13).
func buildDashboard(course *intake.Course, subs []store.Submission) []TaskView {
	byTask := map[string][]store.Submission{}
	for _, s := range subs {
		byTask[s.TaskID] = append(byTask[s.TaskID], s)
	}
	policy := course.Resolved.Course.ScoringPolicy
	views := make([]TaskView, 0, len(course.Resolved.Tasks))
	for _, t := range course.Resolved.Tasks {
		views = append(views, buildTaskView(t, byTask[t.ID], policy))
	}
	return views
}

// countdown renders a compact "in 3d 4h" / "42m overdue" string.
func countdown(t time.Time, now time.Time) string {
	d := t.Sub(now).Round(time.Minute)
	overdue := d < 0
	if overdue {
		d = -d
	}
	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	mins := int(d % time.Hour / time.Minute)
	var s string
	switch {
	case days > 0:
		s = fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		s = fmt.Sprintf("%dh %dm", hours, mins)
	default:
		s = fmt.Sprintf("%dm", mins)
	}
	if overdue {
		return s + " overdue"
	}
	return "in " + s
}
