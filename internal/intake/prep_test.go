package intake

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/gitserver"
	"github.com/ekalinin/anygrade/internal/hidden"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/store"
)

// newPrep builds a Prep over the fixture's repos, store and course holder.
func newPrep(t *testing.T, s *Server) *Prep {
	t.Helper()
	return &Prep{
		Repos: s.Repos, Users: s.DB, Course: s.Course,
		DataDir: s.Repos.DataDir,
		Log:     slog.New(slog.DiscardHandler),
	}
}

// setTaskHidden rewrites t1's task.yaml with a hidden_tests block, lands it in
// the mirror, and swaps the holder - the same path a teacher push takes.
func setTaskHidden(t *testing.T, s *Server, courseSrc, block string) {
	t.Helper()
	path := filepath.Join(courseSrc, "tasks", "t1", "task.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, []byte(block)...), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, courseSrc, "configure hidden tests")
	if err := s.Repos.EnsureCourse(t.Context(), courseSrc); err != nil {
		t.Fatal(err)
	}
	course, diags, err := LoadCourse(t.Context(), s.Repos.CourseDir())
	if err != nil || course == nil {
		t.Fatalf("LoadCourse: %v %v", err, diags)
	}
	s.Course.Set(course)
}

// TestPrepareMissingLocalHiddenTests: a configured local hidden-tests path
// that does not exist must fail the submission, not silently grade it against
// the open tests only. The failure is terminal - retrying cannot fix a wrong
// path - and the message never leaks it to the student.
func TestPrepareMissingLocalHiddenTests(t *testing.T) {
	s, _, courseSrc, user := newIntakeFixture(t)
	missing := filepath.Join(t.TempDir(), "not-there")
	setTaskHidden(t, s, courseSrc, "hidden_tests:\n  source: local\n  path: "+missing+"\n")

	studentDir := s.Repos.StudentDir("alice")
	head := git(t, studentDir, "rev-parse", "HEAD")

	_, err := newPrep(t, s).Prepare(t.Context(), store.Submission{
		ID: 1, UserID: user.ID, TaskID: "t1", CommitSHA: head,
	})
	if err == nil {
		t.Fatal("a missing hidden-tests path must fail the submission")
	}
	if !errors.Is(err, queue.ErrTerminal) {
		t.Errorf("error must be terminal, got %v", err)
	}
	if got := err.Error(); got != "hidden tests unavailable for this task" {
		t.Errorf("message %q leaks detail or differs from the git source's", got)
	}
}

// TestPrepareLocalHiddenTestsPresent: the happy path still wires the directory
// in as the hidden source.
func TestPrepareLocalHiddenTestsPresent(t *testing.T) {
	s, _, courseSrc, user := newIntakeFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hidden_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTaskHidden(t, s, courseSrc, "hidden_tests:\n  source: local\n  path: "+dir+"\n")

	studentDir := s.Repos.StudentDir("alice")
	head := git(t, studentDir, "rev-parse", "HEAD")

	prepared, err := newPrep(t, s).Prepare(t.Context(), store.Submission{
		ID: 1, UserID: user.ID, TaskID: "t1", CommitSHA: head,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Assembly.Hidden == nil {
		t.Fatal("hidden source must be wired when the directory exists")
	}
}

// TestPrepareNoteReportsIncludeAndMissingSolution: the worker note must cover
// both silent restorations - a tampered workspace.include path outside the
// task dir, and a solution file the student deleted (graded from the
// authoritative template, SPEC §6.1).
func TestPrepareNoteReportsIncludeAndMissingSolution(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)

	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module course.example/hacked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(work, "tasks", "t1", "main.go")); err != nil {
		t.Fatal(err)
	}
	_, head := push(t, work, "tamper with go.mod, drop the solution file")

	prepared, err := newPrep(t, s).Prepare(t.Context(), store.Submission{
		ID: 1, UserID: user.ID, TaskID: "t1", CommitSHA: head,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := []string{
		"modified outside solution_files (restored): go.mod",
		"solution file missing in the submitted commit (template used): tasks/t1/main.go",
	}
	for _, line := range want {
		if !strings.Contains(prepared.Note, line) {
			t.Errorf("worker note %q is missing %q", prepared.Note, line)
		}
	}
}

// TestPrepareUsesOneCourseSnapshot pins the invariant the single-snapshot read
// exists for: the authoritative tree is exported at the head of the very
// course version the task metadata came from. It cannot observe a swap landing
// mid-call - that is covered by construction, Prepare reads the holder once.
func TestPrepareUsesOneCourseSnapshot(t *testing.T) {
	s, _, courseSrc, user := newIntakeFixture(t)
	head := git(t, s.Repos.StudentDir("alice"), "rev-parse", "HEAD")

	// Move the course on so the holder's head is no longer the mirror's
	// original commit: a stale pairing would now be visible.
	setTaskHidden(t, s, courseSrc, "# teacher edit\n")

	prepared, err := newPrep(t, s).Prepare(t.Context(), store.Submission{
		ID: 1, UserID: user.ID, TaskID: "t1", CommitSHA: head,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	src, ok := prepared.Assembly.Authoritative.(gitserver.GitSource)
	if !ok {
		t.Fatalf("authoritative source is %T, want gitserver.GitSource", prepared.Assembly.Authoritative)
	}
	if want := s.Course.Get().Head; src.Commit != want {
		t.Errorf("authoritative commit = %s, want the snapshot head %s", src.Commit, want)
	}
}

// TestPrepareLocalHiddenTransientErrorIsRetryable: an unreadable hidden path is
// not a wrong one. Terminally failing every in-flight submission over a mount
// that blinked leaves the teacher re-running them by hand (SPEC §13).
func TestPrepareLocalHiddenTransientErrorIsRetryable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, _, courseSrc, user := newIntakeFixture(t)
	parent := t.TempDir()
	path := filepath.Join(parent, "hidden")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	setTaskHidden(t, s, courseSrc, "hidden_tests:\n  source: local\n  path: "+path+"\n")
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	head := git(t, s.Repos.StudentDir("alice"), "rev-parse", "HEAD")
	_, err := newPrep(t, s).Prepare(t.Context(), store.Submission{
		ID: 1, UserID: user.ID, TaskID: "t1", CommitSHA: head,
	})
	if err == nil {
		t.Fatal("an unreadable hidden-tests path must still fail the submission")
	}
	if errors.Is(err, queue.ErrTerminal) {
		t.Errorf("an unreadable path must stay retryable, got terminal: %v", err)
	}
	if strings.Contains(err.Error(), path) {
		t.Errorf("error text leaks the path: %s", err)
	}
}

// TestPrepareLocalHiddenOutsideAllowedRoots: with the operator's allowlist set,
// a course.yaml pointing anywhere else reads nothing - and says nothing about
// where it pointed.
func TestPrepareLocalHiddenOutsideAllowedRoots(t *testing.T) {
	s, _, courseSrc, user := newIntakeFixture(t)
	outside := t.TempDir()
	setTaskHidden(t, s, courseSrc, "hidden_tests:\n  source: local\n  path: "+outside+"\n")
	t.Setenv(hidden.LocalRootsEnv, t.TempDir())

	head := git(t, s.Repos.StudentDir("alice"), "rev-parse", "HEAD")
	_, err := newPrep(t, s).Prepare(t.Context(), store.Submission{
		ID: 1, UserID: user.ID, TaskID: "t1", CommitSHA: head,
	})
	if !errors.Is(err, queue.ErrTerminal) {
		t.Fatalf("a path outside the allowlist must be terminal, got %v", err)
	}
	if got := err.Error(); got != "hidden tests unavailable for this task" {
		t.Errorf("message %q leaks detail or differs from the git source's", got)
	}
}
