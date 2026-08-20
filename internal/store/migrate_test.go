package store

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMigrationsLedgerAtHead: a fresh database ends at the highest migration
// number, and reopening it applies nothing. That one-way ledger is what lets
// migrations be written as single steps (0006 renames a column and drops an
// index, neither of which survives a replay), and this is the guard against a
// file that only works the first time it runs.
func TestMigrationsLedgerAtHead(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var got int
	if err := db.db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := headVersion(t); got != want {
		t.Fatalf("user_version = %d, want %d", got, want)
	}
}

// TestMigrateSessionsAndTokens upgrades a database left by the previous
// version: the shape 0006 has to repair (two tokens for one account, what two
// interleaved rotations used to leave behind) and the one it has to drop
// (session ids stored as the cookie itself).
func TestMigrateSessionsAndTokens(t *testing.T) {
	dir := t.TempDir()
	seedLegacyDB(t, dir)

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var hash string
	if err := db.db.QueryRowContext(t.Context(),
		`SELECT hash FROM tokens WHERE user_id = 1`).Scan(&hash); err != nil {
		t.Fatalf("token after upgrade: %v", err)
	}
	if hash != "newer" {
		t.Errorf("surviving token = %q, want the newest row", hash)
	}
	var sessions int
	if err := db.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Errorf("%d sessions survived, want 0: a plaintext id cannot be hashed backwards", sessions)
	}
	// The account keeps working: a new token and a new session on top of the
	// upgraded schema.
	plaintext, err := db.IssueToken(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateSession(t.Context(), 1, plaintext, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.LookupSession(t.Context(), id); err != nil || !ok {
		t.Fatalf("session after upgrade: ok=%v err=%v", ok, err)
	}
}

// TestMigrateGrandfathersSSHKeys: keys registered before proof of possession
// existed keep authenticating across the upgrade. Invalidating them would lock
// a running course out of SSH over a hole that is denial of service only, and
// already detected and audited (SPEC §8); they are marked unproven instead.
func TestMigrateGrandfathersSSHKeys(t *testing.T) {
	dir := t.TempDir()
	seedPre0007DB(t, dir)

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	keys, err := db.ListSSHKeys(t.Context(), 1)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListSSHKeys = %v (err %v), want the pre-existing key", keys, err)
	}
	if keys[0].VerifiedAt != nil {
		t.Errorf("verified_at = %v, want nil: nobody ever proved this key", keys[0].VerifiedAt)
	}
	got, ok, err := db.UserByFingerprint(t.Context(), "SHA256:legacy")
	if err != nil || !ok || got.Login != "bob" {
		t.Fatalf("the legacy key no longer authenticates: %+v ok=%v err=%v", got, ok, err)
	}
	// And the owner can prove it afterwards, upgrading the row in place.
	if _, displaced, perr := db.AddProvenSSHKey(t.Context(), 1, "SHA256:legacy", "ssh-ed25519 LEGACY b"); perr != nil || displaced != nil {
		t.Fatalf("proving a legacy key: displaced=%+v err=%v", displaced, perr)
	}
}

// seedPre0007DB writes a database as the version before 0007 left it, with one
// SSH key registered under the old first-come-first-served rule.
func seedPre0007DB(t *testing.T, dir string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "anygrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	for _, name := range migrationsBefore(t, 7) {
		content, cerr := migrationsFS.ReadFile("migrations/" + name)
		if cerr != nil {
			t.Fatal(cerr)
		}
		exec(t, raw, string(content))
	}
	exec(t, raw, `PRAGMA user_version = 6`)
	exec(t, raw, `INSERT INTO users (id, login, display_name, role, created_at)
		VALUES (1, 'bob', 'Bob', 'student', '2026-01-01T00:00:00.000000000Z')`)
	exec(t, raw, `INSERT INTO ssh_keys (id, user_id, fingerprint, public_key, created_at)
		VALUES (1, 1, 'SHA256:legacy', 'ssh-ed25519 LEGACY b', '2026-01-01T00:00:00.000000000Z')`)
}

// seedLegacyDB writes a database as the version before 0006 left it: every
// earlier migration applied, the matching user_version, and rows only that
// schema could hold.
func seedLegacyDB(t *testing.T, dir string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "anygrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	for _, name := range migrationsBefore(t, 6) {
		content, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		exec(t, raw, string(content))
	}
	exec(t, raw, `PRAGMA user_version = 5`)
	exec(t, raw, `INSERT INTO users (id, login, display_name, role, created_at)
		VALUES (1, 'bob', 'Bob', 'student', '2026-01-01T00:00:00.000000000Z')`)
	// Two live tokens for one account: DELETE + INSERT as separate statements.
	exec(t, raw, `INSERT INTO tokens (id, user_id, hash, created_at) VALUES
		(1, 1, 'older', '2026-01-01T00:00:00.000000000Z'),
		(2, 1, 'newer', '2026-01-02T00:00:00.000000000Z')`)
	exec(t, raw, `INSERT INTO sessions (id, user_id, token_hash, created_at, expires_at)
		VALUES ('the-cookie-itself', 1, 'newer',
		        '2026-01-02T00:00:00.000000000Z', '2099-01-01T00:00:00.000000000Z')`)
}

// migrationsBefore lists every migration numbered below n, in order.
func migrationsBefore(t *testing.T, n int) []string {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if migrationNumber(t, e.Name()) < n {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func headVersion(t *testing.T) int {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	head := 0
	for _, e := range entries {
		head = max(head, migrationNumber(t, e.Name()))
	}
	return head
}

func migrationNumber(t *testing.T, name string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
	if err != nil {
		t.Fatalf("migration %s: bad numeric prefix: %v", name, err)
	}
	return n
}
