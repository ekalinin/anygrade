package store

import (
	"testing"
	"time"
)

func TestPushLog(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	base := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)

	record := func(old, new string, at time.Time) Push {
		t.Helper()
		p, err := db.RecordPush(t.Context(), NewPush{UserID: u.ID, Ref: "refs/heads/main",
			OldSHA: old, NewSHA: new, ReceivedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	first := record("sha0", "sha1", base)
	second := record("sha1", "sha2", base.Add(time.Second))
	if first.ProcessedAt != nil || !first.ReceivedAt.Equal(base) {
		t.Fatalf("recorded push = %+v", first)
	}

	pending, err := db.PendingPushes(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != first.ID || pending[1].ID != second.ID {
		t.Fatalf("pending = %+v, want %d then %d", pending, first.ID, second.ID)
	}
	if pending[0].OldSHA != "sha0" || pending[0].NewSHA != "sha1" || pending[0].Ref != "refs/heads/main" {
		t.Errorf("push boundaries lost: %+v", pending[0])
	}

	if err := db.MarkPushProcessed(t.Context(), first.ID, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	pending, err = db.PendingPushes(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != second.ID {
		t.Fatalf("pending after marking = %+v, want only %d", pending, second.ID)
	}

	// A force-push cycle repeats the pair of ends; the rows stay distinct.
	back := record("sha2", "sha1", base.Add(2*time.Second))
	if back.ID == first.ID {
		t.Fatal("a repeated old/new pair must still be its own push")
	}

	// The log is per student.
	other, err := db.CreateUser(t.Context(), "student2", "Student Two", "student")
	if err != nil {
		t.Fatal(err)
	}
	if pending, err = db.PendingPushes(t.Context(), other.ID); err != nil || len(pending) != 0 {
		t.Fatalf("other student's pending = %+v, err=%v", pending, err)
	}
}
