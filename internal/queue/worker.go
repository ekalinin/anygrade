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
	"github.com/ekalinin/anygrade/internal/testreport"
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

// Public marks a retryable failure whose message is student-safe: it is stored
// as the submission's student note next to the worker note, so the student is
// told why their submission could not be graded instead of reading a bare "no
// results" (SPEC §14). Only the package that produced the message can make
// that promise - hidden scrubs every hidden-tests failure to one fixed
// wording - so anything unmarked stays operator detail: a docker failure names
// the image and quotes the daemon, a prepare failure quotes a path inside the
// data dir, and neither may reach a student.
func Public(err error) error { return &publicError{err} }

type publicError struct{ err error }

func (e *publicError) Error() string { return e.err.Error() }
func (e *publicError) Unwrap() error { return e.err }

// publicNote returns the student-safe projection of a failure, "" when it
// carries none. A marked error wrapped in further context yields only the
// marked part, so context added on the way out stays with the teacher.
func publicNote(cause error) string {
	if pe, ok := errors.AsType[*publicError](cause); ok {
		return pe.Error()
	}
	return ""
}

// The shipped retry schedule for infra errors (SPEC §13). Exported because
// `serve` lets an operator widen it and has to advertise in its own help what
// it passes on when they do not: a default that lived only inside init() would
// drift from the one the flags print.
const (
	DefaultBackoffBase = 10 * time.Second
	DefaultBackoffCap  = 5 * time.Minute
	DefaultMaxRetries  = 8
)

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
	Notify    Notifier  // optional terminal-outcome sink; nil = disabled

	// Backoff schedule for infra errors: min(Base<<retries, Cap), then after
	// MaxRetries the submission becomes terminal infra_error (SPEC §13).
	// Zero means the shipped default; `serve` fills all three from its flags.
	BackoffBase time.Duration // default DefaultBackoffBase
	BackoffCap  time.Duration // default DefaultBackoffCap
	MaxRetries  int           // default DefaultMaxRetries

	// PollInterval is the claim-loop wakeup for retry_at; default 1s. It is
	// not an operator knob: a scheduled retry is picked up on the next tick,
	// so it is the floor under every backoff and means nothing on its own.
	PollInterval time.Duration

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
			q.BackoffBase = DefaultBackoffBase
		}
		if q.BackoffCap <= 0 {
			q.BackoffCap = DefaultBackoffCap
		}
		if q.MaxRetries <= 0 {
			q.MaxRetries = DefaultMaxRetries
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

// complete reports a terminal outcome to the optional completion sink. It is
// called after the row is written and outside every transaction: a delivery
// must never be something a submission's status can wait on, let alone depend
// on (AGENTS.md: no transaction is held across slow work).
func (q *Queue) complete(sub store.Submission, status string, raw, penalty, final float64) {
	if q.Notify != nil {
		q.Notify.Completed(Completion{
			SubID: sub.ID, UserID: sub.UserID, TaskID: sub.TaskID, Status: status,
			Raw: raw, Penalty: penalty, Final: final,
		})
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q.trackStart(sub.ID, cancel)
	defer q.trackEnd(sub.ID)
	// A cancel that arrived between the claim and the registration above found
	// no job to interrupt, so the row - not the in-memory marker - is what
	// says whether this submission is still ours to run. Read it before any
	// work starts; Cancel reads the registry again after its own write, so
	// between the two checks no cancel can slip through unnoticed.
	if q.canceledMeanwhile(ctx, sub.ID) {
		return
	}
	q.publish(sub, store.StatusRunning)

	p, err := q.Prep.Prepare(ctx, sub)
	if err != nil {
		if errors.Is(err, ErrTaskGone) {
			q.terminal(sub, err.Error())
			return
		}
		if te, ok := errors.AsType[*terminalError](err); ok {
			q.terminal(sub, te.msg)
			return
		}
		q.fail(ctx, sub, err)
		return
	}

	ws, err := runner.Assemble(ctx, p.Assembly)
	if err != nil {
		// The submitted content itself broke the workspace contract (symlinked
		// or oversized solution file): retrying it can only fail again, and the
		// teacher needs to see why.
		if te, ok := errors.AsType[*runner.TamperError](err); ok {
			q.terminal(sub, te.Error())
			return
		}
		q.fail(ctx, sub, err)
		return
	}
	defer ws.Close()

	r, err := q.NewRunner(p.Task.Runner)
	if err != nil {
		q.terminal(sub, err.Error()) // misconfigured runner: retrying won't help
		return
	}
	outcomes, err := r.Run(ctx, runner.Job{
		WorkspaceDir: ws.Root,
		TaskRelDir:   p.Assembly.TaskRelDir,
		Spec:         p.Task.Runner,
		Checks:       p.Task.Checks,
		LogDir:       p.LogDir,
		// What the hidden-tests overlay wrote, so the runner can take it back
		// out between the build and the run phases (SPEC §6.1).
		HiddenPaths: ws.HiddenPaths,
		HiddenDirs:  ws.HiddenDirs,
	})
	if err != nil {
		q.fail(ctx, sub, err)
		return
	}

	results := make([]scoring.CheckResult, len(outcomes))
	checks := make([]store.CheckRow, len(outcomes))
	for i, o := range outcomes {
		// A check that declared a `parser:` and produced a readable report
		// earns the passed proportion of its weight instead of all of it or
		// none; every other check leaves the tally at 0/0 and is scored by its
		// exit code exactly as before (SPEC §4.3).
		passedCases, scoredCases := testreport.Tally(o.Cases)
		results[i] = scoring.CheckResult{
			Name:        o.Name,
			Required:    p.Task.Checks[i].Required,
			Weight:      p.Task.Checks[i].Weight,
			Passed:      o.Passed,
			PassedCases: passedCases,
			ScoredCases: scoredCases,
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
			// Empty excerpt with a reason: the build phase is teacher-only.
			BuildFailed: o.BuildFailed,
			// No cases with a reason: the parser found no report to read, so
			// the excerpt above is all there is (SPEC §4.3).
			ParseFailed: o.ParseFailed,
			Cases:       caseRows(o.Cases),
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
		q.fail(ctx, sub, err)
		return
	}
	q.publish(sub, store.StatusDone)
	q.complete(sub, store.StatusDone, raw, pen, final)
}

// fail records a failed submission. A canceled execution context is not an
// infra fault: either the teacher canceled this submission - the row is
// already terminal - or the process is shutting down, and then the submission
// goes back to the queue without counting a retry (SPEC §13). Every cancel
// point lands here, so a shutdown during preparation or workspace assembly is
// classified exactly like one during the check run: the runner's own
// InfraError{Op: "canceled"} is only ever returned with ctx already canceled.
func (q *Queue) fail(ctx context.Context, sub store.Submission, cause error) {
	if ctx.Err() != nil {
		if !q.wasCanceled(sub.ID) {
			q.requeue(sub)
		}
		return
	}
	q.retry(sub, cause)
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
	// And read the registry once more AFTER the write. A worker that had
	// claimed the row but not yet registered its job when the first read
	// happened is registered by now, or it has not reached its own status
	// re-read yet - it registers before reading. Either this read finds the
	// job and kills it, or that read finds the canceled row and stops before
	// the check starts.
	q.mu.Lock()
	if cancel = q.running[id]; cancel != nil {
		q.canceling[id] = true
	}
	q.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	q.publish(sub, "canceled")
	// A cancel is a terminal outcome too, and an external system that only ever
	// hears about completions would otherwise wait on this submission forever.
	// The status reported is the row's own - infra_error, since there is no
	// 'canceled' status (SPEC §12) - never the UI's display wording.
	q.complete(sub, sub.Status, 0, 0, 0)
	return true, nil
}

// canceledMeanwhile reports whether the claimed row stopped being this
// worker's to run. Only a teacher cancel takes a claimed row out of 'running'
// while a worker holds it, so any change means the run must not start. A
// failed read is not treated as a cancel: dropping a live submission over a
// transient DB error would cost more than the one wasted run it saves.
func (q *Queue) canceledMeanwhile(ctx context.Context, id int64) bool {
	cur, _, err := q.Store.GetSubmission(ctx, id)
	if err != nil {
		return false
	}
	return cur.Status != store.StatusRunning || cur.CanceledAt != nil
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
func (q *Queue) retry(sub store.Submission, cause error) {
	if q.wasCanceled(sub.ID) {
		return // never resurrect a teacher-canceled row into the queue
	}
	note, student := cause.Error(), publicNote(cause)
	if sub.Retries >= q.MaxRetries {
		const exhausted = " (retries exhausted)"
		if student != "" {
			student += exhausted
		}
		q.scheduleRetry(sub, nil, note+exhausted, student)
		return
	}
	delay := min(q.BackoffBase<<sub.Retries, q.BackoffCap)
	// ±10% jitter avoids a thundering herd on a shared cause (docker down).
	delay += time.Duration((rand.Float64() - 0.5) * 0.2 * float64(delay))
	at := time.Now().Add(delay)
	q.scheduleRetry(sub, &at, note, student)
}

// terminal ends a submission with a note both audiences read: every caller
// hands over text about the course or the student's own commit - Terminal's
// own contract, a task that left the repo, an overlay the workspace refused, a
// runner type the metadata misnames - never infrastructure detail.
func (q *Queue) terminal(sub store.Submission, note string) {
	q.scheduleRetry(sub, nil, note, note)
}

// scheduleRetry publishes only what it managed to write: the store refuses the
// update once the row stopped being this worker's running submission (a
// teacher cancel landing after the in-memory marker was read), and an event
// for a status nobody wrote would contradict the row itself.
func (q *Queue) scheduleRetry(sub store.Submission, at *time.Time, note, studentNote string) {
	wctx, cancel := writeCtx()
	defer cancel()
	if ok, err := q.Store.ScheduleRetry(wctx, sub.ID, at, note, studentNote); err != nil || !ok {
		return
	}
	q.publish(sub, store.StatusInfraError)
	// A nil retry_at is the end of the line - no further attempt will change
	// this row - so this write is the submission's outcome, not a step on the
	// way to one. There are no scores: it never produced any.
	if at == nil {
		q.complete(sub, store.StatusInfraError, 0, 0, 0)
	}
}

// requeue survives ctx cancellation: it must run even during shutdown.
func (q *Queue) requeue(sub store.Submission) {
	wctx, cancel := writeCtx()
	defer cancel()
	_ = q.Store.Requeue(wctx, sub.ID)
	q.publish(sub, store.StatusQueued)
}

// writeCtx detaches a submission's final status write from its execution
// context: that context is already canceled whenever the write is caused by a
// shutdown or a teacher cancel, and a write made with it would silently do
// nothing, leaving the row stuck in running until the next restart.
func writeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// caseRows turns a parsed report into storage rows. No status translation is
// needed: testreport.Status carries exactly the strings the check_cases CHECK
// constraint allows, so the two cannot drift apart silently.
func caseRows(cases []testreport.Case) store.CaseRows {
	if len(cases) == 0 {
		return nil
	}
	rows := make(store.CaseRows, len(cases))
	for i, c := range cases {
		rows[i] = store.CaseRow{
			Name:     c.Name,
			Status:   string(c.Status),
			Duration: c.Duration,
			Message:  c.Message,
		}
	}
	return rows
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
