package queue

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/runner"
	"github.com/ekalinin/anygrade/internal/scoring"
	"github.com/ekalinin/anygrade/internal/store"
)

// ErrTaskGone marks a submission whose task id no longer exists in the course
// repo (SPEC §13): terminal infra_error, surfaced to the teacher, never retried.
var ErrTaskGone = errors.New("task no longer exists in the course repo")

// Prepared is everything a worker needs to run one submission.
type Prepared struct {
	Assembly runner.Assembly     // sources wired; Dest/TaskRelDir set
	Task     config.ResolvedTask // runner spec, checks, score, deadline
	LogDir   string
}

// JobPrep builds the run context for a claimed submission. M3 tests inject a
// working-copy-backed prep; the M4 git server swaps in a bare-repo one without
// touching queue code.
type JobPrep interface {
	Prepare(ctx context.Context, sub store.Submission) (Prepared, error)
}

// Queue is the worker pool over the submissions table.
type Queue struct {
	Store     store.SubmissionStore
	Prep      JobPrep
	NewRunner func(config.ResolvedRunner) (runner.Runner, error)
	Workers   int

	// Backoff schedule for infra errors: min(Base<<retries, Cap), then after
	// MaxRetries the submission becomes terminal infra_error (SPEC §13).
	BackoffBase time.Duration // default 10s
	BackoffCap  time.Duration // default 5m
	MaxRetries  int           // default 8

	PollInterval time.Duration // claim-loop wakeup for retry_at; default 1s

	notify chan struct{}
	once   sync.Once
}

func (q *Queue) init() {
	q.once.Do(func() {
		q.notify = make(chan struct{}, 1)
		if q.Workers <= 0 {
			q.Workers = 4
		}
		if q.BackoffBase <= 0 {
			q.BackoffBase = 10 * time.Second
		}
		if q.BackoffCap <= 0 {
			q.BackoffCap = 5 * time.Minute
		}
		if q.MaxRetries <= 0 {
			q.MaxRetries = 8
		}
		if q.PollInterval <= 0 {
			q.PollInterval = time.Second
		}
		if q.NewRunner == nil {
			q.NewRunner = func(spec config.ResolvedRunner) (runner.Runner, error) {
				return runner.New(spec, "", nil)
			}
		}
	})
}

// Enqueue admits nothing by itself (policy is the caller's job via Admit);
// it persists the submission and wakes a worker.
func (q *Queue) Enqueue(ctx context.Context, ns store.NewSubmission) (store.Submission, error) {
	q.init()
	sub, err := q.Store.Enqueue(ctx, ns)
	if err != nil {
		return store.Submission{}, err
	}
	q.wake()
	return sub, nil
}

