package store

import "context"

// ListEventsByTarget implements AuditStore.
func (s *DB) ListEventsByTarget(ctx context.Context, login string, limit int) ([]EventRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.kind, e.target, e.detail, e.created_at, COALESCE(a.login, '')
		FROM events e LEFT JOIN users a ON a.id = e.actor_id
		WHERE e.target = ? OR e.target LIKE ? || '/%'
		ORDER BY e.created_at DESC LIMIT ?`,
		login, login, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EventRow
	for rows.Next() {
		var (
			ev        EventRow
			createdAt string
		)
		if err := rows.Scan(&ev.Kind, &ev.Target, &ev.Detail, &createdAt, &ev.ActorLogin); err != nil {
			return nil, err
		}
		if ev.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
