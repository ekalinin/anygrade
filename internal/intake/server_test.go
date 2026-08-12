package intake

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
			"defaults:\n  runner:\n    type: local\n    timeout: 1m\n" +
			"  workspace:\n    include:\n      - go.mod\n",
		"go.mod": "module course.example/intake\n",
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

// stallingAdmit parks the push that records commit `commit` at its admission
// write until release is closed, so one hook handler can be held mid-push
// while another runs to completion. It blocks before delegating, never inside
// the real call: the store keeps a single connection, so a parked transaction
// would stall the other handler instead of racing it.
type stallingAdmit struct {
	store.Store
	commit  string        // stall the push recording this commit
	entered chan struct{} // closed once that push is parked
	release chan struct{} // close to let it through
	once    sync.Once     // park at the first task only, not at every one
}

func (a *stallingAdmit) AdmitSubmission(ctx context.Context, ns store.NewSubmission,
	decide func(history []store.Submission) store.Admission) (store.Submission, error) {
	if ns.CommitSHA == a.commit {
		// Only the stalled push ever reaches Do, so the other handler is
		// never held by Once itself.
		a.once.Do(func() {
			close(a.entered)
			<-a.release
		})
	}
	return a.Store.AdmitSubmission(ctx, ns, decide)
}

// racePushes replays the hooks of two back-to-back pushes concurrently, with
// the handler of the first parked at its admission write. It asserts on the way
// that the second handler cannot get past the per-student lock while that
// lasts, so the interleaving is the same on every run. Returns both outputs.
func racePushes(t *testing.T, s *Server, firstOld, first, secondOld, second string) (firstOut, secondOut string) {
	t.Helper()
	stall := &stallingAdmit{Store: s.DB, commit: first,
		entered: make(chan struct{}), release: make(chan struct{})}
	// Both handles have to be swapped: gradePush reads through Server.DB and
	// records through the queue's own store reference.
	s.DB = stall
	s.Queue.Store = stall

	out1 := make(chan string, 1)
	go func() { out1 <- joined(s.dispatch(t.Context(), postReceive(firstOld, first))) }()
	select {
	case <-stall.entered:
	case <-time.After(60 * time.Second):
		// The park never happened: the first push admits nothing, so it has no
		// admission write to hold the lock at. This is a fuse against a
		// mis-specified test, not a timing assertion, so it is set far above
		// anything a loaded machine can need.
		t.Fatal("the first push never reached its admission write")
	}

	out2 := make(chan string, 1)
	go func() { out2 <- joined(s.dispatch(t.Context(), postReceive(secondOld, second))) }()

	// The parked handler holds the student's lock, so the second one cannot
	// read the baseline - let alone act on the range behind it - until the
	// first is done. A handler that got through here would be reading a
	// baseline that is about to change under it.
	select {
	case o := <-out2:
		t.Fatalf("the second handler ran while the first was parked: %s", o)
	case <-time.After(300 * time.Millisecond):
	}

	close(stall.release)
	return <-out1, <-out2
}

// TestGradePushSerializesConcurrentPushes: hook connections are served
// concurrently, but every handler of one student diffs from
// refs/anygrade/baseline and then walks the range that starts there. Letting
// two of them overlap means both read the same baseline, so both walk the same
// range and both race to move the ref - and which commit it ends on depends on
// who gets there first, which git topology cannot even settle after a force
// push. Serialized, the second handler starts from the baseline the first one
// left, and the ref ends on the last commit pushed.
func TestGradePushSerializesConcurrentPushes(t *testing.T) {
	s, work, _, _ := newIntakeFixture(t)

	solve := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, "tasks", "t1", "main.go"),
			[]byte("package main // "+body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	solve("v2")
	oldOlder, older := push(t, work, "solve t1")
	solve("v3")
	oldNewer, newer := push(t, work, "improve t1")

	newerOut, olderOut := racePushes(t, s, oldNewer, newer, oldOlder, older)

	if base := git(t, s.Repos.StudentDir("alice"), "rev-parse", "refs/anygrade/baseline"); base != newer {
		t.Fatalf("baseline = %s, want the last pushed commit %s", base, newer)
	}
	if !strings.Contains(newerOut, "queued") {
		t.Errorf("the push that is the head must be graded: %s", newerOut)
	}
	if !strings.Contains(olderOut, "superseded") {
		t.Errorf("the superseded push must say so: %s", olderOut)
	}
	for _, out := range []string{olderOut, newerOut} {
		if strings.Contains(out, "baseline") {
			t.Errorf("baseline bookkeeping must stay out of the push output: %s", out)
		}
	}
}

