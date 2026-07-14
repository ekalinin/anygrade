package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// newTokenPlaintext returns a fresh "ag_"-prefixed random token.
func newTokenPlaintext() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "ag_" + hex.EncodeToString(buf), nil
}

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// IssueToken implements UserStore: a user has at most one active token.
func (s *DB) IssueToken(ctx context.Context, userID int64) (string, error) {
	plaintext, err := newTokenPlaintext()
	if err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE user_id = ?`, userID); err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tokens (user_id, hash, created_at)
		VALUES (?, ?, ?)`,
		userID, hashToken(plaintext), fmtTime(time.Now()))
	if err != nil {
		return "", err
	}
	return plaintext, nil
}

// VerifyToken implements UserStore.
func (s *DB) VerifyToken(ctx context.Context, plaintext string) (User, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT users.id, users.login, users.display_name, users.role, users.state, users.created_at
		FROM tokens JOIN users ON users.id = tokens.user_id
		WHERE tokens.hash = ? AND users.state = 'active'`,
		hashToken(plaintext))
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	// Best effort: last_used_at is informational only.
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE hash = ?`,
		fmtTime(time.Now()), hashToken(plaintext))
	return u, true, nil
}

// SetUserState implements UserStore.
func (s *DB) SetUserState(ctx context.Context, login, state string) error {
	if state != "active" && state != "disabled" {
		return fmt.Errorf("SetUserState: invalid state %q", state)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE users SET state = ? WHERE login = ?`, state, login)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("user %q not found", login)
	}
	return nil
}

// ListUsers implements UserStore.
func (s *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, login, display_name, role, state, created_at
		FROM users ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}
