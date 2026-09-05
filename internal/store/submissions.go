package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const submissionCols = `id, user_id, task_id, commit_sha, received_at, attempt_no,
	counts, status, raw_score, penalty_percent, final_score, log_dir,
	worker_note, student_note, retries, retry_at, started_at, canceled_at`

// dbtx is the subset of *sql.DB and *sql.Tx the submission queries need, so
// one implementation serves both the standalone and the transactional path.
type dbtx interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AdmitSubmission implements SubmissionStore: history read, policy decision,
// and insert happen inside one transaction, so two concurrent pushes (or a
// push racing a UI recheck) cannot both claim the last free attempt slot.
//
// The DSN sets _txlock=immediate, so the write lock is taken before the
// history is read: no other writer can slip in between the SELECT and the
// INSERT, in this process or another one.
//
// decide runs while the single write connection is held. It must be pure and
// fast, and must never touch the store - that would deadlock on the one
// connection this transaction owns.
func (s *DB) AdmitSubmission(ctx context.Context, ns NewSubmission,
	decide func(history []Submission) Admission) (Submission, error) {

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Submission{}, err
	}
	defer tx.Rollback()

	history, err := listByUserTask(ctx, tx, ns.UserID, ns.TaskID)
	if err != nil {
		return Submission{}, err
	}
	a := decide(history)
	// The verdict owns Counts: taking it from the caller's NewSubmission
	// would let the row disagree with the decision that produced it.
	ns.Counts = a.Counts

	var sub Submission
	if a.Admit {
		sub, err = enqueueRow(ctx, tx, ns)
	} else {
		sub, err = rejectedRow(ctx, tx, ns, a.RejectStatus, a.RejectReason)
	}
	if err != nil {
		return Submission{}, err
	}
	if err := tx.Commit(); err != nil {
		return Submission{}, err
	}
	return sub, nil
}

// Enqueue implements SubmissionStore. attempt_no is assigned inside the INSERT
// so concurrent enqueues cannot race (all writes serialize on one connection).
func (s *DB) Enqueue(ctx context.Context, ns NewSubmission) (Submission, error) {
	return enqueueRow(ctx, s.db, ns)
}

func enqueueRow(ctx context.Context, q dbtx, ns NewSubmission) (Submission, error) {
	row := q.QueryRowContext(ctx, `
		INSERT INTO submissions
		  (user_id, task_id, commit_sha, received_at, counts, attempt_no, status)
		VALUES (?, ?, ?, ?, ?,
		  CASE WHEN ? = 1
		    THEN (SELECT COALESCE(MAX(attempt_no), 0) + 1 FROM submissions
		          WHERE user_id = ? AND task_id = ? AND counts = 1)
		    ELSE NULL END,
		  'queued')
		RETURNING `+submissionCols,
		ns.UserID, ns.TaskID, ns.CommitSHA, fmtTime(ns.ReceivedAt), ns.Counts,
		ns.Counts, ns.UserID, ns.TaskID)
	return scanSubmission(row)
}

// rejectedRow persists a submission the policy refused: recorded for the
// student's history, never queued (SPEC §6 step 4). The reason goes into
// worker_note - the row never reaches a worker, so nothing overwrites it, and
// the student sees why the submission was refused instead of a bare status.
// It is the same text the push output already carried, so it is the student's
// note too.
func rejectedRow(ctx context.Context, q dbtx, ns NewSubmission, status, reason string) (Submission, error) {
	if status != StatusRejectedDeadline && status != StatusRejectedLimit {
		return Submission{}, fmt.Errorf("rejected submission: invalid status %q", status)
	}
	row := q.QueryRowContext(ctx, `
		INSERT INTO submissions
		  (user_id, task_id, commit_sha, received_at, counts, attempt_no, status,
		   worker_note, student_note)
		VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?)
		RETURNING `+submissionCols,
		ns.UserID, ns.TaskID, ns.CommitSHA, fmtTime(ns.ReceivedAt), ns.Counts, status,
		reason, reason)
	return scanSubmission(row)
}

