package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SetScoreOverride implements OverrideStore.
func (s *DB) SetScoreOverride(ctx context.Context, o ScoreOverride) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO score_overrides (user_id, task_id, score, comment, teacher_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, task_id) DO UPDATE SET
			score = excluded.score,
			comment = excluded.comment,
			teacher_id = excluded.teacher_id,
			created_at = excluded.created_at`,
		o.UserID, o.TaskID, o.Score, o.Comment, o.TeacherID, fmtTime(time.Now()))
	return err
}

// DeleteScoreOverride implements OverrideStore.
func (s *DB) DeleteScoreOverride(ctx context.Context, userID int64, taskID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM score_overrides WHERE user_id = ? AND task_id = ?`,
		userID, taskID)
	return err
}

// GetScoreOverride implements OverrideStore.
func (s *DB) GetScoreOverride(ctx context.Context, userID int64, taskID string) (ScoreOverride, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT user_id, task_id, score, comment, teacher_id, created_at
		FROM score_overrides WHERE user_id = ? AND task_id = ?`,
		userID, taskID)
	o, err := scanScoreOverride(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ScoreOverride{}, false, nil
	}
	if err != nil {
		return ScoreOverride{}, false, err
	}
	return o, true, nil
}

// ListScoreOverrides implements OverrideStore.
func (s *DB) ListScoreOverrides(ctx context.Context) ([]ScoreOverride, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id, task_id, score, comment, teacher_id, created_at
		FROM score_overrides ORDER BY user_id, task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ScoreOverride
	for rows.Next() {
		o, err := scanScoreOverride(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func scanScoreOverride(row scanner) (ScoreOverride, error) {
	var o ScoreOverride
	var createdAt string
	if err := row.Scan(&o.UserID, &o.TaskID, &o.Score, &o.Comment, &o.TeacherID, &createdAt); err != nil {
		return ScoreOverride{}, err
	}
	var err error
	if o.CreatedAt, err = parseTime(createdAt); err != nil {
		return ScoreOverride{}, err
	}
	return o, nil
}
