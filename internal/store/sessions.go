package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

// CreateSession opens a browser session for a user whose credential was just
// verified. A non-empty tokenPlaintext binds the session to that token's hash:
// resetting the token revokes every session created from it (SPEC §8).
//
// An empty tokenPlaintext stores NULL and binds the session to no token. That
// is what an identity-provider login opens: it did not authenticate against a
// token, so a token rotation has nothing to say about it - and an account that
// was invited but never activated has no token to bind to in the first place.
// Deactivating the account still ends it, through the state filter below.
//
// The returned id is the cookie value and exists in plaintext only here: the
// row keeps its hash, so a stolen database yields no usable cookie, the same
// way it yields no usable token.
func (s *DB) CreateSession(ctx context.Context, userID int64, tokenPlaintext string, ttl time.Duration) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(buf)

	var tokenHash any
	if tokenPlaintext != "" {
		tokenHash = hashToken(tokenPlaintext)
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id_hash, user_id, token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		hashToken(id), userID, tokenHash, fmtTime(now), fmtTime(now.Add(ttl)))
	if err != nil {
		return "", err
	}
	return id, nil
}

// LookupSession resolves a session cookie to its ACTIVE user. ok=false for
// unknown/expired sessions, disabled users, and token-bound sessions whose
// token was reset (the LEFT JOIN finds no row). A session with no token
// binding - one an identity provider opened - survives a token reset by
// design; only the state filter and its own expiry end it.
func (s *DB) LookupSession(ctx context.Context, id string) (User, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.login, u.display_name, u.role, u.state, u.created_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		LEFT JOIN tokens t ON t.user_id = s.user_id AND t.hash = s.token_hash
		WHERE s.id_hash = ? AND s.expires_at > ? AND u.state = 'active'
		  AND (s.token_hash IS NULL OR t.hash IS NOT NULL)`,
		hashToken(id), fmtTime(time.Now()))
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
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, hashToken(id))
	return err
}