func (q *Queue) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Start requeues submissions left running by a previous process (restart
// recovery, SPEC §5) and launches the worker pool. It blocks until ctx is
// canceled and every worker has drained.
func (q *Queue) Start(ctx context.Context) error {
	q.init()
	if _, err := q.Store.RequeueRunning(ctx); err != nil {
		return fmt.Errorf("requeue running: %w", err)
	}
	var wg sync.WaitGroup
	for range q.Workers {
		wg.Go(func() { q.workerLoop(ctx) })
	}
	wg.Wait()
	return nil
}

func (q *Queue) workerLoop(ctx context.Context) {
	ticker := time.NewTicker(q.PollInterval)
	defer ticker.Stop()
	for {
		// Drain everything claimable, then sleep.
		for {
			if ctx.Err() != nil {
				return
			}
			sub, ok, err := q.Store.ClaimNext(ctx, time.Now())
			if err != nil || !ok {
				break
			}
			q.process(ctx, sub)
		}
		select {
		case <-ctx.Done():
			return
		case <-q.notify:
		case <-ticker.C:
		}
	}
}

// process runs one claimed submission through prepare → assemble → run →
// score → persist. No DB transaction is ever held across the check run.
func (q *Queue) process(ctx context.Context, sub store.Submission) {
	p, err := q.Prep.Prepare(ctx, sub)
	if err != nil {
		if errors.Is(err, ErrTaskGone) {
			q.terminal(ctx, sub, err.Error())
			return
		}
		q.retry(ctx, sub, err)
		return
	}

	ws, err := runner.Assemble(ctx, p.Assembly)
	if err != nil {
		q.retry(ctx, sub, err)
		return
	}
	defer ws.Close()

	r, err := q.NewRunner(p.Task.Runner)
	if err != nil {
		q.terminal(ctx, sub, err.Error()) // misconfigured runner: retrying won't help
		return
	}
	outcomes, err := r.Run(ctx, runner.Job{
		WorkspaceDir: ws.Root,
		TaskRelDir:   p.Assembly.TaskRelDir,
		Spec:         p.Task.Runner,
		Checks:       p.Task.Checks,
		LogDir:       p.LogDir,
	})
	if err != nil {
		if infra, ok := errors.AsType[*runner.InfraError](err); ok && infra.Op == "canceled" {
			// Graceful shutdown: back to the queue, no retry counting.
			q.requeue(sub)
			return
		}
		q.retry(ctx, sub, err)
		return
	}

	results := make([]scoring.CheckResult, len(outcomes))
	checks := make([]store.CheckRow, len(outcomes))
	for i, o := range outcomes {
		results[i] = scoring.CheckResult{
			Name:     o.Name,
			Required: p.Task.Checks[i].Required,
			Weight:   p.Task.Checks[i].Weight,
			Passed:   o.Passed,
		}
		checks[i] = store.CheckRow{
			Name:       o.Name,
			Passed:     o.Passed,
			ExitCode:   o.ExitCode,
			Duration:   o.Duration,
			Weight:     p.Task.Checks[i].Weight,
			Skipped:    o.Skipped,
			TimedOut:   o.TimedOut,
			LogExcerpt: o.LogExcerpt,
		}
	}
	raw := scoring.RawScore(p.Task.Score, results)
	// Penalty is fixed at submission time (SPEC §9), not at grading time.
	pen := scoring.PenaltyPercent(deadlineOf(p.Task), sub.ReceivedAt)
	final := scoring.FinalScore(raw, pen)

	err = q.Store.FinishSubmission(ctx, sub.ID, store.SubmissionResult{
		Status: store.StatusDone, Raw: raw, Penalty: pen, Final: final, Checks: checks,
	})
	if err != nil {
		q.retry(ctx, sub, err)
	}
}

// retry schedules an infra_error retry with exponential backoff and jitter;
// after MaxRetries the submission becomes terminal (retry_at NULL).
func (q *Queue) retry(ctx context.Context, sub store.Submission, cause error) {
	note := cause.Error()
	if sub.Retries >= q.MaxRetries {
		q.terminal(ctx, sub, note+" (retries exhausted)")
		return
	}
	delay := min(q.BackoffBase<<sub.Retries, q.BackoffCap)
	// ±10% jitter avoids a thundering herd on a shared cause (docker down).
	delay += time.Duration((rand.Float64() - 0.5) * 0.2 * float64(delay))
	at := time.Now().Add(delay)
	_ = q.Store.ScheduleRetry(ctx, sub.ID, &at, note)
}

func (q *Queue) terminal(ctx context.Context, sub store.Submission, note string) {
	_ = q.Store.ScheduleRetry(ctx, sub.ID, nil, note)
}

// requeue survives ctx cancellation: it must run even during shutdown.
func (q *Queue) requeue(sub store.Submission) {
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = q.Store.Requeue(rctx, sub.ID)
}

func deadlineOf(t config.ResolvedTask) scoring.Deadline {
	return scoring.Deadline{
		Soft: t.Deadline.Soft,
		Hard: t.Deadline.Hard,
		Penalty: scoring.Penalty{
			Percent:    t.Deadline.Penalty.Percent,
			Per:        t.Deadline.Penalty.Per,
			MaxPercent: t.Deadline.Penalty.MaxPercent,
		},
	}
}
