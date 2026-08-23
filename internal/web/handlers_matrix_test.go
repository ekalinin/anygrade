package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/store"
)

// historyHref is the (student, task) drill-down a matrix cell points at.
const historyHref = `href="/students/alice?task=t1#submissions"`

// TestMatrixRowCellDrillDown: the cell score links to the whole history of the
// pair, and the arrow still links straight to the latest submission. This is
// the partial matrixStream re-renders, so it covers the SSE path as well.
func TestMatrixRowCellDrillDown(t *testing.T) {
	data := matrixRowData{
		Row: gradebook.Row{
			User: store.User{Login: "alice", State: "active"},
			Cells: map[string]gradebook.Cell{
				"t1": {Status: gradebook.StatusPassed, Display: 10, LatestSubID: 7},
			},
		},
		Tasks: []gradebook.TaskCol{{ID: "t1", Name: "Task one", MaxScore: 10}},
	}
	html, err := renderPartial("en", "matrix-row", data)
	if err != nil {
		t.Fatalf("render matrix-row: %v", err)
	}
	if !strings.Contains(html, historyHref) {
		t.Errorf("cell does not link to the history page (%s):\n%s", historyHref, html)
	}
	if want := `href="/submissions/7"`; !strings.Contains(html, want) {
		t.Errorf("the arrow no longer links to the latest submission (%s):\n%s", want, html)
	}
}

// TestMatrixRowUntouchedCellHasNoLinks: a task the student never touched has
// nothing to drill into.
func TestMatrixRowUntouchedCellHasNoLinks(t *testing.T) {
	data := matrixRowData{
		Row: gradebook.Row{
			User:  store.User{Login: "alice", State: "active"},
			Cells: map[string]gradebook.Cell{"t1": {}},
		},
		Tasks: []gradebook.TaskCol{{ID: "t1", Name: "Task one", MaxScore: 10}},
	}
	html, err := renderPartial("en", "matrix-row", data)
	if err != nil {
		t.Fatalf("render matrix-row: %v", err)
	}
	if strings.Contains(html, historyHref) || strings.Contains(html, `href="/submissions/`) {
		t.Errorf("an untouched cell should have no drill-down links:\n%s", html)
	}
}

// TestMatrixPageCellDrillDown: the same links reach the teacher through the
// full page render, not just the partial.
func TestMatrixPageCellDrillDown(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, teacher := newSession(t, h, "teacher", "teacher")
	alice, _ := newSession(t, h, "alice", "student")
	id := enqueue(t, h, alice.ID, "t1", time.Now())

	rec := do(h, http.MethodGet, "/matrix", teacher)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /matrix: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, historyHref) {
		t.Errorf("matrix page cell does not link to the history page (%s):\n%s", historyHref, body)
	}
	if want := `href="/submissions/1"`; !strings.Contains(body, want) || id != 1 {
		t.Errorf("matrix page lost the latest-submission arrow (%s, id %d):\n%s", want, id, body)
	}
}

// TestMatrixCellShowsTheCorrection: an overridden cell shows both numbers, so
// a teacher scanning the matrix sees at a glance which marks are the machine's
// and which are their own. Replaces the old "*" glyph, which said an override
// existed without saying what it changed.
func TestMatrixCellShowsTheCorrection(t *testing.T) {
	computed, override := 72.0, 85.0
	data := matrixRowData{
		Row: gradebook.Row{
			User: store.User{Login: "alice", State: "active"},
			Cells: map[string]gradebook.Cell{
				"t1": {
					Status:   gradebook.StatusOverridden,
					Computed: &computed,
					Override: &override,
					Display:  85,
				},
			},
		},
		Tasks: []gradebook.TaskCol{{ID: "t1", Name: "Task one", MaxScore: 100}},
	}
	html, err := renderPartial("en", "matrix-row", data)
	if err != nil {
		t.Fatalf("render matrix-row: %v", err)
	}
	for _, want := range []string{`class="machine">72<`, `class="pen">85<`} {
		if !strings.Contains(html, want) {
			t.Errorf("matrix cell: missing %s:\n%s", want, html)
		}
	}
}

// TestMatrixCellWithoutOverrideHasNoPen: red is reserved for the human hand.
func TestMatrixCellWithoutOverrideHasNoPen(t *testing.T) {
	computed := 72.0
	data := matrixRowData{
		Row: gradebook.Row{
			User: store.User{Login: "alice", State: "active"},
			Cells: map[string]gradebook.Cell{
				"t1": {Status: gradebook.StatusPassed, Computed: &computed, Display: 72},
			},
		},
		Tasks: []gradebook.TaskCol{{ID: "t1", Name: "Task one", MaxScore: 100}},
	}
	html, err := renderPartial("en", "matrix-row", data)
	if err != nil {
		t.Fatalf("render matrix-row: %v", err)
	}
	if strings.Contains(html, `class="pen"`) || strings.Contains(html, `class="machine"`) {
		t.Errorf("matrix cell: no override, so no correction markup:\n%s", html)
	}
}

func TestFilterRows(t *testing.T) {
	tasks := []gradebook.TaskCol{{ID: "t1", MaxScore: 10}, {ID: "t2", MaxScore: 10}}
	rows := []gradebook.Row{
		{
			User: store.User{Login: "alice", DisplayName: "Alice A"},
			Cells: map[string]gradebook.Cell{
				"t1": {Status: gradebook.StatusPassed},
				"t2": {Status: ""}, // not started
			},
		},
		{
			User: store.User{Login: "bob", DisplayName: "Bob B"},
			Cells: map[string]gradebook.Cell{
				"t1": {Status: gradebook.StatusFailed},
				"t2": {Status: gradebook.StatusPassed},
			},
		},
	}

	t.Run("no filters", func(t *testing.T) {
		got := filterRows(rows, tasks, "", "", "")
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2", len(got))
		}
	})

	t.Run("q matches login", func(t *testing.T) {
		got := filterRows(rows, tasks, "ali", "", "")
		if len(got) != 1 || got[0].User.Login != "alice" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("q matches display name case-insensitively", func(t *testing.T) {
		got := filterRows(rows, tasks, "BOB B", "", "")
		if len(got) != 1 || got[0].User.Login != "bob" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("status alone matches any cell", func(t *testing.T) {
		got := filterRows(rows, tasks, "", "", gradebook.StatusPassed)
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2 (both have a passed cell)", len(got))
		}
	})

	t.Run("task+status matches only that task's cell", func(t *testing.T) {
		got := filterRows(rows, tasks, "", "t1", gradebook.StatusPassed)
		if len(got) != 1 || got[0].User.Login != "alice" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("task without status is a no-op", func(t *testing.T) {
		got := filterRows(rows, tasks, "", "t1", "")
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2 (task alone is a no-op)", len(got))
		}
	})

	t.Run("not started status matches the empty display status", func(t *testing.T) {
		got := filterRows(rows, tasks, "", "t2", gradebook.StatusNotStarted)
		if len(got) != 1 || got[0].User.Login != "alice" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("unknown task id is ignored", func(t *testing.T) {
		got := filterRows(rows, tasks, "", "bogus", gradebook.StatusPassed)
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2 (unknown task falls back to any-cell match)", len(got))
		}
	})
}
