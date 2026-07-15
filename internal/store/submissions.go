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
	worker_note, retries, retry_at, started_at, canceled_at`

// Enqueue implements SubmissionStore. attempt_no is assigned inside the INSERT
// so concurrent enqueues cannot race (all writes serialize on one connection).
func (s *DB) Enqueue(ctx context.Context, ns NewSubmission) (Submission, error) {
	row := s.db.QueryRowContext(ctx, `
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

// RecordRejected implements SubmissionStore.
func (s *DB) RecordRejected(ctx context.Context, ns NewSubmission, status string) (Submission, error) {
	if status != StatusRejectedDeadline && status != StatusRejectedLimit {
		return Submission{}, fmt.Errorf("RecordRejected: invalid status %q", status)
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO submissions
		  (user_id, task_id, commit_sha, received_at, counts, attempt_no, status)
		VALUES (?, ?, ?, ?, ?, NULL, ?)
		RETURNING `+submissionCols,
		ns.UserID, ns.TaskID, ns.CommitSHA, fmtTime(ns.ReceivedAt), ns.Counts, status)
	return scanSubmission(row)
}

// ClaimNext implements SubmissionStore: one atomic UPDATE...RETURNING flips the
// oldest eligible row to running. Serialized by MaxOpenConns(1), so concurrent
// workers always claim distinct rows.
func (s *DB) ClaimNext(ctx context.Context, now time.Time) (Submission, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE submissions SET status = 'running', started_at = ?
		WHERE id = (
		  SELECT id FROM submissions
		  WHERE status = 'queued'
		     OR (status = 'infra_error' AND retry_at IS NOT NULL AND retry_at <= ?)
		  ORDER BY received_at ASC
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

	// Idempotency guard for a rare double-finish after requeue.
	if _, err := tx.ExecContext(ctx, `DELETE FROM check_results WHERE submission_id = ?`, id); err != nil {
		return err
	}
	for _, c := range res.Checks {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO check_results
			  (submission_id, name, passed, exit_code, duration_ms, weight,
			   skipped, timed_out, log_excerpt)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, c.Name, c.Passed, c.ExitCode, c.Duration.Milliseconds(),
			c.Weight, c.Skipped, c.TimedOut, c.LogExcerpt); err != nil {
			return err
		}
	}
	// The status guard closes the teacher-cancel race: once CancelSubmission
	// flipped the row terminal, a late finish must not resurrect it as done.
	result, err := tx.ExecContext(ctx, `
		UPDATE submissions SET status = ?, raw_score = ?, penalty_percent = ?,
		  final_score = ?, worker_note = ?, log_dir = ?, retry_at = NULL
		WHERE id = ? AND status = 'running'`,
		res.Status, res.Raw, res.Penalty, res.Final, res.Note, res.LogDir, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("FinishSubmission: submission %d not found or not running", id)
	}
	return tx.Commit()
}

// ScheduleRetry implements SubmissionStore.
func (s *DB) ScheduleRetry(ctx context.Context, id int64, retryAt *time.Time, note string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE submissions SET status = 'infra_error', retries = retries + 1,
		  retry_at = ?, worker_note = ?
		WHERE id = ?`,
		fmtTimePtr(retryAt), note, id)
	return err
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
		`UPDATE submissions SET status = 'queued', started_at = NULL WHERE id = ? AND status = 'running'`, id)
	return err
}

// ListByUserTask implements SubmissionStore.
func (s *DB) ListByUserTask(ctx context.Context, userID int64, taskID string) ([]Submission, error) {
	rows, err := s.db.QueryContext(ctx, `
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
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, passed, exit_code, duration_ms, weight, skipped, timed_out, log_excerpt
		FROM check_results WHERE submission_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return Submission{}, nil, err
	}
	defer rows.Close()
	var checks []CheckRow
	for rows.Next() {
		var c CheckRow
		var durMS int64
		if err := rows.Scan(&c.Name, &c.Passed, &c.ExitCode, &durMS, &c.Weight,
			&c.Skipped, &c.TimedOut, &c.LogExcerpt); err != nil {
			return Submission{}, nil, err
		}
		c.Duration = time.Duration(durMS) * time.Millisecond
		checks = append(checks, c)
	}
	return sub, checks, rows.Err()
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
		  canceled_at = ?, worker_note = 'canceled by teacher'
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
		&sub.FinalScore, &sub.LogDir, &sub.WorkerNote, &sub.Retries, &retryAt, &startedAt,
		&canceledAt)
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
