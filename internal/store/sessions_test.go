package store

import (
	"testing"
	"time"
)

// TestSessionLifecycle: create, look up, and delete a session.
func TestSessionLifecycle(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	plaintext, err := db.IssueToken(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}

	id, err := db.CreateSession(t.Context(), u.ID, plaintext, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || len(id) < 40 {
		t.Fatalf("unexpected session id shape: %q", id)
	}

	got, ok, err := db.LookupSession(t.Context(), id)
	if err != nil || !ok || got.Login != u.Login {
		t.Fatalf("lookup: got=%+v ok=%v err=%v", got, ok, err)
	}

	if _, ok, err := db.LookupSession(t.Context(), "nope"); err != nil || ok {
		t.Fatalf("lookup unknown session: ok=%v err=%v", ok, err)
	}

	if err := db.DeleteSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.LookupSession(t.Context(), id); err != nil || ok {
		t.Fatalf("lookup after delete: ok=%v err=%v", ok, err)
	}

	if err := db.DeleteSession(t.Context(), "nope"); err != nil {
		t.Fatalf("delete unknown session: %v", err)
	}
}

// TestSessionExpiry: an already-expired session is never looked up.
func TestSessionExpiry(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	plaintext, err := db.IssueToken(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}

	id, err := db.CreateSession(t.Context(), u.ID, plaintext, -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.LookupSession(t.Context(), id); err != nil || ok {
		t.Fatalf("expired session: ok=%v err=%v", ok, err)
	}
}

// TestSessionRevokedByTokenReset: reissuing the token revokes sessions
// created from the old one.
func TestSessionRevokedByTokenReset(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	plaintext, err := db.IssueToken(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateSession(t.Context(), u.ID, plaintext, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.IssueToken(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := db.LookupSession(t.Context(), id); err != nil || ok {
		t.Fatalf("session must be revoked after token reset: ok=%v err=%v", ok, err)
	}
}

// TestSessionDisabledUser: a disabled user's session no longer resolves.
func TestSessionDisabledUser(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	plaintext, err := db.IssueToken(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateSession(t.Context(), u.ID, plaintext, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.SetUserState(t.Context(), u.Login, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.LookupSession(t.Context(), id); err != nil || ok {
		t.Fatalf("disabled user session: ok=%v err=%v", ok, err)
	}
}

// TestListByUser: results are ordered by task then time, scoped to one user.
func TestListByUser(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	other, err := db.CreateUser(t.Context(), "student2", "Student Two", "student")
	if err != nil {
		t.Fatal(err)
	}

	enqueueN(t, db, u.ID, "b", 1)
	enqueueN(t, db, u.ID, "a", 1)
	enqueueN(t, db, other.ID, "a", 1)

	subs, err := db.ListByUser(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 || subs[0].TaskID != "a" || subs[1].TaskID != "b" {
		t.Fatalf("subs: %+v", subs)
	}
	for _, s := range subs {
		if s.UserID != u.ID {
			t.Errorf("other user's submission leaked: %+v", s)
		}
	}
}
