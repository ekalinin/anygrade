package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestRoleRights is the role table itself, asserted once so the handlers can
// ask the question instead of comparing strings.
func TestRoleRights(t *testing.T) {
	for _, c := range []struct {
		role               string
		review, administer bool
	}{
		{RoleStudent, false, false},
		{RoleTA, true, false},
		{RoleTeacher, true, true},
	} {
		u := User{Role: c.role}
		if got := u.CanReview(); got != c.review {
			t.Errorf("%s: CanReview() = %v, want %v", c.role, got, c.review)
		}
		if got := u.CanAdminister(); got != c.administer {
			t.Errorf("%s: CanAdminister() = %v, want %v", c.role, got, c.administer)
		}
	}
	if ValidRole("assistant") || !ValidRole(RoleTA) {
		t.Error("ValidRole accepts exactly the three schema roles")
	}
}

// TestTARoleIsStorable: the CHECK constraint was widened by rebuilding the
// table, so the first thing to prove is that the new value goes in and comes
// back out.
func TestTARoleIsStorable(t *testing.T) {
	db := openTestDB(t)
	u, err := db.CreateUser(t.Context(), "ta1", "Assistant", RoleTA)
	if err != nil {
		t.Fatalf("create a TA: %v", err)
	}
	got, err := db.GetUserByLogin(t.Context(), "ta1")
	if err != nil || got.Role != RoleTA {
		t.Fatalf("GetUserByLogin = %+v (err %v), want role %q", got, err, RoleTA)
	}
	if !got.CanReview() || got.CanAdminister() {
		t.Errorf("stored TA has the wrong rights: %+v", got)
	}
	if _, err := db.CreateUser(t.Context(), "nope", "", "assistant"); err == nil {
		t.Errorf("the CHECK constraint accepted an unknown role (user %d)", u.ID)
	}
}

// TestMigrateWidensRoleCheckWithoutLosingAccounts: 0010 rebuilds users, which
// is the one migration that could take the rest of the database with it - the
// tables referencing users(id) cascade on delete, and DROP TABLE fires those
// actions when foreign keys are on. Every child row must still be there, and
// enforcement must be back on afterwards.
func TestMigrateWidensRoleCheckWithoutLosingAccounts(t *testing.T) {
	dir := t.TempDir()
	seedPre0010DB(t, dir)

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	u, err := db.GetUserByLogin(t.Context(), "bob")
	if err != nil || u.Role != RoleStudent {
		t.Fatalf("GetUserByLogin = %+v (err %v), want the pre-existing student", u, err)
	}
	for _, c := range []struct {
		table string
		want  int
	}{
		{"tokens", 1}, {"ssh_keys", 1}, {"invites", 1}, {"submissions", 1}, {"events", 1},
	} {
		var n int
		if err := db.db.QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM "+c.table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != c.want {
			t.Errorf("%s: %d rows after the rebuild, want %d", c.table, n, c.want)
		}
	}

	// The references still point at the rebuilt table, and the connection the
	// store hands out enforces them again.
	rows, err := db.db.QueryContext(t.Context(), "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("foreign_key_check reports a dangling reference after the rebuild")
	}
	var on int
	if err := db.db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&on); err != nil {
		t.Fatal(err)
	}
	if on != 1 {
		t.Error("foreign keys stayed off after the migration ledger")
	}

	// And the point of the rebuild: the new role is accepted now.
	if _, err := db.CreateUser(t.Context(), "ta1", "Assistant", RoleTA); err != nil {
		t.Errorf("create a TA after the upgrade: %v", err)
	}
}

// TestLogRecordsActorRole: the audit row carries the role its actor held when
// they acted - a promotion afterwards must not rewrite it - while an event
// written before the column existed stays unknown rather than being guessed at.
func TestLogRecordsActorRole(t *testing.T) {
	db := openTestDB(t)
	ta, err := db.CreateUser(t.Context(), "ta1", "Assistant", RoleTA)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Log(t.Context(), Event{
		ActorID: &ta.ID, Kind: "recheck", Target: "bob/t1", Detail: "staff recheck #1",
	}); err != nil {
		t.Fatal(err)
	}
	// A system event has no actor and therefore no role.
	if err := db.Log(t.Context(), Event{Kind: "user.register", Target: "bob"}); err != nil {
		t.Fatal(err)
	}

	events, err := db.ListEvents(t.Context(), "", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("%d events, want 2", len(events))
	}
	if events[0].ActorRole != "" || events[0].ActorLogin != "" {
		t.Errorf("system event = %+v, want no actor and no role", events[0])
	}
	if events[1].ActorRole != RoleTA {
		t.Errorf("actor role = %q, want %q", events[1].ActorRole, RoleTA)
	}

	// The promotion does not reach back: the row still says what the action was
	// taken as, which is the whole reason the role is stored rather than joined.
	if _, err := db.db.ExecContext(t.Context(),
		`UPDATE users SET role = ? WHERE id = ?`, RoleTeacher, ta.ID); err != nil {
		t.Fatal(err)
	}
	byTarget, err := db.ListEventsByTarget(t.Context(), "bob", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range byTarget {
		if e.Kind == "recheck" && e.ActorRole != RoleTA {
			t.Errorf("after the promotion the old row reads %q, want %q", e.ActorRole, RoleTA)
		}
	}
}

// seedPre0010DB writes a database as the version before 0010 left it: one
// account with a row in every table that references it, plus an audit event
// from the days when the log had no role column.
func seedPre0010DB(t *testing.T, dir string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "anygrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	for _, name := range migrationsBefore(t, 10) {
		content, cerr := migrationsFS.ReadFile("migrations/" + name)
		if cerr != nil {
			t.Fatal(cerr)
		}
		exec(t, raw, string(content))
	}
	exec(t, raw, `PRAGMA user_version = 9`)
	const at = "2026-01-01T00:00:00.000000000Z"
	exec(t, raw, `INSERT INTO users (id, login, display_name, role, created_at)
		VALUES (1, 'bob', 'Bob', 'student', ?)`, at)
	exec(t, raw, `INSERT INTO tokens (id, user_id, hash, created_at) VALUES (1, 1, 'h', ?)`, at)
	exec(t, raw, `INSERT INTO ssh_keys (id, user_id, fingerprint, public_key, created_at)
		VALUES (1, 1, 'SHA256:legacy', 'ssh-ed25519 LEGACY b', ?)`, at)
	exec(t, raw, `INSERT INTO invites (id, token_hash, user_id, expires_at)
		VALUES (1, 'inv', 1, ?)`, time.Now().Add(time.Hour).Format(timeLayout))
	exec(t, raw, `INSERT INTO submissions (id, user_id, task_id, commit_sha, received_at, status)
		VALUES (1, 1, 't1', 'deadbeef', ?, 'done')`, at)
	exec(t, raw, `INSERT INTO events (id, actor_id, kind, target, detail, created_at)
		VALUES (1, 1, 'token.reset', 'bob', 'by teacher', ?)`, at)
}

// TestLegacyAuditRowsHaveNoRole: rows the log already holds keep an empty role
// after the upgrade. Backfilling them from users would claim a role for
// actions taken before roles were recorded, and students write audited events
// too, so "teacher" would not even be a good guess.
func TestLegacyAuditRowsHaveNoRole(t *testing.T) {
	dir := t.TempDir()
	seedPre0010DB(t, dir)

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	events, err := db.ListEvents(t.Context(), "", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("%d events, want the one seeded row", len(events))
	}
	if events[0].ActorRole != "" {
		t.Errorf("legacy event = %+v, want an empty (unknown) role", events[0])
	}
}
