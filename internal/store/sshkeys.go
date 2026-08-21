package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrKeyHeld is returned when a fingerprint is already registered to another
// account by a key whose owner proved possession. Such a key is never taken
// over: the holder demonstrated the private half, so the second claimant is
// the one who has to be refused (SPEC §8).
var ErrKeyHeld = errors.New("store: fingerprint already registered")

// AddSSHKey implements UserStore: an unproven registration, which today only
// the teacher CLI (`anygrade user add-key`) still performs. verified_at is
// left NULL, so the key authenticates but is displayed as unproven and can
// lose the fingerprint to somebody who proves possession of it.
func (s *DB) AddSSHKey(ctx context.Context, userID int64, fingerprint, publicKey string) (SSHKey, error) {
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO ssh_keys (user_id, fingerprint, public_key, created_at)
		VALUES (?, ?, ?, ?)
		RETURNING id, user_id, fingerprint, public_key, created_at, verified_at`,
		userID, fingerprint, publicKey, fmtTime(time.Now()))
	return scanSSHKey(row)
}

// AddProvenSSHKey implements UserStore: the key's owner has just signed a
// server challenge with the private half (SPEC §8).
//
// The contested-fingerprint rule lives here because it needs one transaction:
//
//   - free fingerprint: registered, verified_at stamped;
//   - already the caller's own: verified_at stamped, so re-proving a legacy
//     key upgrades it in place instead of failing on the UNIQUE constraint;
//   - held by another account with a *proven* key: ErrKeyHeld, exactly as
//     before - a proof cannot beat a proof;
//   - held by another account with an *unproven* key: taken over, and the
//     losing account is returned so the caller can audit it. This is the one
//     deliberate change of who wins. Every unproven row is either a legacy
//     registration or a teacher-added one, and the only party who can produce
//     a signature is the party holding the private key; leaving the squat in
//     place would keep the victim locked out until a teacher noticed.
func (s *DB) AddProvenSSHKey(ctx context.Context, userID int64, fingerprint, publicKey string) (SSHKey, *User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SSHKey{}, nil, err
	}
	defer tx.Rollback()

	var (
		holderID   int64
		verifiedAt *string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT user_id, verified_at FROM ssh_keys WHERE fingerprint = ?`, fingerprint).
		Scan(&holderID, &verifiedAt)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return SSHKey{}, nil, err
	case err == nil && holderID == userID:
		// Own key: stamp the proof and hand back the row as it now stands.
		row := tx.QueryRowContext(ctx, `
			UPDATE ssh_keys SET verified_at = ? WHERE fingerprint = ?
			RETURNING id, user_id, fingerprint, public_key, created_at, verified_at`,
			fmtTime(time.Now()), fingerprint)
		k, kerr := scanSSHKey(row)
		if kerr != nil {
			return SSHKey{}, nil, kerr
		}
		return k, nil, tx.Commit()
	case err == nil && verifiedAt != nil:
		return SSHKey{}, nil, ErrKeyHeld
	}

	var displaced *User
	if err == nil {
		// Unproven holder: read them before the row goes, so the caller can name
		// both accounts in the audit log.
		u, uerr := scanUser(tx.QueryRowContext(ctx, `
			SELECT id, login, display_name, role, state, created_at
			FROM users WHERE id = ?`, holderID))
		if uerr != nil {
			return SSHKey{}, nil, uerr
		}
		displaced = &u
		if _, derr := tx.ExecContext(ctx,
			`DELETE FROM ssh_keys WHERE fingerprint = ?`, fingerprint); derr != nil {
			return SSHKey{}, nil, derr
		}
	}

	now := fmtTime(time.Now())
	row := tx.QueryRowContext(ctx, `
		INSERT INTO ssh_keys (user_id, fingerprint, public_key, created_at, verified_at)
		VALUES (?, ?, ?, ?, ?)
		RETURNING id, user_id, fingerprint, public_key, created_at, verified_at`,
		userID, fingerprint, publicKey, now, now)
	k, err := scanSSHKey(row)
	if err != nil {
		return SSHKey{}, nil, err
	}
	return k, displaced, tx.Commit()
}

// ListSSHKeys implements UserStore.
func (s *DB) ListSSHKeys(ctx context.Context, userID int64) ([]SSHKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, fingerprint, public_key, created_at, verified_at
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

// KeyHolder implements UserStore: the owner of a fingerprint whatever their
// state. Authentication must keep using UserByFingerprint - a disabled account
// stays disabled - but the squatting report has to name a holder even when the
// squatter has been deactivated, which is precisely the case a teacher needs to
// see: the victim is locked out of their own key and nothing says why.
func (s *DB) KeyHolder(ctx context.Context, fingerprint string) (User, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT users.id, users.login, users.display_name, users.role, users.state, users.created_at
		FROM ssh_keys JOIN users ON users.id = ssh_keys.user_id
		WHERE ssh_keys.fingerprint = ?`,
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
	var (
		k          SSHKey
		createdAt  string
		verifiedAt *string
	)
	if err := row.Scan(&k.ID, &k.UserID, &k.Fingerprint, &k.PublicKey, &createdAt, &verifiedAt); err != nil {
		return SSHKey{}, err
	}
	var err error
	if k.CreatedAt, err = parseTime(createdAt); err != nil {
		return SSHKey{}, err
	}
	if k.VerifiedAt, err = parseTimePtr(verifiedAt); err != nil {
		return SSHKey{}, err
	}
	return k, nil
}
