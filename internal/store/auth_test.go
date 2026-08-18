package store

import (
	"strings"
	"testing"
)

// TestTokenLifecycle: issue, verify, reissue invalidates the old token.
func TestTokenLifecycle(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	plaintext, err := db.IssueToken(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plaintext, "ag_") || len(plaintext) <= 20 {
		t.Fatalf("unexpected token shape: %q", plaintext)
	}

	got, ok, err := db.VerifyToken(t.Context(), plaintext)
	if err != nil || !ok || got.Login != u.Login {
		t.Fatalf("verify: got=%+v ok=%v err=%v", got, ok, err)
	}

	if _, ok, err := db.VerifyToken(t.Context(), "ag_wrong"); err != nil || ok {
		t.Fatalf("verify unknown token: ok=%v err=%v", ok, err)
	}

	reissued, err := db.IssueToken(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.VerifyToken(t.Context(), plaintext); ok {
		t.Fatal("old token must be invalid after reissue")
	}
	if _, ok, err := db.VerifyToken(t.Context(), reissued); err != nil || !ok {
		t.Fatalf("new token: ok=%v err=%v", ok, err)
	}
}

// TestIssueTokenKeepsOneRow: an account can never hold two valid tokens. The
// rotations come from three places (student regenerate, teacher reset, invite
// activation) and from two processes, so the schema has to carry the
// invariant, not the ordering of two statements.
func TestIssueTokenKeepsOneRow(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	for range 3 {
		if _, err := db.IssueToken(t.Context(), u.ID); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM tokens WHERE user_id = ?`, u.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d token rows after three rotations, want 1", n)
	}
	// The row two interleaved rotations used to leave behind is now refused.
	if _, err := db.db.ExecContext(t.Context(),
		`INSERT INTO tokens (user_id, hash, created_at)
		 VALUES (?, 'second', '2026-01-01T00:00:00.000000000Z')`, u.ID); err == nil {
		t.Fatal("a second token row for one user must be rejected")
	}
}

// TestVerifyTokenDisabledUser: a disabled user's token no longer verifies.
func TestVerifyTokenDisabledUser(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	plaintext, err := db.IssueToken(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserState(t.Context(), u.Login, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.VerifyToken(t.Context(), plaintext); err != nil || ok {
		t.Fatalf("disabled user token: ok=%v err=%v", ok, err)
	}
}

// TestSetUserStateUnknown: unknown login and invalid state both error.
func TestSetUserStateUnknown(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	if err := db.SetUserState(t.Context(), "ghost", "disabled"); err == nil {
		t.Fatal("want error for unknown login")
	}
	if err := db.SetUserState(t.Context(), u.Login, "bogus"); err == nil {
		t.Fatal("want error for invalid state")
	}
}

// TestSSHKeys: add, list, and resolve by fingerprint.
func TestSSHKeys(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	if _, err := db.AddSSHKey(t.Context(), u.ID, "SHA256:aaa", "ssh-ed25519 AAA a"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddSSHKey(t.Context(), u.ID, "SHA256:bbb", "ssh-ed25519 BBB b"); err != nil {
		t.Fatal(err)
	}

	keys, err := db.ListSSHKeys(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].Fingerprint != "SHA256:aaa" || keys[1].Fingerprint != "SHA256:bbb" {
		t.Fatalf("keys: %+v", keys)
	}

	got, ok, err := db.UserByFingerprint(t.Context(), "SHA256:aaa")
	if err != nil || !ok || got.Login != u.Login {
		t.Fatalf("UserByFingerprint: got=%+v ok=%v err=%v", got, ok, err)
	}

	if _, ok, err := db.UserByFingerprint(t.Context(), "SHA256:unknown"); err != nil || ok {
		t.Fatalf("unknown fingerprint: ok=%v err=%v", ok, err)
	}

	if err := db.SetUserState(t.Context(), u.Login, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.UserByFingerprint(t.Context(), "SHA256:aaa"); err != nil || ok {
		t.Fatalf("disabled user fingerprint: ok=%v err=%v", ok, err)
	}

	if _, err := db.AddSSHKey(t.Context(), u.ID, "SHA256:aaa", "ssh-ed25519 AAA dup"); err == nil {
		t.Fatal("want error for duplicate fingerprint")
	}
}

// TestListUsers: results are ordered by login.
func TestListUsers(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.CreateUser(t.Context(), "b-user", "B User", "student"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(t.Context(), "a-user", "A User", "student"); err != nil {
		t.Fatal(err)
	}

	users, err := db.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Login != "a-user" || users[1].Login != "b-user" {
		t.Fatalf("users: %+v", users)
	}
}
