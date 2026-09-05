package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// UserByOIDC implements UserStore.
//
// The `state = 'active'` filter is the same one VerifyToken, LookupSession and
// UserByFingerprint carry: a provider login is a fourth credential path, and a
// deactivated account must not get a session through it either. A disabled
// account bound to this subject is simply not found, and the caller's fallback
// - matching the login claim - refuses it again on its own state check, so
// there is no route back in.
func (s *DB) UserByOIDC(ctx context.Context, issuer, subject string) (User, bool, error) {
	if issuer == "" || subject == "" {
		return User{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, login, display_name, role, state, created_at
		FROM users WHERE oidc_iss = ? AND oidc_sub = ? AND state = 'active'`,
		issuer, subject)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

// BindOIDC implements UserStore.
//
// The conditions are the whole policy of the binding, in one statement so two
// simultaneous logins cannot both pass them:
//
//   - `oidc_sub IS NULL` means an account already bound to one subject never
//     silently accepts another. A student whose provider account really did
//     change is a case for the teacher (`anygrade user unbind-oidc`), not for
//     whoever arrives next holding a claim to that login;
//   - `state = 'active'` keeps a deactivated account from being bound and then
//     logged into;
//   - the UNIQUE index on (oidc_iss, oidc_sub) keeps one subject from reaching
//     two accounts. Hitting it is a refusal, not a failure: it means the subject
//     already belongs to somebody else.
func (s *DB) BindOIDC(ctx context.Context, userID int64, issuer, subject string) (bool, error) {
	if issuer == "" || subject == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET oidc_iss = ?, oidc_sub = ?
		WHERE id = ? AND state = 'active' AND oidc_sub IS NULL`,
		issuer, subject, userID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			return false, nil
		}
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UnbindOIDC implements UserStore.
func (s *DB) UnbindOIDC(ctx context.Context, login string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET oidc_iss = NULL, oidc_sub = NULL
		WHERE login = ? AND oidc_sub IS NOT NULL`, login)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// HasToken implements UserStore.
func (s *DB) HasToken(ctx context.Context, userID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tokens WHERE user_id = ?`, userID).Scan(&n)
	return n > 0, err
}
