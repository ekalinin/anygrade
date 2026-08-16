package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// unsortable returns the pair that breaks lexicographic ordering under
// time.RFC3339Nano: the earlier value's text is a prefix of the later one's,
// and the 'Z' that follows it sorts after any digit.
//
//	earlier 2026-08-15T10:00:00.387Z
//	later   2026-08-15T10:00:00.387026Z
func unsortable() (earlier, later time.Time) {
	base := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	return base.Add(387 * time.Millisecond), base.Add(387026 * time.Microsecond)
}

func TestFmtTimeIsFixedWidth(t *testing.T) {
	earlier, later := unsortable()
	for _, tc := range []struct {
		name string
		in   time.Time
		want string
	}{
		{"whole second", earlier.Truncate(time.Second), "2026-08-15T10:00:00.000000000Z"},
		{"milliseconds", earlier, "2026-08-15T10:00:00.387000000Z"},
		{"microseconds", later, "2026-08-15T10:00:00.387026000Z"},
		{"nanoseconds", later.Add(7), "2026-08-15T10:00:00.387026007Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fmtTime(tc.in)
			if got != tc.want {
				t.Fatalf("fmtTime = %q, want %q", got, tc.want)
			}
			back, err := parseTime(got)
			if err != nil {
				t.Fatalf("parseTime(%q): %v", got, err)
			}
			if !back.Equal(tc.in) {
				t.Fatalf("round trip = %v, want %v", back, tc.in)
			}
		})
	}
}

// TestSubmissionsOrderByReceivedAt covers the three queries the ordering bug
// reached: the execution order, the history that feeds the cooldown anchor,
// and the "latest submission" lookup.
func TestSubmissionsOrderByReceivedAt(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	earlier, later := unsortable()

	// Inserted later-first, so a query that keeps insertion order fails too.
	second := enqueueAt(t, db, u.ID, "t1", "sha-later", later)
	first := enqueueAt(t, db, u.ID, "t1", "sha-earlier", earlier)

	subs, err := db.ListByUserTask(t.Context(), u.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 || subs[0].ID != first.ID || subs[1].ID != second.ID {
		t.Fatalf("ListByUserTask order = %v, want [%d %d]", ids(subs), first.ID, second.ID)
	}

	last, ok, err := db.LastByUserTask(t.Context(), u.ID, "t1")
	if err != nil || !ok {
		t.Fatalf("LastByUserTask: ok=%v err=%v", ok, err)
	}
	if last.ID != second.ID {
		t.Fatalf("LastByUserTask = %d, want %d", last.ID, second.ID)
	}

	// Both rows are queued and nothing of the pair is running yet, so ClaimNext
	// picks purely by received_at - this is the execution order of SPEC §13.
	claimed, ok, err := db.ClaimNext(t.Context(), time.Now())
	if err != nil || !ok {
		t.Fatalf("ClaimNext: ok=%v err=%v", ok, err)
	}
	if claimed.ID != first.ID {
		t.Fatalf("ClaimNext = %d, want %d (the earlier of the two)", claimed.ID, first.ID)
	}
}

// TestMigrateSortableTimestamps drives migration 0004 over rows in the old
// RFC3339Nano shapes: no fractional part, and a fraction shorter than nine
// digits.
func TestMigrateSortableTimestamps(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	u := testUser(t, db)
	earlier, later := unsortable()
	first := enqueueAt(t, db, u.ID, "t1", "sha-earlier", earlier)
	second := enqueueAt(t, db, u.ID, "t1", "sha-later", later)

	// Rewind to the pre-0004 world: legacy text and the matching user_version.
	legacy := map[int64]string{
		first.ID:  earlier.UTC().Format(time.RFC3339Nano),
		second.ID: later.UTC().Format(time.RFC3339Nano),
	}
	for id, v := range legacy {
		exec(t, db.db, `UPDATE submissions SET received_at = ? WHERE id = ?`, v, id)
	}
	// One value with no fractional part at all, on a column that also nulls.
	exec(t, db.db, `UPDATE submissions SET started_at = ? WHERE id = ?`,
		earlier.Truncate(time.Second).Format(time.RFC3339Nano), first.ID)
	exec(t, db.db, `PRAGMA user_version = 3`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	var got, startedAt string
	if err := db.db.QueryRowContext(t.Context(),
		`SELECT received_at, COALESCE(started_at, '') FROM submissions WHERE id = ?`,
		first.ID).Scan(&got, &startedAt); err != nil {
		t.Fatal(err)
	}
	if want := "2026-08-15T10:00:00.387000000Z"; got != want {
		t.Fatalf("migrated received_at = %q, want %q", got, want)
	}
	if want := "2026-08-15T10:00:00.000000000Z"; startedAt != want {
		t.Fatalf("migrated started_at = %q, want %q", startedAt, want)
	}

	subs, err := db.ListByUserTask(t.Context(), u.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 || subs[0].ID != first.ID {
		t.Fatalf("order after migration = %v, want [%d %d]", ids(subs), first.ID, second.ID)
	}

	// The submission whose retry_at stayed NULL must stay NULL.
	var nulls int
	if err := db.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM submissions WHERE retry_at IS NULL`).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 2 {
		t.Fatalf("retry_at NULLs = %d, want 2", nulls)
	}
}

func enqueueAt(t *testing.T, db *DB, userID int64, task, sha string, at time.Time) Submission {
	t.Helper()
	sub, err := db.Enqueue(t.Context(), NewSubmission{
		UserID:     userID,
		TaskID:     task,
		CommitSHA:  sha,
		ReceivedAt: at,
		Counts:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func ids(subs []Submission) []int64 {
	out := make([]int64, len(subs))
	for i, s := range subs {
		out[i] = s.ID
	}
	return out
}
