package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
}

func (p *testPrep) Prepare(_ context.Context, sub store.Submission) (Prepared, error) {
	if p.failErr != nil {
		return Prepared{}, p.failErr
	}
	runDir := filepath.Join(p.baseDir, fmt.Sprintf("run-%d-%d", sub.ID, time.Now().UnixNano()))
	return Prepared{
		Assembly: runner.Assembly{
			Dest:          filepath.Join(runDir, "ws"),
			Task:          p.task,
			TaskRelDir:    "tasks/t1",
			Authoritative: runner.WorkingCopySource{Root: p.repo},
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
