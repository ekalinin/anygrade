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

// ListEvents implements AuditStore: the global audit log, optionally
// filtered by exact kind and/or a target substring, newest first.
func (s *DB) ListEvents(ctx context.Context, kind, target string, limit, offset int) ([]EventRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.kind, e.target, e.detail, e.created_at, COALESCE(a.login, '')
		FROM events e LEFT JOIN users a ON a.id = e.actor_id
		WHERE (? = '' OR e.kind = ?) AND (? = '' OR e.target LIKE '%' || ? || '%')
		ORDER BY e.id DESC LIMIT ? OFFSET ?`,
		kind, kind, target, target, limit, offset)
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

// ListEventKinds implements AuditStore: the distinct kinds ever logged, for
// building the audit page's filter dropdown.
func (s *DB) ListEventKinds(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT kind FROM events ORDER BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return nil, err
		}
		out = append(out, kind)
	}
	return out, rows.Err()
}
