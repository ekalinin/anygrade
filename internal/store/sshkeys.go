package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AddSSHKey implements UserStore.
func (s *DB) AddSSHKey(ctx context.Context, userID int64, fingerprint, publicKey string) (SSHKey, error) {
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO ssh_keys (user_id, fingerprint, public_key, created_at)
		VALUES (?, ?, ?, ?)
		RETURNING id, user_id, fingerprint, public_key, created_at`,
		userID, fingerprint, publicKey, fmtTime(time.Now()))
	return scanSSHKey(row)
}

// ListSSHKeys implements UserStore.
func (s *DB) ListSSHKeys(ctx context.Context, userID int64) ([]SSHKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, fingerprint, public_key, created_at
		FROM ssh_keys WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []SSHKey
	for rows.Next() {
		k, err := scanSSHKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// UserByFingerprint implements UserStore.
func (s *DB) UserByFingerprint(ctx context.Context, fingerprint string) (User, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT users.id, users.login, users.display_name, users.role, users.state, users.created_at
		FROM ssh_keys JOIN users ON users.id = ssh_keys.user_id
		WHERE ssh_keys.fingerprint = ? AND users.state = 'active'`,
		fingerprint)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

// DeleteSSHKey implements UserStore; scoping to userID prevents cross-user
// deletes from a forged form.
//
// The fingerprint is part of the predicate, not a courtesy check: id is a bare
// INTEGER PRIMARY KEY, so SQLite hands a freed rowid to the next insert. A
// delete that only matched (id, user_id) could therefore land on a key added
// after the form was rendered. Matching the fingerprint the caller saw makes
// the delete idempotent under that reuse - ok=false, nothing removed.
func (s *DB) DeleteSSHKey(ctx context.Context, userID, keyID int64, fingerprint string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ssh_keys WHERE id = ? AND user_id = ? AND fingerprint = ?`,
		keyID, userID, fingerprint)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func scanSSHKey(row scanner) (SSHKey, error) {
	var k SSHKey
	var createdAt string
	if err := row.Scan(&k.ID, &k.UserID, &k.Fingerprint, &k.PublicKey, &createdAt); err != nil {
		return SSHKey{}, err
	}
	var err error
	if k.CreatedAt, err = parseTime(createdAt); err != nil {
		return SSHKey{}, err
	}
	return k, nil
}
