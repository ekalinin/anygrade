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

	for _, name := range legacyMigrations(t) {
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

// legacyMigrations lists every migration before 0006, in order.
func legacyMigrations(t *testing.T) []string {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if migrationNumber(t, e.Name()) < 6 {
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
