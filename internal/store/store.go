// Package store persists anygrade state in SQLite (SPEC §12). The submission
// table doubles as the check queue (SPEC §5): workers claim queued rows
// atomically and write results back. All timestamps are stored as RFC 3339
// UTC strings so lexicographic order equals chronological order.
package store

import (
	"context"
	"io"
	"time"
)

// Submission statuses (SPEC §6, §12).
const (
	StatusQueued           = "queued"
	StatusRunning          = "running"
	StatusDone             = "done"
	StatusInfraError       = "infra_error"
	StatusRejectedDeadline = "rejected_deadline"
	StatusRejectedLimit    = "rejected_limit"
)

// Store is the full persistence interface; *DB is the only implementation.
type Store interface {
	SubmissionStore
	UserStore
	AuditStore
	io.Closer
}

// User is a course account (student or teacher).
type User struct {
	ID          int64
	Login       string
	DisplayName string
	Role        string // student | teacher
	State       string // active | disabled
	CreatedAt   time.Time
}

// UserStore is the minimal account access M3 needs (M5/M6 extend it).
type UserStore interface {
	CreateUser(ctx context.Context, login, displayName, role string) (User, error)
	GetUserByLogin(ctx context.Context, login string) (User, error)
}

// Event is one audit-log entry (SPEC §12).
type Event struct {
	ActorID *int64
	Kind    string
	Target  string
	Detail  string
}

// AuditStore records user/teacher actions.
type AuditStore interface {
	Log(ctx context.Context, e Event) error
}

// Submission is one graded unit: (student, task, commit) (SPEC §3, §12).
type Submission struct {
	ID             int64
	UserID         int64
	TaskID         string
	CommitSHA      string
	ReceivedAt     time.Time // server clock; penalty reference (SPEC §9)
	AttemptNo      *int      // nil for non-counting submissions (teacher recheck)
	Counts         bool      // false = teacher recheck: no attempt/cooldown impact
	Status         string
	RawScore       *float64
	PenaltyPercent *float64
	FinalScore     *float64
	LogDir         string
	WorkerNote     string
	Retries        int        // infra_error retry counter
	RetryAt        *time.Time // next eligible claim time; nil = none/terminal
	StartedAt      *time.Time
}

// NewSubmission is the intake payload; Enqueue assigns ID and AttemptNo.
type NewSubmission struct {
	UserID     int64
	TaskID     string
	CommitSHA  string
	ReceivedAt time.Time
	Counts     bool
}

// CheckRow is one persisted check result (SPEC §12).
type CheckRow struct {
	Name       string
	Passed     bool
	ExitCode   int
	Duration   time.Duration
	Weight     int
	Skipped    bool
	TimedOut   bool
	LogExcerpt string
}

// SubmissionResult is the terminal outcome written by a worker.
type SubmissionResult struct {
	Status  string // StatusDone
	Raw     float64
	Penalty float64
	Final   float64
	Note    string
	Checks  []CheckRow
}

// SubmissionStore is the queue plus submission reads.
type SubmissionStore interface {
	// Enqueue inserts a queued row and assigns attempt_no race-free
	// (counting submissions only; teacher rechecks get AttemptNo nil).
	Enqueue(ctx context.Context, ns NewSubmission) (Submission, error)
	// RecordRejected persists a rejected_deadline/rejected_limit row (not queued).
	RecordRejected(ctx context.Context, ns NewSubmission, status string) (Submission, error)
	// ClaimNext atomically flips the oldest eligible row (queued, or
	// infra_error whose retry_at has passed) to running. ok=false when
	// nothing is claimable.
	ClaimNext(ctx context.Context, now time.Time) (sub Submission, ok bool, err error)
	// FinishSubmission writes the terminal status, scores, and all check
	// rows in one transaction.
	FinishSubmission(ctx context.Context, id int64, res SubmissionResult) error
	// ScheduleRetry records an infra_error; retryAt nil marks it terminal
	// (retries exhausted, surfaced in the teacher queue view).
	ScheduleRetry(ctx context.Context, id int64, retryAt *time.Time, note string) error
	// RequeueRunning resets every running row to queued (startup recovery,
	// SPEC §5). Returns the number of rows requeued.
	RequeueRunning(ctx context.Context) (int, error)
	// Requeue returns one running row to queued (graceful worker shutdown);
	// unlike ScheduleRetry it does not touch retries.
	Requeue(ctx context.Context, id int64) error

	ListByUserTask(ctx context.Context, userID int64, taskID string) ([]Submission, error)
	GetSubmission(ctx context.Context, id int64) (Submission, []CheckRow, error)
	// NextRetryAt returns the earliest pending retry_at, nil if none.
	NextRetryAt(ctx context.Context) (*time.Time, error)
}
