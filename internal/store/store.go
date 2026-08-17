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
	PushStore
	UserStore
	SessionStore
	AuditStore
	OverrideStore
	InviteStore
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

// SSHKey is one registered public key (SPEC §8).
type SSHKey struct {
	ID          int64
	UserID      int64
	Fingerprint string // SHA256:... (OpenSSH format)
	PublicKey   string // authorized_keys line
	CreatedAt   time.Time
}

// UserStore is the minimal account access M3 needs (M5/M6 extend it).
type UserStore interface {
	CreateUser(ctx context.Context, login, displayName, role string) (User, error)
	GetUserByLogin(ctx context.Context, login string) (User, error)
	GetUserByID(ctx context.Context, id int64) (User, error)
	ListUsers(ctx context.Context) ([]User, error)               // ordered by login
	SetUserState(ctx context.Context, login, state string) error // active | disabled; error if user unknown
	// IssueToken replaces any existing tokens of the user with one new
	// personal access token and returns its plaintext exactly once.
	IssueToken(ctx context.Context, userID int64) (string, error)
	// VerifyToken resolves a plaintext token to its ACTIVE user;
	// ok=false for unknown tokens and disabled users. Best-effort
	// bumps tokens.last_used_at.
	VerifyToken(ctx context.Context, plaintext string) (User, bool, error)
	AddSSHKey(ctx context.Context, userID int64, fingerprint, publicKey string) (SSHKey, error)
	ListSSHKeys(ctx context.Context, userID int64) ([]SSHKey, error)
	// UserByFingerprint resolves an SSH key fingerprint to its ACTIVE user.
	UserByFingerprint(ctx context.Context, fingerprint string) (User, bool, error)
	// DeleteSSHKey removes one key, scoped to its owner and pinned to the
	// fingerprint the caller saw; ok=false when nothing matched.
	DeleteSSHKey(ctx context.Context, userID, keyID int64, fingerprint string) (bool, error)
}

// SessionStore persists browser sessions (SPEC §8: token login → cookie).
type SessionStore interface {
	CreateSession(ctx context.Context, userID int64, tokenPlaintext string, ttl time.Duration) (string, error)
	LookupSession(ctx context.Context, id string) (User, bool, error)
	DeleteSession(ctx context.Context, id string) error
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
	// ListEventsByTarget returns recent events for "login" and "login/...",
	// newest first, capped at limit.
	ListEventsByTarget(ctx context.Context, login string, limit int) ([]EventRow, error)
	// ListEvents returns the global audit log, newest first, optionally
	// filtered by exact kind and/or a target substring ("" = no filter).
	ListEvents(ctx context.Context, kind, target string, limit, offset int) ([]EventRow, error)
	// ListEventKinds returns every distinct kind ever logged, for filters.
	ListEventKinds(ctx context.Context) ([]string, error)
}

// EventRow is one audit entry joined with its actor for display.
type EventRow struct {
	ActorLogin string // "" for system
	Kind       string
	Target     string
	Detail     string
	CreatedAt  time.Time
}

// ScoreOverride is a teacher-set manual score for (student, task) (SPEC §9).
type ScoreOverride struct {
	UserID    int64
	TaskID    string
	Score     float64
	Comment   string
	TeacherID int64
	CreatedAt time.Time
}

// OverrideStore persists manual score overrides; they win over computed
// scores (SPEC §9).
type OverrideStore interface {
	// SetScoreOverride inserts or replaces the override for (userID, taskID).
	SetScoreOverride(ctx context.Context, o ScoreOverride) error
	// DeleteScoreOverride removes it; deleting a missing one is a no-op.
	DeleteScoreOverride(ctx context.Context, userID int64, taskID string) error
	// GetScoreOverride: ok=false when none is set.
	GetScoreOverride(ctx context.Context, userID int64, taskID string) (ScoreOverride, bool, error)
	// ListScoreOverrides returns all overrides (matrix/CSV read model).
	ListScoreOverrides(ctx context.Context) ([]ScoreOverride, error)
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
	CanceledAt     *time.Time // teacher cancel timestamp (display only)
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
	Note    string // worker note, e.g. tamper notes (SPEC §6.1)
	LogDir  string
	Checks  []CheckRow
}

// Admission is the caller's verdict for one incoming submission, produced
// inside the transaction that records it. The policy itself lives in queue;
// store only needs to know which row to write.
type Admission struct {
	Admit        bool
	RejectStatus string // StatusRejectedDeadline | StatusRejectedLimit when !Admit
	RejectReason string // stored as the rejected row's note, English (SPEC §10.1)
	Counts       bool   // false = does not consume an attempt or start a cooldown
}

