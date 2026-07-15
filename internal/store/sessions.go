package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

// CreateSession opens a browser session for a user whose token was just
// verified. The session is bound to the token hash: resetting the token
// revokes every session created from it (SPEC §8).
func (s *DB) CreateSession(ctx context.Context, userID int64, tokenPlaintext string, ttl time.Duration) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(buf)

	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		id, userID, hashToken(tokenPlaintext), fmtTime(now), fmtTime(now.Add(ttl)))
	if err != nil {
		return "", err
	}
	return id, nil
}

// LookupSession resolves a session cookie to its ACTIVE user. ok=false for
// unknown/expired sessions, disabled users, and sessions whose token was
// reset (the tokens JOIN fails).
func (s *DB) LookupSession(ctx context.Context, id string) (User, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.login, u.display_name, u.role, u.state, u.created_at
		FROM sessions s
		JOIN users  u ON u.id = s.user_id
		JOIN tokens t ON t.user_id = s.user_id AND t.hash = s.token_hash
		WHERE s.id = ? AND s.expires_at > ? AND u.state = 'active'`,
		id, fmtTime(time.Now()))
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

// DeleteSession logs the session out; deleting an unknown id is a no-op.
func (s *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}
