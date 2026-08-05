package intake

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/gitserver"
	"github.com/ekalinin/anygrade/internal/hookproto"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitAll(t *testing.T, dir, msg string) string {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

// newIntakeFixture builds a real course (local runner, one open task, one
// past-hard-deadline task), a mirror, a provisioned student repo, a store
// with user alice, and the intake server over them.
func newIntakeFixture(t *testing.T) (s *Server, work string, courseSrc string, user store.User) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	courseSrc = t.TempDir()
	git(t, courseSrc, "init", "-b", "main")
	files := map[string]string{
		"course.yaml": "name: Intake course\nregistration:\n  mode: invite\n" +
			"defaults:\n  runner:\n    type: local\n    timeout: 1m\n",
		"tasks/t1/task.yaml": "name: T1\nscore: 100\nsolution_files:\n  - main.go\n" +
			"checks:\n  - name: run\n    weight: 1\n    run: \"true\"\n",
		"tasks/t1/main.go": "package main\n",
		"tasks/t2/task.yaml": "name: T2\nscore: 10\nsolution_files:\n  - main.go\n" +
			"deadline:\n  hard: 2020-01-01T00:00:00+03:00\n" +
			"checks:\n  - name: run\n    weight: 1\n    run: \"true\"\n",
		"tasks/t2/main.go": "package main\n",
	}
	for p, content := range files {
		abs := filepath.Join(courseSrc, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitAll(t, courseSrc, "course init")

	dataDir := t.TempDir()
	repos := &gitserver.RepoManager{DataDir: dataDir, HookBin: "/usr/bin/true"}
	if err := repos.EnsureCourse(t.Context(), courseSrc); err != nil {
		t.Fatal(err)
	}
	studentDir, err := repos.EnsureStudent(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(t.Context(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, err = db.CreateUser(t.Context(), "alice", "Alice", "student")
	if err != nil {
		t.Fatal(err)
	}

	course, diags, err := LoadCourse(t.Context(), repos.CourseDir())
	if err != nil || course == nil {
		t.Fatalf("LoadCourse: %v %v", err, diags)
	}
	holder := &Holder{}
	holder.Set(course)

	s = &Server{
		DB:    db,
		Queue: &queue.Queue{Store: db, Prep: &Prep{Repos: repos, Users: db, Course: holder, DataDir: dataDir}},
		Repos: repos, Course: holder,
		BaseURL: "http://grade.example",
	}

	work = filepath.Join(t.TempDir(), "wc")
	git(t, ".", "clone", studentDir, work)
	return s, work, courseSrc, user
}

// push commits the work tree and pushes to the student bare repo, returning
// (old, head) for the hook message.
func push(t *testing.T, work, msg string) (old, head string) {
	t.Helper()
	old = git(t, work, "rev-parse", "origin/main")
	head = commitAll(t, work, msg)
	git(t, work, "push", "origin", "main")
	return old, head
}

func postReceive(old, new string) hookproto.Request {
	return hookproto.Request{
		Kind: hookproto.KindPostReceive, Repo: "alice", Actor: "alice", Role: "student",
		Updates: []hookproto.RefUpdate{{Old: old, New: new, Ref: "refs/heads/main"}},
	}
}

func joined(r hookproto.Response) string { return strings.Join(r.Lines, "\n") }

// TestProcessPushFullFlow drives the SPEC §6 pipeline: detection, admission,
// rejection, hidden refs, baseline advancement, recheck markers.
func TestProcessPushFullFlow(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)
	studentDir := s.Repos.StudentDir("alice")

	// Push touching both tasks: t1 queued, t2 past its hard deadline.
	if err := os.WriteFile(filepath.Join(work, "tasks", "t1", "main.go"), []byte("package main // v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "tasks", "t2", "main.go"), []byte("package main // v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, new1 := push(t, work, "solve both")
	resp := s.dispatch(t.Context(), postReceive(old, new1))
	out := joined(resp)
	if resp.ExitCode != 0 || !strings.Contains(out, "2 task(s) detected") {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if !strings.Contains(out, "submission #1 queued") || !strings.Contains(out, "http://grade.example/submissions/1") {
		t.Errorf("missing queued line: %s", out)
	}
	if !strings.Contains(out, "rejected: ") {
		t.Errorf("missing t2 rejection: %s", out)
	}

	subs, err := s.DB.ListByUserTask(t.Context(), user.ID, "t1")
	if err != nil || len(subs) != 1 || subs[0].Status != store.StatusQueued || subs[0].CommitSHA != new1 {
		t.Fatalf("t1 submissions: %+v err=%v", subs, err)
	}
	rejected, err := s.DB.ListByUserTask(t.Context(), user.ID, "t2")
	if err != nil || len(rejected) != 1 || rejected[0].Status != store.StatusRejectedDeadline {
		t.Fatalf("t2 submissions: %+v err=%v", rejected, err)
	}
	if ref := git(t, studentDir, "rev-parse", "refs/anygrade/submissions/1"); ref != new1 {
		t.Errorf("submission ref = %s, want %s", ref, new1)
	}
	if base := git(t, studentDir, "rev-parse", "refs/anygrade/baseline"); base != new1 {
		t.Errorf("baseline = %s, want %s", base, new1)
	}

	// Non-task push: nothing new, baseline still advances.
	if err := os.WriteFile(filepath.Join(work, "notes.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, new2 := push(t, work, "notes")
	resp = s.dispatch(t.Context(), postReceive(old, new2))
	if !strings.Contains(joined(resp), "no tasks changed") {
		t.Errorf("want no-tasks-changed, got %v", resp.Lines)
	}
	if base := git(t, studentDir, "rev-parse", "refs/anygrade/baseline"); base != new2 {
		t.Errorf("baseline = %s, want %s", base, new2)
	}

	// Empty commit with an explicit recheck marker re-queues t1.
	old, new3 := push(t, work, "please [recheck t1]")
	resp = s.dispatch(t.Context(), postReceive(old, new3))
	if !strings.Contains(joined(resp), "submission #3 queued") {
		t.Errorf("recheck not queued: %v", resp.Lines)
	}
	subs, _ = s.DB.ListByUserTask(t.Context(), user.ID, "t1")
	if len(subs) != 2 || *subs[1].AttemptNo != 2 {
		t.Fatalf("after recheck: %+v", subs)
	}

	// Non-default branch: stored, not graded.
	resp = s.dispatch(t.Context(), hookproto.Request{
		Kind: hookproto.KindPostReceive, Repo: "alice",
		Updates: []hookproto.RefUpdate{{Old: strings.Repeat("0", 40), New: new3, Ref: "refs/heads/feature"}},
	})
	if !strings.Contains(joined(resp), "only main is graded") {
		t.Errorf("branch line missing: %v", resp.Lines)
	}
}

// failingAdmit makes the admission write fail so gradePush takes its error
// path with the rest of the store intact. It wraps the one call the push path
// makes to record a submission - the read, the verdict and the insert now all
// happen inside AdmitSubmission.
type failingAdmit struct {
	store.Store
	fail bool
}

func (f *failingAdmit) AdmitSubmission(ctx context.Context, ns store.NewSubmission,
	decide func(history []store.Submission) store.Admission) (store.Submission, error) {
	if f.fail {
		return store.Submission{}, errors.New("admission unavailable")
	}
	return f.Store.AdmitSubmission(ctx, ns, decide)
}

// TestGradePushKeepsBaselineOnError: a task that fails to record must not move
// the baseline, so the next push re-detects the change instead of losing it.
func TestGradePushKeepsBaselineOnError(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)
	studentDir := s.Repos.StudentDir("alice")
	before := git(t, studentDir, "rev-parse", "refs/anygrade/baseline")

	// Both handles have to be swapped: gradePush reads through Server.DB and
	// records through the queue's own store reference.
	failing := &failingAdmit{Store: s.DB, fail: true}
	s.DB = failing
	s.Queue.Store = failing

	if err := os.WriteFile(filepath.Join(work, "tasks", "t1", "main.go"), []byte("package main // v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, head := push(t, work, "solve t1")
	out := joined(s.dispatch(t.Context(), postReceive(old, head)))
	if !strings.Contains(out, "error: admission unavailable") || !strings.Contains(out, "baseline kept") {
		t.Fatalf("want the task error and the kept-baseline note: %s", out)
	}
	if base := git(t, studentDir, "rev-parse", "refs/anygrade/baseline"); base != before {
		t.Fatalf("baseline moved to %s, want %s", base, before)
	}

	// The store recovers: the same commit is re-detected and graded.
	failing.fail = false
	out = joined(s.dispatch(t.Context(), postReceive(old, head)))
	if !strings.Contains(out, "submission #1 queued") {
		t.Fatalf("change lost after recovery: %s", out)
	}
	subs, err := s.DB.ListByUserTask(t.Context(), user.ID, "t1")
	if err != nil || len(subs) != 1 || subs[0].CommitSHA != head {
		t.Fatalf("t1 submissions: %+v err=%v", subs, err)
	}
	if base := git(t, studentDir, "rev-parse", "refs/anygrade/baseline"); base != head {
		t.Fatalf("baseline = %s, want %s", base, head)
	}
}

// TestGradePushReportsUnpinnedCommit: pinning is best-effort, but its failure
// must be visible instead of silently dropping the audit guarantee.
func TestGradePushReportsUnpinnedCommit(t *testing.T) {
	s, work, _, _ := newIntakeFixture(t)
	studentDir := s.Repos.StudentDir("alice")

	// refs/anygrade/submissions as a ref of its own blocks every
	// refs/anygrade/submissions/<id> below it (git's D/F conflict).
	git(t, studentDir, "update-ref", "refs/anygrade/submissions",
		git(t, studentDir, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(work, "tasks", "t1", "main.go"), []byte("package main // v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, head := push(t, work, "solve t1")
	out := joined(s.dispatch(t.Context(), postReceive(old, head)))
	if !strings.Contains(out, "submission #1 queued") {
		t.Fatalf("submission must still be queued: %s", out)
	}
	if !strings.Contains(out, "commit not pinned") {
		t.Fatalf("want the unpinned warning: %s", out)
	}
	// A failed pin is not a processing failure: the baseline still advances.
	if base := git(t, studentDir, "rev-parse", "refs/anygrade/baseline"); base != head {
		t.Fatalf("baseline = %s, want %s", base, head)
	}
}

// recordBothTasks pushes a change to t1 and t2 so both have a submission
// recorded (t1 queued, t2 rejected past its hard deadline), and returns the
// commit the baseline pointed at before that push.
func recordBothTasks(t *testing.T, s *Server, work string) (before string) {
	t.Helper()
	for _, id := range []string{"t1", "t2"} {
		if err := os.WriteFile(filepath.Join(work, "tasks", id, "main.go"),
			[]byte("package main // v2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before, head := push(t, work, "solve both")
	if out := joined(s.dispatch(t.Context(), postReceive(before, head))); !strings.Contains(out, "2 task(s) detected") {
		t.Fatalf("setup push: %s", out)
	}
	return before
}

// rewindBaseline drops refs/anygrade/baseline back to an earlier commit: the
// state a push leaves behind when it keeps the ref after a task fails to
// record, and the state two concurrent hooks can leave behind on their own.
func rewindBaseline(t *testing.T, s *Server, to string) {
	t.Helper()
	git(t, s.Repos.StudentDir("alice"), "update-ref", "refs/anygrade/baseline", to)
}

// TestGradePushSkipsAlreadyRecordedTask: a stale baseline re-detects every
// task in the skipped range, but a task whose content has not moved since its
// own last submission must not be graded - and charged - a second time.
func TestGradePushSkipsAlreadyRecordedTask(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)
	rewindBaseline(t, s, recordBothTasks(t, s, work))

	if err := os.WriteFile(filepath.Join(work, "notes.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, head := push(t, work, "notes")
	out := joined(s.dispatch(t.Context(), postReceive(old, head)))

	if !strings.Contains(out, "2 task(s) already graded at this content, skipped") {
		t.Fatalf("want the skipped summary, got: %s", out)
	}
	// t2's row is a rejection: any recorded row means the content was seen.
	for _, id := range []string{"t1", "t2"} {
		subs, err := s.DB.ListByUserTask(t.Context(), user.ID, id)
		if err != nil || len(subs) != 1 {
			t.Fatalf("%s: %d submissions, want 1 - re-detection charged another: %v", id, len(subs), err)
		}
	}
	if base := git(t, s.Repos.StudentDir("alice"), "rev-parse", "refs/anygrade/baseline"); base != head {
		t.Errorf("baseline = %s, want %s", base, head)
	}
}

// TestGradePushRegradesChangedTaskAfterRewind guards the other side of the
// filter: a task that really did move is still graded.
func TestGradePushRegradesChangedTaskAfterRewind(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)
	rewindBaseline(t, s, recordBothTasks(t, s, work))

	if err := os.WriteFile(filepath.Join(work, "tasks", "t1", "main.go"),
		[]byte("package main // v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, head := push(t, work, "improve t1")
	out := joined(s.dispatch(t.Context(), postReceive(old, head)))

	if !strings.Contains(out, "1 task(s) detected") {
		t.Fatalf("t1 changed and must be detected: %s", out)
	}
	subs, err := s.DB.ListByUserTask(t.Context(), user.ID, "t1")
	if err != nil || len(subs) != 2 || subs[1].CommitSHA != head {
		t.Fatalf("t1 submissions: %+v err=%v", subs, err)
	}
	if !strings.Contains(out, "1 task(s) already graded") {
		t.Errorf("t2 did not move and must still be skipped: %s", out)
	}
}

// TestGradePushRecheckMarkerBypassesSkip: an explicit [recheck t1] is the
// student asking for a re-run, so unchanged content is no reason to drop it -
// and the task must not be counted as skipped either.
func TestGradePushRecheckMarkerBypassesSkip(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)
	rewindBaseline(t, s, recordBothTasks(t, s, work))

	old, head := push(t, work, "please [recheck t1]")
	out := joined(s.dispatch(t.Context(), postReceive(old, head)))

	subs, err := s.DB.ListByUserTask(t.Context(), user.ID, "t1")
	if err != nil || len(subs) != 2 {
		t.Fatalf("t1 has %d submissions, want 2 - the marker must bypass the filter: %v", len(subs), err)
	}
	if !strings.Contains(out, "1 task(s) already graded") {
		t.Errorf("only t2 stayed skipped, the count must exclude t1: %s", out)
	}
}

// TestGradePushSkipsRecordedTasksWithoutBaseline covers the self-heal path
// that exists for repos provisioned before the baseline seed: with no
// baseline ref the diff runs against the empty tree and re-detects every task
// that has files, including ones already graded.
func TestGradePushSkipsRecordedTasksWithoutBaseline(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)
	recordBothTasks(t, s, work)
	git(t, s.Repos.StudentDir("alice"), "update-ref", "-d", "refs/anygrade/baseline")

	if err := os.WriteFile(filepath.Join(work, "notes.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, head := push(t, work, "notes")
	out := joined(s.dispatch(t.Context(), postReceive(old, head)))

	if !strings.Contains(out, "2 task(s) already graded at this content, skipped") {
		t.Fatalf("want the skipped summary, got: %s", out)
	}
	for _, id := range []string{"t1", "t2"} {
		if subs, _ := s.DB.ListByUserTask(t.Context(), user.ID, id); len(subs) != 1 {
			t.Fatalf("%s: %d submissions, want 1", id, len(subs))
		}
	}
}

// TestPreReceiveReservedRefs: pushes to refs/anygrade/* are rejected.
func TestPreReceiveReservedRefs(t *testing.T) {
	resp := preReceive(hookproto.Request{
		Kind: hookproto.KindPreReceive, Repo: "alice",
		Updates: []hookproto.RefUpdate{{Old: strings.Repeat("0", 40), New: "abc", Ref: "refs/anygrade/baseline"}},
	})
	if resp.ExitCode != 1 {
		t.Fatalf("reserved ref must be rejected: %+v", resp)
	}
	resp = preReceive(hookproto.Request{
		Updates: []hookproto.RefUpdate{{Ref: "refs/heads/main"}},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("normal ref must pass: %+v", resp)
	}
}

// TestValidateCourseRejectsBrokenMetadata: a teacher push with invalid
// metadata is rejected with the diagnostics; a good one passes and reloads.
func TestValidateCourse(t *testing.T) {
	s, _, courseSrc, _ := newIntakeFixture(t)

	// Break metadata on a side branch and land it in the mirror (simulates
	// the pushed-but-unaccepted commit; quarantine env is a passthrough).
	git(t, courseSrc, "checkout", "-b", "broken")
	if err := os.WriteFile(filepath.Join(courseSrc, "tasks", "t1", "task.yaml"),
		[]byte("name: T1\nscore: -5\nchecks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	badSHA := commitAll(t, courseSrc, "break metadata")
	git(t, courseSrc, "checkout", "main")
	git(t, s.Repos.CourseDir(), "fetch", courseSrc, "refs/heads/broken:refs/heads/broken")

	resp := s.validateCourse(t.Context(), hookproto.Request{
		Kind: hookproto.KindValidateCourse, Repo: "course",
		Updates: []hookproto.RefUpdate{{Old: "x", New: badSHA, Ref: "refs/heads/main"}},
	})
	if resp.ExitCode != 1 || !strings.Contains(joined(resp), "invalid") {
		t.Fatalf("broken metadata must be rejected: %+v", resp)
	}

	// A commit with valid metadata passes.
	goodSHA := git(t, s.Repos.CourseDir(), "rev-parse", "HEAD")
	resp = s.validateCourse(t.Context(), hookproto.Request{
		Kind: hookproto.KindValidateCourse, Repo: "course",
		Updates: []hookproto.RefUpdate{{Old: "x", New: goodSHA, Ref: "refs/heads/main"}},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("valid metadata must pass: %+v", resp)
	}

	// courseUpdated reloads the holder.
	before := s.Course.Get()
	resp = s.courseUpdated(t.Context())
	if !strings.Contains(joined(resp), "reloaded") {
		t.Fatalf("reload response: %+v", resp)
	}
	if s.Course.Get() == before {
		t.Error("holder must be swapped on reload")
	}
}
