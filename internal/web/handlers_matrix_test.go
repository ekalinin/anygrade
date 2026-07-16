package web

import (
	"testing"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/store"
)

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