// ClaimNext implements SubmissionStore: one atomic UPDATE...RETURNING flips the
// oldest eligible row to running. Serialized by MaxOpenConns(1), so concurrent
// workers always claim distinct rows.
//
// A row is skipped while an *earlier* submission of the same (student, task)
// is still in flight: successive pushes to one task run in order rather than
// racing each other (SPEC §13). In flight means running, or waiting on a retry
// backoff - an infra_error that will run again is unfinished work, and letting
// a newer submission past it would put the pair's results out of order. The
// row stays queued and is picked up on a later claim, so other students and
// other tasks keep flowing past it.
//
// The blocker predicate is strictly "earlier", so the oldest row of a pair is
// never blocked by its own successors and the queue cannot stall: the earliest
// one runs, and each retry either completes or exhausts its budget into a
// terminal infra_error (retry_at NULL), which blocks nobody.
func (s *DB) ClaimNext(ctx context.Context, now time.Time) (Submission, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE submissions SET status = 'running', started_at = ?
		WHERE id = (
		  SELECT s.id FROM submissions s
		  WHERE (s.status = 'queued'
		     OR (s.status = 'infra_error' AND s.retry_at IS NOT NULL AND s.retry_at <= ?))
		    AND NOT EXISTS (
		      SELECT 1 FROM submissions r
		      WHERE r.user_id = s.user_id
		        AND r.task_id = s.task_id
		        AND (r.status = 'running'
		          OR (r.status = 'infra_error' AND r.retry_at IS NOT NULL
		            AND (r.received_at < s.received_at
		              OR (r.received_at = s.received_at AND r.id < s.id))))
		    )
		  ORDER BY s.received_at ASC, s.id ASC
		  LIMIT 1
		)
		RETURNING `+submissionCols,
		fmtTime(now), fmtTime(now))
	sub, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Submission{}, false, nil
	}
	if err != nil {
		return Submission{}, false, err
	}
	return sub, true, nil
}

// FinishSubmission implements SubmissionStore: terminal status, scores, and
// check rows commit atomically, so a reader never sees a done submission
// without its check results.
func (s *DB) FinishSubmission(ctx context.Context, id int64, res SubmissionResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Idempotency guard for a rare double-finish after requeue. The case rows
	// go with it through the cascade: they detail a check result and outlive
	// it in no reading.
	if _, err := tx.ExecContext(ctx, `DELETE FROM check_results WHERE submission_id = ?`, id); err != nil {
		return err
	}
	for _, c := range res.Checks {
		var checkID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO check_results
			  (submission_id, name, passed, exit_code, duration_ms, weight,
			   skipped, timed_out, log_excerpt, build_failed, parse_failed)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id`,
			id, c.Name, c.Passed, c.ExitCode, c.Duration.Milliseconds(),
			c.Weight, c.Skipped, c.TimedOut, c.LogExcerpt, c.BuildFailed,
			c.ParseFailed).Scan(&checkID); err != nil {
			return err
		}
		for _, cs := range c.Cases {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO check_cases
				  (check_result_id, name, status, duration_ms, message)
				VALUES (?, ?, ?, ?, ?)`,
				checkID, cs.Name, cs.Status, cs.Duration.Milliseconds(), cs.Message); err != nil {
				return err
			}
		}
	}
	// The status guard closes the teacher-cancel race: once CancelSubmission
	// flipped the row terminal, a late finish must not resurrect it as done.
	// A finished run's note is the prep's tamper notes: they describe the
	// student's own discarded edits, so both audiences read the same text.
	result, err := tx.ExecContext(ctx, `
		UPDATE submissions SET status = ?, raw_score = ?, penalty_percent = ?,
		  final_score = ?, worker_note = ?, student_note = ?, log_dir = ?, retry_at = NULL
		WHERE id = ? AND status = 'running' AND canceled_at IS NULL`,
		res.Status, res.Raw, res.Penalty, res.Final, res.Note, res.Note, res.LogDir, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("FinishSubmission: submission %d not found or not running", id)
	}
	return tx.Commit()
}

// ScheduleRetry implements SubmissionStore. The guard closes the teacher-cancel
// race: the worker decides to retry, the teacher cancels in between (terminal
// row, counts=0, its own note), and an unconditional update would overwrite the
// note, count a retry and re-arm retry_at - handing the canceled submission
// back to ClaimNext. ok=false says the row was no longer the caller's to write.
func (s *DB) ScheduleRetry(ctx context.Context, id int64, retryAt *time.Time, note, studentNote string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE submissions SET status = 'infra_error', retries = retries + 1,
		  retry_at = ?, worker_note = ?, student_note = ?
		WHERE id = ? AND status = 'running' AND canceled_at IS NULL`,
		fmtTimePtr(retryAt), note, studentNote, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RequeueRunning implements SubmissionStore (startup recovery, SPEC §5).
func (s *DB) RequeueRunning(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE submissions SET status = 'queued', started_at = NULL WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// Requeue implements SubmissionStore (graceful worker shutdown: no retry
// counting, the submission simply goes back to the queue).
func (s *DB) Requeue(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE submissions SET status = 'queued', started_at = NULL
		 WHERE id = ? AND status = 'running' AND canceled_at IS NULL`, id)
	return err
}

// ListByUserTask implements SubmissionStore.
func (s *DB) ListByUserTask(ctx context.Context, userID int64, taskID string) ([]Submission, error) {
	return listByUserTask(ctx, s.db, userID, taskID)
}

func listByUserTask(ctx context.Context, q dbtx, userID int64, taskID string) ([]Submission, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+submissionCols+` FROM submissions
		WHERE user_id = ? AND task_id = ? ORDER BY received_at ASC`, userID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Submission
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// LastByUserTask implements SubmissionStore: the newest row of the pair, of
// any status. ok=false when the student has nothing recorded for the task.
func (s *DB) LastByUserTask(ctx context.Context, userID int64, taskID string) (Submission, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+submissionCols+` FROM submissions
		WHERE user_id = ? AND task_id = ?
		ORDER BY received_at DESC, id DESC LIMIT 1`, userID, taskID)
	sub, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Submission{}, false, nil
	}
	if err != nil {
		return Submission{}, false, err
	}
	return sub, true, nil
}

// ListByUser implements SubmissionStore.
func (s *DB) ListByUser(ctx context.Context, userID int64) ([]Submission, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+submissionCols+` FROM submissions
		WHERE user_id = ? ORDER BY task_id ASC, received_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Submission
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// GetSubmission implements SubmissionStore.
func (s *DB) GetSubmission(ctx context.Context, id int64) (Submission, []CheckRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+submissionCols+` FROM submissions WHERE id = ?`, id)
	sub, err := scanSubmission(row)
	if err != nil {
		return Submission{}, nil, err
	}
	// Two queries, each closing its rows before the next one starts: the pool
	// holds a single connection, so a second query issued while the first Rows
	// is open would wait for a connection this call is holding itself.
	checks, byID, err := checkRows(ctx, s.db, id)
	if err != nil {
		return Submission{}, nil, err
	}
	if len(checks) > 0 {
		if err := attachCaseRows(ctx, s.db, id, checks, byID); err != nil {
			return Submission{}, nil, err
		}
	}
	return sub, checks, nil
}

// checkRows reads one submission's check results and, alongside them, where
// each row's id sits in the slice - which is what the case rows are keyed by.
func checkRows(ctx context.Context, q dbtx, id int64) ([]CheckRow, map[int64]int, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, name, passed, exit_code, duration_ms, weight, skipped, timed_out,
		       log_excerpt, build_failed, parse_failed
		FROM check_results WHERE submission_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var checks []CheckRow
	byID := map[int64]int{}
	for rows.Next() {
		var (
			c        CheckRow
			checkID  int64
			duration int64
		)
		if err := rows.Scan(&checkID, &c.Name, &c.Passed, &c.ExitCode, &duration, &c.Weight,
			&c.Skipped, &c.TimedOut, &c.LogExcerpt, &c.BuildFailed, &c.ParseFailed); err != nil {
			return nil, nil, err
		}
		c.Duration = time.Duration(duration) * time.Millisecond
		byID[checkID] = len(checks)
		checks = append(checks, c)
	}
	return checks, byID, rows.Err()
}

// attachCaseRows fills in the per-test-case detail of every check of one
// submission in a single pass, ordered by the case rows' own ids so the order
// the report listed them in survives the round trip.
func attachCaseRows(ctx context.Context, q dbtx, id int64, checks []CheckRow, byID map[int64]int) error {
	rows, err := q.QueryContext(ctx, `
		SELECT c.check_result_id, c.name, c.status, c.duration_ms, c.message
		FROM check_cases c JOIN check_results r ON r.id = c.check_result_id
		WHERE r.submission_id = ? ORDER BY c.id ASC`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			checkID  int64
			cs       CaseRow
			duration int64
		)
		if err := rows.Scan(&checkID, &cs.Name, &cs.Status, &duration, &cs.Message); err != nil {
			return err
		}
		i, ok := byID[checkID]
		if !ok {
			continue // orphan row: the check it detailed is gone
		}
		cs.Duration = time.Duration(duration) * time.Millisecond
		checks[i].Cases = append(checks[i].Cases, cs)
	}
	return rows.Err()
}

// NextRetryAt implements SubmissionStore.
func (s *DB) NextRetryAt(ctx context.Context) (*time.Time, error) {
	var v *string
	err := s.db.QueryRowContext(ctx, `
		SELECT MIN(retry_at) FROM submissions
		WHERE status = 'infra_error' AND retry_at IS NOT NULL`).Scan(&v)
	if err != nil {
		return nil, err
	}
	return parseTimePtr(v)
}

// CancelSubmission implements SubmissionStore.
func (s *DB) CancelSubmission(ctx context.Context, id int64, now time.Time) (Submission, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE submissions SET status = 'infra_error', retry_at = NULL, counts = 0,
		  canceled_at = ?, worker_note = 'canceled by teacher',
		  student_note = 'canceled by teacher'
		WHERE id = ? AND status IN ('queued','running')
		RETURNING `+submissionCols,
		fmtTime(now), id)
	sub, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Submission{}, false, nil
	}
	if err != nil {
		return Submission{}, false, err
	}
	return sub, true, nil
}

// ListAllSubmissions implements SubmissionStore.
func (s *DB) ListAllSubmissions(ctx context.Context) ([]Submission, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+submissionCols+` FROM submissions
		ORDER BY user_id ASC, task_id ASC, received_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Submission
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// ListActive implements SubmissionStore (teacher queue view).
func (s *DB) ListActive(ctx context.Context) ([]Submission, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+submissionCols+` FROM submissions
		WHERE status IN ('queued','running','infra_error')
		ORDER BY received_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Submission
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// scanner covers both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanSubmission(row scanner) (Submission, error) {
	var (
		sub                            Submission
		receivedAt                     string
		retryAt, startedAt, canceledAt *string
	)
	err := row.Scan(&sub.ID, &sub.UserID, &sub.TaskID, &sub.CommitSHA, &receivedAt,
		&sub.AttemptNo, &sub.Counts, &sub.Status, &sub.RawScore, &sub.PenaltyPercent,
		&sub.FinalScore, &sub.LogDir, &sub.WorkerNote, &sub.StudentNote, &sub.Retries,
		&retryAt, &startedAt, &canceledAt)
	if err != nil {
		return Submission{}, err
	}
	if sub.ReceivedAt, err = parseTime(receivedAt); err != nil {
		return Submission{}, err
	}
	if sub.RetryAt, err = parseTimePtr(retryAt); err != nil {
		return Submission{}, err
	}
	if sub.StartedAt, err = parseTimePtr(startedAt); err != nil {
		return Submission{}, err
	}
	if sub.CanceledAt, err = parseTimePtr(canceledAt); err != nil {
		return Submission{}, err
	}
	return sub, nil
}
