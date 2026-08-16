package store

import (
	"context"
	"time"
)

const pushCols = `id, user_id, ref, old_sha, new_sha, received_at, processed_at`

// RecordPush implements PushStore.
func (s *DB) RecordPush(ctx context.Context, np NewPush) (Push, error) {
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO pushes (user_id, ref, old_sha, new_sha, received_at)
		VALUES (?, ?, ?, ?, ?)
		RETURNING `+pushCols,
		np.UserID, np.Ref, np.OldSHA, np.NewSHA, fmtTime(np.ReceivedAt))
	return scanPush(row)
}

// PendingPushes implements PushStore.
func (s *DB) PendingPushes(ctx context.Context, userID int64) ([]Push, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+pushCols+` FROM pushes
		WHERE user_id = ? AND processed_at IS NULL
		ORDER BY id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Push
	for rows.Next() {
		p, err := scanPush(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkPushProcessed implements PushStore.
func (s *DB) MarkPushProcessed(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pushes SET processed_at = ? WHERE id = ?`, fmtTime(at), id)
	return err
}

func scanPush(sc scanner) (Push, error) {
	var (
		p           Push
		receivedAt  string
		processedAt *string
	)
	if err := sc.Scan(&p.ID, &p.UserID, &p.Ref, &p.OldSHA, &p.NewSHA,
		&receivedAt, &processedAt); err != nil {
		return Push{}, err
	}
	var err error
	if p.ReceivedAt, err = parseTime(receivedAt); err != nil {
		return Push{}, err
	}
	if p.ProcessedAt, err = parseTimePtr(processedAt); err != nil {
		return Push{}, err
	}
	return p, nil
}