// SubmissionStore is the queue plus submission reads.
type SubmissionStore interface {
	// Enqueue inserts a queued row and assigns attempt_no race-free
	// (counting submissions only; teacher rechecks get AttemptNo nil).
	Enqueue(ctx context.Context, ns NewSubmission) (Submission, error)
	// AdmitSubmission reads the (user, task) history, hands it to decide, and
	// records the outcome - queued or rejected_* - in one transaction, so a
	// concurrent submission cannot decide against the same stale history.
	// decide must be pure and must not touch the store.
	AdmitSubmission(ctx context.Context, ns NewSubmission,
		decide func(history []Submission) Admission) (Submission, error)
	// ClaimNext atomically flips the oldest eligible row (queued, or
	// infra_error whose retry_at has passed) to running. ok=false when
	// nothing is claimable.
	ClaimNext(ctx context.Context, now time.Time) (sub Submission, ok bool, err error)
	// FinishSubmission writes the terminal status, scores, and all check
	// rows in one transaction.
	FinishSubmission(ctx context.Context, id int64, res SubmissionResult) error
	// ScheduleRetry records an infra_error for the row the caller is running;
	// retryAt nil marks it terminal (retries exhausted, surfaced in the
	// teacher queue view). ok=false when the row is no longer running or was
	// canceled meanwhile - the caller must not report a status it did not
	// write.
	ScheduleRetry(ctx context.Context, id int64, retryAt *time.Time, note string) (ok bool, err error)
	// RequeueRunning resets every running row to queued (startup recovery,
	// SPEC §5). Returns the number of rows requeued.
	RequeueRunning(ctx context.Context) (int, error)
	// Requeue returns one running row to queued (graceful worker shutdown);
	// unlike ScheduleRetry it does not touch retries.
	Requeue(ctx context.Context, id int64) error

	ListByUserTask(ctx context.Context, userID int64, taskID string) ([]Submission, error)
	// LastByUserTask returns the newest submission of the pair, of any
	// status; ok=false when the student has nothing recorded for the task.
	LastByUserTask(ctx context.Context, userID int64, taskID string) (Submission, bool, error)
	// ListByUser returns every submission of one user, ordered by task then
	// time (dashboard read model).
	ListByUser(ctx context.Context, userID int64) ([]Submission, error)
	GetSubmission(ctx context.Context, id int64) (Submission, []CheckRow, error)
	// NextRetryAt returns the earliest pending retry_at, nil if none.
	NextRetryAt(ctx context.Context) (*time.Time, error)
	// CancelSubmission marks a queued/running submission canceled by a
	// teacher: terminal infra_error, counts=0, canceled_at set (the status
	// CHECK has no 'canceled'; canceled_at is the display marker). ok=false
	// when the row is already terminal.
	CancelSubmission(ctx context.Context, id int64, now time.Time) (Submission, bool, error)
	// ListAllSubmissions returns every submission across all users, ordered
	// by user_id, task_id, received_at (matrix + CSV read model).
	ListAllSubmissions(ctx context.Context) ([]Submission, error)
	// ListActive returns queued, running, and infra_error submissions
	// (teacher queue view), newest first.
	ListActive(ctx context.Context) ([]Submission, error)
}

// Push is one accepted ref update on a student's graded branch (SPEC §13).
// The row is what makes a push an event rather than a ref position: it has an
// identity a force-push cycle cannot repeat, the boundaries of exactly this
// push, the time the server accepted it, and a processed marker.
type Push struct {
	ID         int64
	UserID     int64
	Ref        string
	OldSHA     string // the branch tip this push replaced; zero SHA on creation
	NewSHA     string
	ReceivedAt time.Time // server clock at hook arrival; the submission's own
	// ProcessedAt is nil while the push still has to be graded.
	ProcessedAt *time.Time
}

// NewPush is the intake payload; RecordPush assigns the ID.
type NewPush struct {
	UserID     int64
	Ref        string
	OldSHA     string
	NewSHA     string
	ReceivedAt time.Time
}

// PushStore persists the push log. Hook handlers are concurrent and can die
// mid-flight, so a push is recorded on arrival and graded afterwards: whoever
// holds the student's lock next drains every pending row in order.
type PushStore interface {
	// RecordPush appends an accepted ref update, unprocessed.
	RecordPush(ctx context.Context, np NewPush) (Push, error)
	// PendingPushes returns the student's ungraded pushes, oldest first.
	PendingPushes(ctx context.Context, userID int64) ([]Push, error)
	// MarkPushProcessed records that the push has been graded; the row is
	// never handed out by PendingPushes again.
	MarkPushProcessed(ctx context.Context, id int64, at time.Time) error
}

// Invite is a one-shot activation link for a tokenless user (SPEC §8).
type Invite struct {
	ID        int64
	UserID    int64
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// InviteStore persists hashed invite tokens.
type InviteStore interface {
	CreateInvite(ctx context.Context, userID int64, tokenPlaintext string, expiresAt time.Time) error
	// VerifyInvite resolves a plaintext token to an unused, unexpired invite.
	VerifyInvite(ctx context.Context, tokenPlaintext string) (Invite, bool, error)
	// ConsumeInvite burns the one-shot invite: ok=true for the single caller
	// that set used_at, false when someone else already did. Callers must
	// consume before any side effect of the activation.
	ConsumeInvite(ctx context.Context, id int64, now time.Time) (bool, error)
}