// TestGradePushSupersededPushIsNotGraded: the lock orders the handlers, but
// not by the order their pushes arrived - each does its own work before
// reaching it, so the handler of the older push can take it second. Grading
// its commit then would diff backwards from the newer baseline, score content
// the student has already replaced, and leave the marker on the abandoned
// commit.
func TestGradePushSupersededPushIsNotGraded(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)

	solve := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, "tasks", "t1", "main.go"),
			[]byte("package main // "+body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	solve("v2")
	oldOlder, older := push(t, work, "solve t1")
	solve("v3")
	oldNewer, newer := push(t, work, "improve t1")

	// The newer push's handler takes the lock first and does the work.
	if out := joined(s.dispatch(t.Context(), postReceive(oldNewer, newer))); !strings.Contains(out, "queued") {
		t.Fatalf("the newer push must be graded: %s", out)
	}
	// The older one only gets there afterwards.
	out := joined(s.dispatch(t.Context(), postReceive(oldOlder, older)))

	if strings.Contains(out, "queued") {
		t.Errorf("a superseded push must not be graded: %s", out)
	}
	if base := git(t, s.Repos.StudentDir("alice"), "rev-parse", "refs/anygrade/baseline"); base != newer {
		t.Fatalf("baseline = %s, want the head %s", base, newer)
	}
	subs, err := s.DB.ListByUserTask(t.Context(), user.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range subs {
		if sub.CommitSHA != newer {
			t.Errorf("submission #%d recorded %s, want the head %s", sub.ID, sub.CommitSHA, newer)
		}
	}
}

// TestGradePushConcurrentPushesAdmitAMarkerOnce pins the same defect in the
// unit the student pays it in. An explicit [recheck <id>] marker is picked up
// from the commit range behind the baseline, and the re-detection filter lets
// it through by design, so a range two handlers both walk charges two attempts
// for one marker.
func TestGradePushConcurrentPushesAdmitAMarkerOnce(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)

	// Neither push touches a task file, so t1 can only come from the marker.
	notes := filepath.Join(work, "NOTES.md")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(notes, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("one")
	oldOlder, older := push(t, work, "notes [recheck t1]")
	write("two")
	oldNewer, newer := push(t, work, "more notes")

	// The head's handler is the one that does the work, so it is the one
	// parked; the superseded push queues up behind it on the student's lock.
	racePushes(t, s, oldNewer, newer, oldOlder, older)

	subs, err := s.DB.ListByUserTask(t.Context(), user.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("one marker admitted %d times, want once", len(subs))
	}
}

// TestGradePushConcurrentForcePushAdmitsAMarkerOnce: the two heads of a force
// push are siblings, so neither reachability nor any other property of the
// commits says which one arrived later. Serialization never has to answer
// that: the second handler starts from the baseline the first one left and
// walks only the range beyond it.
func TestGradePushConcurrentForcePushAdmitsAMarkerOnce(t *testing.T) {
	s, work, _, user := newIntakeFixture(t)

	if err := os.WriteFile(filepath.Join(work, "tasks", "t2", "main.go"),
		[]byte("package main // v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, first := push(t, work, "solve t2")

	// A sibling of the first commit, carrying the marker, force-pushed over it.
	git(t, work, "reset", "--hard", base)
	if err := os.WriteFile(filepath.Join(work, "NOTES.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := commitAll(t, work, "rewritten [recheck t1]")
	git(t, work, "push", "--force", "origin", "main")

	// The force-pushed sibling is the head, so its handler is the one parked;
	// the commit it replaced queues up behind it.
	racePushes(t, s, first, second, base, first)

	subs, err := s.DB.ListByUserTask(t.Context(), user.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("one marker admitted %d times, want once", len(subs))
	}
	if b := git(t, s.Repos.StudentDir("alice"), "rev-parse", "refs/anygrade/baseline"); b != second {
		t.Fatalf("baseline = %s, want the last pushed commit %s", b, second)
	}
}

// TestGradePushAdvancesBaselineWithMissingObject covers the self-heal path for
// a baseline whose object is gone: the diff falls back to the empty tree, but
// the compare-and-swap still has to expect the value the ref store holds, or
// the repo would never get a usable baseline back.
func TestGradePushAdvancesBaselineWithMissingObject(t *testing.T) {
	s, work, _, _ := newIntakeFixture(t)
	studentDir := s.Repos.StudentDir("alice")

	if err := os.WriteFile(filepath.Join(work, "tasks", "t1", "main.go"), []byte("package main // v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, head := push(t, work, "solve t1")

	// A commit reachable only from the baseline ref, then dropped from the
	// object store: the ref still reads, every diff against it fails. Done
	// after the push, the way it happens in the wild - receive-pack refuses
	// to serve a repo that already has a dangling ref.
	tree := git(t, studentDir, "rev-parse", "HEAD^{tree}")
	dangling := git(t, studentDir, "-c", "user.name=t", "-c", "user.email=t@t",
		"commit-tree", "-m", "gone", tree)
	git(t, studentDir, "update-ref", "refs/anygrade/baseline", dangling)
	if err := os.Remove(filepath.Join(studentDir, "objects", dangling[:2], dangling[2:])); err != nil {
		t.Fatal(err)
	}

	out := joined(s.dispatch(t.Context(), postReceive(old, head)))

	if !strings.Contains(out, "queued") {
		t.Fatalf("the push must still be graded: %s", out)
	}
	if base := git(t, studentDir, "rev-parse", "refs/anygrade/baseline"); base != head {
		t.Fatalf("baseline = %s, want %s - the repo cannot self-heal", base, head)
	}
}

// TestGradePushReportsBrokenBaselineRef: only a lost race is silent. A ref
// store that cannot take the write at all is still an error the student sees,
// because their next push really does re-detect these changes.
func TestGradePushReportsBrokenBaselineRef(t *testing.T) {
	s, work, _, _ := newIntakeFixture(t)
	studentDir := s.Repos.StudentDir("alice")

	// refs/anygrade/baseline/x blocks refs/anygrade/baseline itself (git's
	// D/F conflict), so update-ref fails for a reason that is not a mismatch.
	git(t, studentDir, "update-ref", "-d", "refs/anygrade/baseline")
	git(t, studentDir, "update-ref", "refs/anygrade/baseline/x", git(t, studentDir, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(work, "tasks", "t1", "main.go"), []byte("package main // v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, head := push(t, work, "solve t1")
	out := joined(s.dispatch(t.Context(), postReceive(old, head)))

	if !strings.Contains(out, "baseline update failed") {
		t.Fatalf("a broken ref store must be reported, not taken for a race: %s", out)
	}
}

// TestGradePushReportsBrokenBaselineContents: a ref file that does not hold a
// resolvable object id fails with the same "cannot lock ref ... unable to
// resolve reference" prefix a mismatch produces, but it is not a race - git
// refuses to move such a ref, and refuses to delete it too, so the repo cannot
// recover on its own. Taking it for a lost race would hide that forever while
// every push re-detects every task.
func TestGradePushReportsBrokenBaselineContents(t *testing.T) {
	s, work, _, _ := newIntakeFixture(t)
	studentDir := s.Repos.StudentDir("alice")

	ref := filepath.Join(studentDir, "refs", "anygrade", "baseline")
	if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ref, []byte("not-a-sha\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(work, "tasks", "t1", "main.go"), []byte("package main // v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, head := push(t, work, "solve t1")
	out := joined(s.dispatch(t.Context(), postReceive(old, head)))

	if !strings.Contains(out, "baseline update failed") {
		t.Fatalf("a broken ref must be reported, not taken for a race: %s", out)
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
