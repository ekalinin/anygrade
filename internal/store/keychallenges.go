package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CreateKeyChallenge implements KeyChallengeStore.
//
// One upsert against UNIQUE(user_id), so a student who starts the flow twice
// has exactly one live nonce: the second issue invalidates the first rather
// than leaving both usable. Only the hash is stored (SPEC §14).
func (s *DB) CreateKeyChallenge(ctx context.Context, userID int64, noncePlaintext, fingerprint, publicKey string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ssh_key_challenges (user_id, nonce_hash, fingerprint, public_key, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
		  nonce_hash = excluded.nonce_hash, fingerprint = excluded.fingerprint,
		  public_key = excluded.public_key, created_at = excluded.created_at,
		  expires_at = excluded.expires_at`,
		userID, hashToken(noncePlaintext), fingerprint, publicKey,
		fmtTime(time.Now()), fmtTime(expiresAt))
	return err
}

// LookupKeyChallenge implements KeyChallengeStore: ok=false for unknown,
// already consumed, or expired nonces. It only reads, so a mistyped signature
// leaves the challenge usable - the same reason VerifyInvite is separate from
// ConsumeInvite.
func (s *DB) LookupKeyChallenge(ctx context.Context, noncePlaintext string, now time.Time) (KeyChallenge, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT user_id, fingerprint, public_key, expires_at
		FROM ssh_key_challenges WHERE nonce_hash = ?`,
		hashToken(noncePlaintext))
	var (
		c         KeyChallenge
		expiresAt string
	)
	if err := row.Scan(&c.UserID, &c.Fingerprint, &c.PublicKey, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return KeyChallenge{}, false, nil
		}
		return KeyChallenge{}, false, err
	}
	var err error
	if c.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return KeyChallenge{}, false, err
	}
	if !c.ExpiresAt.After(now) {
		return KeyChallenge{}, false, nil
	}
	return c, true, nil
}

// ConsumeKeyChallenge implements KeyChallengeStore. LookupKeyChallenge alone
// only proves the nonce was live at the time it was read, so the delete is
// what makes the proof single-use under a race: exactly one caller removes the
// row and gets ok=true. Callers must consume before registering the key.
//
// The delete is also how the table stays small - an expired row is refused by
// LookupKeyChallenge and overwritten by the account's next challenge, so
// nothing has to sweep it.
func (s *DB) ConsumeKeyChallenge(ctx context.Context, noncePlaintext string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ssh_key_challenges WHERE nonce_hash = ?`, hashToken(noncePlaintext))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
