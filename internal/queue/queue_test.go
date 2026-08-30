package queue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/runner"
	"github.com/ekalinin/anygrade/internal/store"
)

// testPrep serves a fixed task from a temp working copy; failErr, when set,
// simulates an infra failure in Prepare.
type testPrep struct {
	task    config.ResolvedTask
	repo    string
	baseDir string
	failErr error
	// auth replaces the authoritative source when set: tests use it to pin the
	// cancellation window inside runner.Assemble.
	auth runner.Source
	// calls counts started jobs; a run that must never happen leaves it at 0.
	calls atomic.Int64
	// student, when set, overlays solution files like the real prep does.
	student runner.Source
}

func (p *testPrep) Prepare(_ context.Context, sub store.Submission) (Prepared, error) {
	p.calls.Add(1)
	if p.failErr != nil {
		return Prepared{}, p.failErr
	}
	runDir := filepath.Join(p.baseDir, fmt.Sprintf("run-%d-%d", sub.ID, time.Now().UnixNano()))
	var authoritative runner.Source = runner.WorkingCopySource{Root: p.repo}
	if p.auth != nil {
		authoritative = p.auth
	}
	return Prepared{
		Assembly: runner.Assembly{
			Dest:          filepath.Join(runDir, "ws"),
			Task:          p.task,
			TaskRelDir:    "tasks/t1",
			Authoritative: authoritative,
			Student:       p.student,
			RunAsUID:      -1,
			RunAsGID:      -1,
		},
		Task:   p.task,
		LogDir: filepath.Join(runDir, "logs"),
	}, nil
}

// newTestQueue builds a store + queue over a tiny real task using the local
// runner: a passing gate, a passing weighted check, and a failing one.
func newTestQueue(t *testing.T) (*Queue, *store.DB, store.User, *testPrep) {
	t.Helper()
	db, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u, err := db.CreateUser(t.Context(), "s1", "S1", "student")
	if err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "tasks", "t1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tasks", "t1", "solution.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	soft := time.Now().Add(-25 * time.Hour) // one started 24h interval late
	task := config.ResolvedTask{
		ID: "t1", Name: "T1", Score: 100,
		SolutionFiles: []string{"solution.txt"},
		Runner:        config.ResolvedRunner{Type: "local", Timeout: time.Minute},
		Checks: []config.Check{
			{Name: "gate", Required: true, Run: "test -f solution.txt"},
			{Name: "good", Weight: 60, Run: "true"},
			{Name: "bad", Weight: 40, Run: "exit 1"},
		},
		Deadline: config.ResolvedDeadline{
			Soft:    &soft,
			Penalty: config.ResolvedPenalty{Percent: 10, Per: 24 * time.Hour, MaxPercent: 50},
		},
	}
	prep := &testPrep{task: task, repo: repo, baseDir: t.TempDir()}
	q := &Queue{Store: db, Prep: prep, Workers: 2, PollInterval: 20 * time.Millisecond}
	return q, db, u, prep
}

func waitStatus(t *testing.T, db *store.DB, id int64, want string) store.Submission {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sub, _, err := db.GetSubmission(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if sub.Status == want {
			return sub
		}
		time.Sleep(20 * time.Millisecond)
	}
	sub, _, _ := db.GetSubmission(t.Context(), id)
	t.Fatalf("submission %d: status %q, want %q (note: %s)", id, sub.Status, want, sub.WorkerNote)
	return store.Submission{}
}

