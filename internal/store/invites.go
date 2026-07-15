package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CreateInvite implements InviteStore.
func (s *DB) CreateInvite(ctx context.Context, userID int64, tokenPlaintext string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO invites (token_hash, user_id, expires_at)
		VALUES (?, ?, ?)`,
		hashToken(tokenPlaintext), userID, fmtTime(expiresAt))
	return err
}

// VerifyInvite implements InviteStore: ok=false for unknown, used, or
// expired invites.
func (s *DB) VerifyInvite(ctx context.Context, tokenPlaintext string) (Invite, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, expires_at, used_at
		FROM invites WHERE token_hash = ?`,
		hashToken(tokenPlaintext))
	inv, err := scanInvite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, false, nil
	}
	if err != nil {
		return Invite{}, false, err
	}
	if inv.UsedAt != nil || !inv.ExpiresAt.After(time.Now()) {
		return Invite{}, false, nil
	}
	return inv, true, nil
}

// MarkInviteUsed implements InviteStore.
func (s *DB) MarkInviteUsed(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE invites SET used_at = ? WHERE id = ?`, fmtTime(now), id)
	return err
}

func scanInvite(row scanner) (Invite, error) {
	var (
		inv       Invite
		expiresAt string
		usedAt    *string
	)
	if err := row.Scan(&inv.ID, &inv.UserID, &expiresAt, &usedAt); err != nil {
		return Invite{}, err
	}
	var err error
	if inv.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return Invite{}, err
	}
	if inv.UsedAt, err = parseTimePtr(usedAt); err != nil {
		return Invite{}, err
	}
	return inv, nil
}
