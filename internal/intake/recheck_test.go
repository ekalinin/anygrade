package intake

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/store"
)

// solveT1 records one counting submission for alice/t1 by pushing a change to
// the task's solution file, and returns the pushed commit.
func solveT1(t *testing.T, s *Server, work string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, "tasks", "t1", "main.go"),
		[]byte("package main // v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, head := push(t, work, "solve t1")
	if out := joined(s.dispatch(t.Context(), postReceive(old, head))); !strings.Contains(out, "submission #1 queued") {
		t.Fatalf("setup push: %s", out)
	}
	return head
}

// blockPins makes every refs/anygrade/submissions/<id> write fail: a ref at
// refs/anygrade/submissions is a D/F conflict with everything below it.
func blockPins(t *testing.T, s *Server) {
	t.Helper()
	dir := s.Repos.StudentDir("alice")
	git(t, dir, "update-ref", "refs/anygrade/submissions", git(t, dir, "rev-parse", "HEAD"))
}

// TestRecheckPinsRecheckedCommit is the happy path: no warning, and the
// rechecked commit is pinned like a push pins it (SPEC §6 step 7).
func TestRecheckPinsRecheckedCommit(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)
	head := solveT1(t, s, work)

	sub, d, warn, err := s.Recheck(t.Context(), user.ID, "t1")
	if err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	if !d.Admit {
		t.Fatalf("recheck rejected: %s", d.RejectReason)
	}
	if warn != "" {
		t.Errorf("warn = %q, want none", warn)
	}
	pin := git(t, s.Repos.StudentDir("alice"), "rev-parse", "refs/anygrade/submissions/2")
	if pin != head {
		t.Errorf("submission #%d pinned at %s, want %s", sub.ID, pin, head)
	}
}

// TestRecheckWarnsOnUnpinnedCommit: a failed pin is a warning, not an error.
// The submission is already queued by then, so it must stand.
func TestRecheckWarnsOnUnpinnedCommit(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)
	blockPins(t, s)
	solveT1(t, s, work)

	sub, d, warn, err := s.Recheck(t.Context(), user.ID, "t1")
	if err != nil {
		t.Fatalf("Recheck must not fail on an unpinned commit: %v", err)
	}
	if !d.Admit {
		t.Fatalf("recheck rejected: %s", d.RejectReason)
	}
	if warn != WarnCommitNotPinned {
		t.Errorf("warn = %q, want %q", warn, WarnCommitNotPinned)
	}
	got, _, err := s.DB.GetSubmission(t.Context(), sub.ID)
	if err != nil {
		t.Fatalf("submission #%d not recorded: %v", sub.ID, err)
	}
	if got.Status != store.StatusQueued {
		t.Errorf("status = %q, want %q", got.Status, store.StatusQueued)
	}
}

// TestTeacherRecheckWarnsOnUnpinnedCommit: the teacher entry point reports the
// same warning to its caller (the queue and student views render it).
func TestTeacherRecheckWarnsOnUnpinnedCommit(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)
	blockPins(t, s)
	solveT1(t, s, work)

	teacher, err := s.DB.CreateUser(t.Context(), "tina", "Tina", "teacher")
	if err != nil {
		t.Fatal(err)
	}
	sub, warn, err := s.TeacherRecheck(t.Context(), teacher, user.ID, "t1")
	if err != nil {
		t.Fatalf("TeacherRecheck must not fail on an unpinned commit: %v", err)
	}
	if warn != WarnCommitNotPinned {
		t.Errorf("warn = %q, want %q", warn, WarnCommitNotPinned)
	}
	got, _, err := s.DB.GetSubmission(t.Context(), sub.ID)
	if err != nil {
		t.Fatalf("submission #%d not recorded: %v", sub.ID, err)
	}
	if got.Counts {
		t.Error("a teacher recheck must not consume an attempt")
	}
}