// TestSubmitAdmitsOneSlotUnderRace: the last free attempt goes to exactly one
// of N concurrent submissions. Before the verdict moved inside the write
// transaction every caller read the same empty history and all of them were
// admitted, overshooting max_attempts.
func TestSubmitAdmitsOneSlotUnderRace(t *testing.T) {
	q, db, u, _ := newTestQueue(t)
	task := policyTask(1, 0, nil)

	const n = 8
	decisions := make([]Decision, n)
	errs := make([]error, n)
	start := make(chan struct{})
	now := time.Now()

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start // release them all at once
			_, d, err := q.Submit(t.Context(), task, store.NewSubmission{
				UserID: u.ID, TaskID: "t1", CommitSHA: fmt.Sprintf("c%d", i),
				ReceivedAt: now,
			}, false)
			decisions[i], errs[i] = d, err
		})
	}
	close(start)
	wg.Wait()

	admitted := 0
	for i, d := range decisions {
		if errs[i] != nil {
			t.Fatalf("submit %d: %v", i, errs[i])
		}
		if d.Admit {
			admitted++
			continue
		}
		if d.RejectStatus != store.StatusRejectedLimit {
			t.Errorf("submit %d rejected as %q, want %q", i, d.RejectStatus, store.StatusRejectedLimit)
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d of %d, want exactly 1", admitted, n)
	}

	// Every attempt is still recorded for the student's history; only one of
	// them consumed the slot.
	history, err := db.ListByUserTask(t.Context(), u.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != n {
		t.Fatalf("history has %d rows, want %d", len(history), n)
	}
	if got := CountAttempts(history); got != 1 {
		t.Errorf("CountAttempts = %d, want 1", got)
	}
}

// TestSubmitCooldownUnderRace: a burst that arrives together may not slip
// past an active cooldown either.
func TestSubmitCooldownUnderRace(t *testing.T) {
	q, db, u, _ := newTestQueue(t)
	task := policyTask(0, time.Hour, nil)
	now := time.Now()

	const n = 6
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			_, _, _ = q.Submit(t.Context(), task, store.NewSubmission{
				UserID: u.ID, TaskID: "t1", CommitSHA: fmt.Sprintf("c%d", i),
				ReceivedAt: now,
			}, false)
		})
	}
	close(start)
	wg.Wait()

	history, err := db.ListByUserTask(t.Context(), u.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got := CountAttempts(history); got != 1 {
		t.Fatalf("CountAttempts = %d, want 1: the cooldown admitted more than one", got)
	}
}

