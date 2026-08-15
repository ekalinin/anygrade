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

// ErrTerminal matches any non-retryable preparation failure, so a JobPrep
// implementation can assert its own classification: errors.Is(err, ErrTerminal).
var ErrTerminal = errors.New("preparation failed permanently")

// Terminal wraps msg as a non-retryable preparation failure: process flips
// the submission straight to terminal infra_error with msg as the worker
// note (verbatim - callers hand over already student-safe text).
func Terminal(msg string) error { return &terminalError{msg} }

type terminalError struct{ msg string }

func (e *terminalError) Error() string { return e.msg }
func (e *terminalError) Unwrap() error { return ErrTerminal }

// Prepared is everything a worker needs to run one submission.
type Prepared struct {
	Assembly runner.Assembly     // sources wired; Dest/TaskRelDir set
	Task     config.ResolvedTask // runner spec, checks, score, deadline
	LogDir   string
	Note     string // worker note carried to the result (e.g. tamper notes)
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
	Events    Publisher // optional live-update sink; nil = disabled

	// Backoff schedule for infra errors: min(Base<<retries, Cap), then after
	// MaxRetries the submission becomes terminal infra_error (SPEC §13).
	BackoffBase time.Duration // default 10s
	BackoffCap  time.Duration // default 5m
	MaxRetries  int           // default 8

	PollInterval time.Duration // claim-loop wakeup for retry_at; default 1s

	notify chan struct{}
	once   sync.Once

	// Teacher-cancel bookkeeping: running maps live submissions to their
	// execution cancel funcs; canceling marks teacher cancels so the worker
	// can tell them apart from a graceful shutdown.
	mu        sync.Mutex
	running   map[int64]context.CancelFunc
	canceling map[int64]bool
}

func (q *Queue) init() {
	q.once.Do(func() {
		q.notify = make(chan struct{}, 1)
		q.running = make(map[int64]context.CancelFunc)
		q.canceling = make(map[int64]bool)
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

// Submit is the single entry point for an incoming submission (SPEC §6
// step 4): it applies the admission policy and records the outcome - queued
// or rejected_* - then publishes it and wakes a worker.
//
// Reading the history, deciding, and writing the row must not be interleaved:
// two concurrent pushes, or a push racing a UI recheck, would otherwise both
// see the last free attempt slot and both take it. AdmitSubmission holds the
// whole sequence in one write transaction; Admit itself stays pure, so the
// policy remains testable without a database.
//
// The verdict is taken at ns.ReceivedAt - the moment the server accepted the
// push, never a commit timestamp (SPEC §6).
func (q *Queue) Submit(ctx context.Context, t config.ResolvedTask,
	ns store.NewSubmission, teacherRecheck bool) (store.Submission, Decision, error) {

	q.init()
	var d Decision
	sub, err := q.Store.AdmitSubmission(ctx, ns, func(history []store.Submission) store.Admission {
		d = Admit(t, history, ns.ReceivedAt, teacherRecheck)
		return store.Admission{Admit: d.Admit, RejectStatus: d.RejectStatus,
			RejectReason: d.RejectReason, Counts: d.Counts}
	})
	if err != nil {
		return store.Submission{}, Decision{}, err
	}
	q.publish(sub, sub.Status)
	if d.Admit {
		q.wake()
	}
	return sub, d, nil
}

// Enqueue persists an already-admitted submission and wakes a worker. Submit
// is the normal path; this stays for callers that own the policy themselves.
func (q *Queue) Enqueue(ctx context.Context, ns store.NewSubmission) (store.Submission, error) {
	q.init()
	sub, err := q.Store.Enqueue(ctx, ns)
	if err != nil {
		return store.Submission{}, err
	}
	q.publish(sub, store.StatusQueued)
	q.wake()
	return sub, nil
}

// publish notifies the optional live-update sink after a DB write. Whoever
// writes the row publishes; delivery is best-effort by contract.
func (q *Queue) publish(sub store.Submission, status string) {
	if q.Events != nil {
		q.Events.Publish(Event{SubID: sub.ID, UserID: sub.UserID, TaskID: sub.TaskID, Status: status})
	}
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
	q.publish(sub, store.StatusRunning)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q.trackStart(sub.ID, cancel)
	defer q.trackEnd(sub.ID)

	p, err := q.Prep.Prepare(ctx, sub)
	if err != nil {
		if errors.Is(err, ErrTaskGone) {
			q.terminal(ctx, sub, err.Error())
			return
		}
		if te, ok := errors.AsType[*terminalError](err); ok {
			q.terminal(ctx, sub, te.msg)
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
			if q.wasCanceled(sub.ID) {
				return // teacher cancel: the row is already terminal
			}
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

	if q.wasCanceled(sub.ID) {
		return // canceled after the last check: keep the canceled row
	}
	err = q.Store.FinishSubmission(ctx, sub.ID, store.SubmissionResult{
		Status: store.StatusDone, Raw: raw, Penalty: pen, Final: final,
		Note: p.Note, LogDir: p.LogDir, Checks: checks,
	})
	if err != nil {
		q.retry(ctx, sub, err)
		return
	}
	q.publish(sub, store.StatusDone)
}

// Cancel aborts one submission on a teacher's behalf: a queued row just
// flips terminal in the DB; a running row additionally gets its execution
// context canceled (the docker runner kills the live container). ok=false
// when the submission already finished.
func (q *Queue) Cancel(ctx context.Context, id int64) (bool, error) {
	q.init()
	// Mark BEFORE the DB write: if the flip beats a concurrent finish, the
	// worker must already see the marker on its post-run checks.
	q.mu.Lock()
	cancel := q.running[id]
	if cancel != nil {
		q.canceling[id] = true
	}
	q.mu.Unlock()

	sub, ok, err := q.Store.CancelSubmission(ctx, id, time.Now())
	if err != nil || !ok {
		q.mu.Lock()
		delete(q.canceling, id)
		q.mu.Unlock()
		return false, err
	}
	if cancel != nil {
		cancel()
	}
	q.publish(sub, "canceled")
	return true, nil
}

func (q *Queue) trackStart(id int64, cancel context.CancelFunc) {
	q.mu.Lock()
	q.running[id] = cancel
	q.mu.Unlock()
}

func (q *Queue) trackEnd(id int64) {
	q.mu.Lock()
	delete(q.running, id)
	delete(q.canceling, id)
	q.mu.Unlock()
}

func (q *Queue) wasCanceled(id int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.canceling[id]
}

// retry schedules an infra_error retry with exponential backoff and jitter;
// after MaxRetries the submission becomes terminal (retry_at NULL).
func (q *Queue) retry(ctx context.Context, sub store.Submission, cause error) {
	if q.wasCanceled(sub.ID) {
		return // never resurrect a teacher-canceled row into the queue
	}
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
	q.publish(sub, store.StatusInfraError)
}

func (q *Queue) terminal(ctx context.Context, sub store.Submission, note string) {
	_ = q.Store.ScheduleRetry(ctx, sub.ID, nil, note)
	q.publish(sub, store.StatusInfraError)
}

// requeue survives ctx cancellation: it must run even during shutdown.
func (q *Queue) requeue(sub store.Submission) {
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = q.Store.Requeue(rctx, sub.ID)
	q.publish(sub, store.StatusQueued)
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
