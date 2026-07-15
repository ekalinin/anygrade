package web

import (
	"fmt"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/store"
)

// TaskView is one dashboard row / task-page header. Status/score derivation
// lives in gradebook (shared with the matrix and CSV export).
type TaskView struct {
	Task     config.ResolvedTask
	Status   string
	Score    *float64 // displayed per scoring policy; nil = nothing graded yet
	Attempts int      // counting submissions used
	Latest   *store.Submission
}

// buildTaskView assembles one row from a task and its history.
func buildTaskView(t config.ResolvedTask, history []store.Submission, policy string) TaskView {
	v := TaskView{
		Task:     t,
		Status:   gradebook.DeriveStatus(history, t.Score),
		Score:    gradebook.DisplayScore(history, policy),
		Attempts: gradebook.CountAttempts(history),
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

// taskCols maps course tasks to gradebook columns (matrix, CSV, leaderboard).
func taskCols(course *intake.Course) []gradebook.TaskCol {
	cols := make([]gradebook.TaskCol, len(course.Resolved.Tasks))
	for i, t := range course.Resolved.Tasks {
		cols[i] = gradebook.TaskCol{ID: t.ID, Name: t.Name, MaxScore: t.Score}
	}
	return cols
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
