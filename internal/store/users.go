package store

import (
	"context"
	"time"
)

// CreateUser implements UserStore.
func (s *DB) CreateUser(ctx context.Context, login, displayName, role string) (User, error) {
	now := time.Now()
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO users (login, display_name, role, created_at)
		VALUES (?, ?, ?, ?)
		RETURNING id, login, display_name, role, state, created_at`,
		login, displayName, role, fmtTime(now))
	return scanUser(row)
}

// GetUserByLogin implements UserStore.
func (s *DB) GetUserByLogin(ctx context.Context, login string) (User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, login, display_name, role, state, created_at
		FROM users WHERE login = ?`, login)
	return scanUser(row)
}

// GetUserByID implements UserStore.
func (s *DB) GetUserByID(ctx context.Context, id int64) (User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, login, display_name, role, state, created_at
		FROM users WHERE id = ?`, id)
	return scanUser(row)
}

func scanUser(row scanner) (User, error) {
	var u User
	var createdAt string
	if err := row.Scan(&u.ID, &u.Login, &u.DisplayName, &u.Role, &u.State, &createdAt); err != nil {
		return User{}, err
	}
	var err error
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return User{}, err
	}
	return u, nil
}

// Log implements AuditStore.
//
// The actor's role is read here instead of being passed in: Log runs inside
// the request that took the action, so the subquery records the role held at
// that moment - which is the point of storing it at all - and no caller can
// forget it. A system event (nil actor) and an actor whose account is gone
// both leave the role empty, exactly like rows written before it existed.
func (s *DB) Log(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO events (actor_id, actor_role, kind, target, detail, created_at)
		VALUES (?, COALESCE((SELECT role FROM users WHERE id = ?), ''), ?, ?, ?, ?)`,
		e.ActorID, e.ActorID, e.Kind, e.Target, e.Detail, fmtTime(time.Now()))
	return err
}