// TestSubmitStoresRejectReason: the reason the policy computed reaches the row,
// not only the push output - the submission page has nothing else to show for a
// rejection. English by SPEC §10.1: it is written at submission time.
func TestSubmitStoresRejectReason(t *testing.T) {
	q, db, u, _ := newTestQueue(t)
	now := time.Now()
	hard := now.Add(-time.Hour)

	cases := []struct {
		taskID string
		task   config.ResolvedTask
		before int // counting submissions already recorded for the task
		status string
		want   string
	}{
		{"deadline", policyTask(0, 0, &hard), 0, store.StatusRejectedDeadline,
			"hard deadline passed (" + hard.Format("2006-01-02 15:04 -07") + ")"},
		{"limit", policyTask(1, 0, nil), 1, store.StatusRejectedLimit,
			"attempt limit reached (1 of 1)"},
	}
	for _, c := range cases {
		t.Run(c.taskID, func(t *testing.T) {
			for i := range c.before {
				if _, err := db.Enqueue(t.Context(), store.NewSubmission{
					UserID: u.ID, TaskID: c.taskID, CommitSHA: fmt.Sprintf("old%d", i),
					ReceivedAt: now.Add(-time.Hour), Counts: true,
				}); err != nil {
					t.Fatal(err)
				}
			}
			sub, d, err := q.Submit(t.Context(), c.task, store.NewSubmission{
				UserID: u.ID, TaskID: c.taskID, CommitSHA: "new", ReceivedAt: now,
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			if sub.Status != c.status || d.RejectStatus != c.status {
				t.Fatalf("status %q (decision %q), want %q", sub.Status, d.RejectStatus, c.status)
			}
			if d.RejectReason != c.want {
				t.Fatalf("decision reason = %q, want %q", d.RejectReason, c.want)
			}
			got, _, err := db.GetSubmission(t.Context(), sub.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.WorkerNote != c.want {
				t.Errorf("stored note = %q, want %q", got.WorkerNote, c.want)
			}
		})
	}
}

// TestPipelineScoresAndPersists: full pipeline over the local runner, with the
// penalty computed at received_at (2 intervals late at enqueue time).
func TestPipelineScoresAndPersists(t *testing.T) {
	q, db, u, _ := newTestQueue(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Start(ctx) }()

	sub, err := q.Enqueue(ctx, store.NewSubmission{
		UserID: u.ID, TaskID: "t1", CommitSHA: "abc",
		ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := waitStatus(t, db, sub.ID, store.StatusDone)
	cancel()
	<-done

	// raw = 100 * 60/100 = 60; penalty: 25h past soft, per=24h → 2 intervals → 20%.
	if *got.RawScore != 60 {
		t.Errorf("raw: %v", *got.RawScore)
	}
	if *got.PenaltyPercent != 20 {
		t.Errorf("penalty: %v", *got.PenaltyPercent)
	}
	if want := 48.0; *got.FinalScore != want {
		t.Errorf("final: %v, want %v", *got.FinalScore, want)
	}
	_, checks, err := db.GetSubmission(t.Context(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 3 || !checks[0].Passed || !checks[1].Passed || checks[2].Passed {
		t.Errorf("checks: %+v", checks)
	}
}

// TestInfraErrorBackoffToTerminal: Prepare fails → backoff grows, and after
// MaxRetries the submission is terminal infra_error.
func TestInfraErrorBackoffToTerminal(t *testing.T) {
	q, db, u, prep := newTestQueue(t)
	prep.failErr = &runner.InfraError{Op: "workspace", Err: errors.New("disk on fire")}
	q.BackoffBase = 10 * time.Millisecond
	q.BackoffCap = 40 * time.Millisecond
	q.MaxRetries = 2

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Start(ctx) }()

	sub, err := q.Enqueue(ctx, store.NewSubmission{
		UserID: u.ID, TaskID: "t1", CommitSHA: "abc", ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait until terminal: infra_error with retry_at nil and retries > MaxRetries.
	deadline := time.Now().Add(10 * time.Second)
	var got store.Submission
	for time.Now().Before(deadline) {
		got, _, _ = db.GetSubmission(t.Context(), sub.ID)
		if got.Status == store.StatusInfraError && got.RetryAt == nil && got.Retries >= q.MaxRetries {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if got.RetryAt != nil || got.Retries < q.MaxRetries {
		t.Fatalf("not terminal: %+v", got)
	}
	if got.WorkerNote == "" {
		t.Error("terminal infra_error must carry a worker note for the teacher view")
	}
	// Running out of retries does not turn operator detail into something the
	// student may read (SPEC §14).
	if got.StudentNote != "" {
		t.Errorf("student note = %q, want empty: the cause was never marked public", got.StudentNote)
	}
}

// TestRetryableNoteReachesStudentOnlyWhenMarked: a retryable failure is stored
// for two audiences at once. hidden scrubs its own message and intake marks it
// Public, so its owner reads why the submission is stuck (SPEC §14); anything
// unmarked - a docker daemon error, a wrapped filesystem failure - is the
// teacher's alone and leaves the student note empty.
func TestRetryableNoteReachesStudentOnlyWhenMarked(t *testing.T) {
	const scrubbed = "hidden tests temporarily unavailable"
	cases := []struct {
		name    string
		fail    error
		want    string // the student's note
		teacher string // the teacher's
	}{
		{"marked", Public(errors.New(scrubbed)), scrubbed, scrubbed},
		{"unmarked", errors.New("student repo: stat /srv/.anygrade/repos/bob.git: no such file"),
			"", "student repo: stat /srv/.anygrade/repos/bob.git: no such file"},
		// Context added on the way out is the teacher's; the student keeps
		// exactly the wording the scrubbing package promised.
		{"wrapped", fmt.Errorf("hidden tests: %w", Public(errors.New(scrubbed))),
			scrubbed, "hidden tests: " + scrubbed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, db, u, prep := newTestQueue(t)
			prep.failErr = c.fail

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); _ = q.Start(ctx) }()

			sub, err := q.Enqueue(ctx, store.NewSubmission{
				UserID: u.ID, TaskID: "t1", CommitSHA: "abc", ReceivedAt: time.Now(), Counts: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := waitStatus(t, db, sub.ID, store.StatusInfraError)
			cancel()
			<-done

			if got.RetryAt == nil {
				t.Fatalf("a retryable failure must stay armed: %+v", got)
			}
			if got.StudentNote != c.want {
				t.Errorf("student note = %q, want %q", got.StudentNote, c.want)
			}
			if got.WorkerNote != c.teacher {
				t.Errorf("worker note = %q, want %q", got.WorkerNote, c.teacher)
			}
		})
	}
}

// TestTerminalPrepareError: a Terminal prepare failure flips the submission
// straight to terminal infra_error, note verbatim, no retries burned.
func TestTerminalPrepareError(t *testing.T) {
	q, db, u, prep := newTestQueue(t)
	prep.failErr = Terminal("hidden tests unavailable for this task")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Start(ctx) }()

	sub, err := q.Enqueue(ctx, store.NewSubmission{
		UserID: u.ID, TaskID: "t1", CommitSHA: "abc", ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := waitStatus(t, db, sub.ID, store.StatusInfraError)
	cancel()
	<-done

	if got.RetryAt != nil {
		t.Fatalf("terminal error must not schedule a retry: %+v", got.RetryAt)
	}
	if got.WorkerNote != "hidden tests unavailable for this task" {
		t.Fatalf("worker note %q", got.WorkerNote)
	}
	// Terminal's contract is already student-safe text, so its note is the
	// student's explanation as much as the teacher's (SPEC §14).
	if got.StudentNote != got.WorkerNote {
		t.Errorf("student note = %q, want the terminal note", got.StudentNote)
	}
}

// TestTamperingIsTerminal: an overlay the workspace refuses (here: a solution
// file past the size limit) is the student's doing, so the submission fails
// terminally with the reason as the note - retrying it forever would only
// occupy a worker and hide the cause from the teacher.
func TestTamperingIsTerminal(t *testing.T) {
	q, db, u, prep := newTestQueue(t)
	student := t.TempDir()
	if err := os.MkdirAll(filepath.Join(student, "tasks", "t1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(student, "tasks", "t1", "solution.txt"),
		[]byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	prep.student = runner.WorkingCopySource{Root: student}
	prep.task.Workspace = config.ResolvedWorkspace{MaxFileSize: 64, MaxTotalSize: 64}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Start(ctx) }()

	sub, err := q.Enqueue(ctx, store.NewSubmission{
		UserID: u.ID, TaskID: "t1", CommitSHA: "abc", ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := waitStatus(t, db, sub.ID, store.StatusInfraError)
	cancel()
	<-done

	if got.RetryAt != nil {
		t.Fatalf("a rejected overlay must not be retried: %+v", got.RetryAt)
	}
	if !strings.Contains(got.WorkerNote, "exceeds") {
		t.Fatalf("worker note %q", got.WorkerNote)
	}
}

// TestTeacherCancelRunning: Queue.Cancel on a live submission kills the run,
// keeps the row terminal-canceled, and never requeues or resurrects it.
func TestTeacherCancelRunning(t *testing.T) {
	q, db, u, prep := newTestQueue(t)
	prep.task.Checks = []config.Check{{Name: "slow", Weight: 1, Run: "sleep 30"}}
	prep.task.Runner.Timeout = time.Minute

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Start(ctx) }()

	sub, err := q.Enqueue(ctx, store.NewSubmission{
		UserID: u.ID, TaskID: "t1", CommitSHA: "abc", ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, db, sub.ID, store.StatusRunning)

	ok, err := q.Cancel(t.Context(), sub.ID)
	if err != nil || !ok {
		t.Fatalf("Cancel: ok=%v err=%v", ok, err)
	}

	// Poll for a while: the row must stay terminal-canceled, and the freed
	// worker must not requeue or resurrect it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _, err := db.GetSubmission(t.Context(), sub.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != store.StatusInfraError || got.CanceledAt == nil ||
			got.RetryAt != nil || got.Counts {
			t.Fatalf("canceled row mutated: %+v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	// Cancel of an already-terminal submission reports ok=false.
	if ok, err := q.Cancel(context.Background(), sub.ID); ok || err != nil {
		t.Fatalf("second cancel: ok=%v err=%v", ok, err)
	}
}

// TestTeacherCancelBeforeJobRegistered: the cancel lands in the window between
// the claim and the registration of the running job, where it finds nothing to
// interrupt. The worker has to notice by itself that the row is no longer its
// to run - and it must notice before starting the check, not only when it
// finally fails to write a result.
func TestTeacherCancelBeforeJobRegistered(t *testing.T) {
	q, db, u, prep := newTestQueue(t)
	prep.task.Checks = []config.Check{{Name: "slow", Weight: 1, Run: "sleep 30"}}

	sub, err := q.Enqueue(t.Context(), store.NewSubmission{
		UserID: u.ID, TaskID: "t1", CommitSHA: "abc", ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Play the worker's first step by hand: the row is claimed, but no job is
	// registered yet.
	claimed, ok, err := db.ClaimNext(t.Context(), time.Now())
	if err != nil || !ok || claimed.ID != sub.ID {
		t.Fatalf("claim: #%d ok=%v err=%v", claimed.ID, ok, err)
	}
	if ok, err := q.Cancel(t.Context(), sub.ID); err != nil || !ok {
		t.Fatalf("cancel: ok=%v err=%v", ok, err)
	}

	// Only now does the worker pick up the submission it claimed.
	q.process(t.Context(), claimed)

	if n := prep.calls.Load(); n != 0 {
		t.Fatalf("the check was started %d times for a canceled submission, want 0", n)
	}
	got, _, err := db.GetSubmission(t.Context(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusInfraError || got.CanceledAt == nil ||
		got.RetryAt != nil || got.Counts || got.WorkerNote != "canceled by teacher" {
		t.Fatalf("canceled row mutated: %+v", got)
	}
}

// TestGracefulShutdownRequeues: cancel during a long check → the submission
// returns to queued with no retry counted.
func TestGracefulShutdownRequeues(t *testing.T) {
	q, db, u, prep := newTestQueue(t)
	prep.task.Checks = []config.Check{{Name: "slow", Weight: 1, Run: "sleep 30"}}
	prep.task.Runner.Timeout = time.Minute

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Start(ctx) }()

	sub, err := q.Enqueue(ctx, store.NewSubmission{
		UserID: u.ID, TaskID: "t1", CommitSHA: "abc", ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, db, sub.ID, store.StatusRunning)
	cancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("workers did not drain after cancel")
	}
	got, _, err := db.GetSubmission(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusQueued {
		t.Fatalf("status %q, want queued", got.Status)
	}
	if got.Retries != 0 {
		t.Fatalf("cancellation must not count as a retry: %+v", got)
	}
}

// blockingSource enters Export and stays there until the context is canceled,
// which pins the cancellation window inside runner.Assemble instead of the
// check run.
type blockingSource struct{ entered chan struct{} }

func (s *blockingSource) Export(ctx context.Context, _, _ string) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *blockingSource) Open(context.Context, string) (io.ReadCloser, bool, error) {
	return nil, false, nil
}

// TestShutdownDuringAssembleRequeues: the shutdown window is not only the
// check run. A cancel landing while the workspace is still being assembled
// must requeue the submission too - Assemble reports it as a plain workspace
// error, and treating that as an infra failure both counted a retry and wrote
// with the already-canceled context, leaving the row stuck in running.
func TestShutdownDuringAssembleRequeues(t *testing.T) {
	q, db, u, prep := newTestQueue(t)
	src := &blockingSource{entered: make(chan struct{}, 1)}
	prep.auth = src

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Start(ctx) }()

	sub, err := q.Enqueue(ctx, store.NewSubmission{
		UserID: u.ID, TaskID: "t1", CommitSHA: "abc", ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-src.entered // the worker is inside Assemble
	cancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("workers did not drain after cancel")
	}
	got, _, err := db.GetSubmission(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusQueued {
		t.Fatalf("status %q, want queued (note: %s)", got.Status, got.WorkerNote)
	}
	if got.Retries != 0 {
		t.Fatalf("cancellation must not count as a retry: %+v", got)
	}
}

// TestTeacherCancelDuringAssemble: the same window, but the context dies
// because the teacher canceled the submission - the row must stay terminal
// instead of going back to the queue.
func TestTeacherCancelDuringAssemble(t *testing.T) {
	q, db, u, prep := newTestQueue(t)
	src := &blockingSource{entered: make(chan struct{}, 1)}
	prep.auth = src

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Start(ctx) }()

	sub, err := q.Enqueue(ctx, store.NewSubmission{
		UserID: u.ID, TaskID: "t1", CommitSHA: "abc", ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-src.entered // the worker is inside Assemble
	if ok, err := q.Cancel(ctx, sub.ID); err != nil || !ok {
		t.Fatalf("Cancel: ok=%v err=%v", ok, err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got, _, err := db.GetSubmission(ctx, sub.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != store.StatusInfraError || got.CanceledAt == nil ||
			got.RetryAt != nil || got.Counts {
			t.Fatalf("canceled row mutated: %+v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
}
