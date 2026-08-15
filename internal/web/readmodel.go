package web

import (
	"context"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/i18n"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/store"
)

// TaskView is one dashboard row / task-page header. Status/score derivation
// lives in gradebook (shared with the matrix and CSV export); the attempt
// count comes from queue, which owns the admission rule.
type TaskView struct {
	Task     config.ResolvedTask
	Status   string
	Score    *float64             // computed per scoring policy; nil = nothing graded yet
	Override *store.ScoreOverride // teacher's manual score, nil if none (SPEC §9)
	Attempts int                  // counting submissions used
	Latest   *store.Submission
}

// Display is the score to show: an override wins over the computed score,
// the same rule gradebook.Cell applies, so a student's pages and the matrix
// can never disagree (SPEC §9).
func (v TaskView) Display() *float64 {
	if v.Override != nil {
		return &v.Override.Score
	}
	return v.Score
}

// buildTaskView assembles one row from a task and its history.
func buildTaskView(t config.ResolvedTask, history []store.Submission, policy string,
	override *store.ScoreOverride) TaskView {

	v := TaskView{
		Task:     t,
		Status:   gradebook.DeriveStatus(history, t.Score),
		Score:    gradebook.DisplayScore(history, policy),
		Override: override,
		Attempts: queue.CountAttempts(history),
	}
	if len(history) > 0 {
		v.Latest = &history[len(history)-1]
	}
	return v
}

// buildDashboard groups one user's submissions by task, in course order.
// Submissions of deleted tasks are omitted (historical results stay
// reachable via their submission pages, SPEC §13).
func buildDashboard(course *intake.Course, subs []store.Submission,
	overrides map[string]*store.ScoreOverride) []TaskView {

	byTask := map[string][]store.Submission{}
	for _, s := range subs {
		byTask[s.TaskID] = append(byTask[s.TaskID], s)
	}
	policy := course.Resolved.Course.ScoringPolicy
	views := make([]TaskView, 0, len(course.Resolved.Tasks))
	for _, t := range course.Resolved.Tasks {
		views = append(views, buildTaskView(t, byTask[t.ID], policy, overrides[t.ID]))
	}
	return views
}

// userOverrides indexes one student's manual overrides by task id. One read
// per page: the override table is course-sized, so filtering it here beats a
// query per task.
func (h *Handler) userOverrides(ctx context.Context, userID int64) (map[string]*store.ScoreOverride, error) {
	all, err := h.DB.ListScoreOverrides(ctx)
	if err != nil {
		return nil, err
	}
	m := map[string]*store.ScoreOverride{}
	for i := range all {
		if all[i].UserID == userID {
			m[all[i].TaskID] = &all[i]
		}
	}
	return m, nil
}

// taskOverride reads the manual override for one (student, task); nil when
// none is set.
func (h *Handler) taskOverride(ctx context.Context, userID int64, taskID string) (*store.ScoreOverride, error) {
	o, ok, err := h.DB.GetScoreOverride(ctx, userID, taskID)
	if err != nil || !ok {
		return nil, err
	}
	return &o, nil
}

// taskCols maps course tasks to gradebook columns (matrix, CSV, leaderboard).
func taskCols(course *intake.Course) []gradebook.TaskCol {
	cols := make([]gradebook.TaskCol, len(course.Resolved.Tasks))
	for i, t := range course.Resolved.Tasks {
		cols[i] = gradebook.TaskCol{ID: t.ID, Name: t.Name, MaxScore: t.Score}
	}
	return cols
}

// countdown renders a compact, localized "in 3d 4h" / "42m overdue" string.
func countdown(t time.Time, now time.Time, tr i18n.Translator) string {
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
		s = tr.T("countdown.dh", days, hours)
	case hours > 0:
		s = tr.T("countdown.hm", hours, mins)
	default:
		s = tr.T("countdown.m", mins)
	}
	if overdue {
		return tr.T("countdown.overdue", s)
	}
	return tr.T("countdown.in", s)
}
