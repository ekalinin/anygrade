// Package gradebook is the shared scoring read model (SPEC §9, §10): the
// students × tasks matrix with best|latest policy and teacher overrides.
// It is neutral ground - web pages, the CSV export route, and the CLI
// exporter all consume it, so cli never imports web.
package gradebook

import (
	"slices"
	"strings"

	"github.com/ekalinin/anygrade/internal/store"
)

// Display statuses beyond the raw store statuses (SPEC §10). "retrying",
// "error", and "canceled" refine infra_error for humans.
const (
	StatusNotStarted = "not started"
	StatusRetrying   = "retrying"
	StatusError      = "error"
	StatusCanceled   = "canceled"
	StatusRejected   = "rejected"
	StatusPassed     = "passed"
	StatusPartial    = "partial"
	StatusFailed     = "failed"
)

// TaskCol is one matrix column, in course order.
type TaskCol struct {
	ID       string
	Name     string
	MaxScore int
}

// Cell is one (student, task) intersection.
type Cell struct {
	Status      string   // display status; "" when the student never touched the task
	Computed    *float64 // best|latest over done submissions; nil if none
	Override    *float64 // teacher override; nil if none
	Display     float64  // Override if set, else Computed, else 0
	LatestSubID int64    // click-through target; 0 if none
}

// Row is one student's matrix row.
type Row struct {
	User  store.User
	Cells map[string]Cell // by task id
	Total float64         // sum of Display over all tasks
}

// Matrix is the full gradebook.
type Matrix struct {
	Tasks []TaskCol
	Rows  []Row // students only (teachers excluded), ordered by login
}

// Build assembles the matrix. Pure: submissions are grouped in Go (course
// scale makes one scan + O(n) grouping trivially cheap and testable).
func Build(users []store.User, tasks []TaskCol, subs []store.Submission,
	overrides []store.ScoreOverride, policy string) Matrix {

	byUserTask := map[int64]map[string][]store.Submission{}
	for _, s := range subs {
		if byUserTask[s.UserID] == nil {
			byUserTask[s.UserID] = map[string][]store.Submission{}
		}
		byUserTask[s.UserID][s.TaskID] = append(byUserTask[s.UserID][s.TaskID], s)
	}
	ovr := map[int64]map[string]float64{}
	for _, o := range overrides {
		if ovr[o.UserID] == nil {
			ovr[o.UserID] = map[string]float64{}
		}
		ovr[o.UserID][o.TaskID] = o.Score
	}

	m := Matrix{Tasks: tasks}
	for _, u := range users {
		if u.Role != "student" {
			continue
		}
		row := Row{User: u, Cells: make(map[string]Cell, len(tasks))}
		for _, t := range tasks {
			history := byUserTask[u.ID][t.ID]
			cell := Cell{
				Status:   DeriveStatus(history, t.MaxScore),
				Computed: DisplayScore(history, policy),
			}
			if cell.Status == StatusNotStarted {
				cell.Status = ""
			}
			if len(history) > 0 {
				cell.LatestSubID = history[len(history)-1].ID
			}
			if o, ok := ovr[u.ID][t.ID]; ok {
				cell.Override = &o
			}
			switch {
			case cell.Override != nil:
				cell.Display = *cell.Override
			case cell.Computed != nil:
				cell.Display = *cell.Computed
			}
			row.Total += cell.Display
			row.Cells[t.ID] = cell
		}
		m.Rows = append(m.Rows, row)
	}
	slices.SortFunc(m.Rows, func(a, b Row) int {
		return strings.Compare(a.User.Login, b.User.Login)
	})
	return m
}

// DeriveStatus maps a task's submission history (received_at ascending) to
// its display status (SPEC §10).
func DeriveStatus(history []store.Submission, taskScore int) string {
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
		switch {
		case last.CanceledAt != nil:
			return StatusCanceled
		case last.RetryAt == nil:
			return StatusError // terminal: surfaced in the teacher queue view
		default:
			return StatusRetrying
		}
	case store.StatusRejectedDeadline, store.StatusRejectedLimit:
		return StatusRejected
	}
	final := 0.0
	if last.FinalScore != nil {
		final = *last.FinalScore
	}
	switch {
	case final >= float64(taskScore):
		return StatusPassed
	case final > 0:
		return StatusPartial
	default:
		return StatusFailed
	}
}

// DisplayScore picks the shown score per the course scoring policy
// (SPEC §9: best|latest over done submissions).
func DisplayScore(history []store.Submission, policy string) *float64 {
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

// CountAttempts mirrors the queue policy's counting rule for display.
func CountAttempts(history []store.Submission) int {
	n := 0
	for _, s := range history {
		if s.Counts && s.Status != store.StatusRejectedDeadline && s.Status != store.StatusRejectedLimit {
			n++
		}
	}
	return n
}
