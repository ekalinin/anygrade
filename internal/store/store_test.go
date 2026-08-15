package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testUser(t *testing.T, db *DB) User {
	t.Helper()
	u, err := db.CreateUser(t.Context(), "student1", "Student One", "student")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func enqueueN(t *testing.T, db *DB, userID int64, task string, n int) []Submission {
	t.Helper()
	subs := make([]Submission, n)
	for i := range n {
		var err error
		subs[i], err = db.Enqueue(t.Context(), NewSubmission{
			UserID:     userID,
			TaskID:     task,
			CommitSHA:  "sha",
			ReceivedAt: time.Now().Add(time.Duration(i) * time.Millisecond),
			Counts:     true,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return subs
}

// enqueueAcrossTasks queues one submission per task t1..tn. Rows of the same
// (user, task) are claimed one at a time, so a test that wants n rows in
// flight at once has to spread them over n tasks.
func enqueueAcrossTasks(t *testing.T, db *DB, userID int64, n int) []Submission {
	t.Helper()
	subs := make([]Submission, n)
	for i := range n {
		subs[i] = enqueueN(t, db, userID, fmt.Sprintf("t%d", i+1), 1)[0]
	}
	return subs
}

// TestConcurrentClaimUniqueness: N workers racing over M queued rows must
// produce exactly M distinct claims.
func TestConcurrentClaimUniqueness(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	const queued, workers = 5, 12
	enqueueAcrossTasks(t, db, u.ID, queued)

	var mu sync.Mutex
	claimed := map[int64]int{}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for {
				sub, ok, err := db.ClaimNext(context.Background(), time.Now())
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				claimed[sub.ID]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if len(claimed) != queued {
		t.Fatalf("claimed %d distinct rows, want %d", len(claimed), queued)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("submission %d claimed %d times", id, n)
		}
	}
}

// TestClaimSerializesOneTask: successive pushes to one task run in order, one
// at a time, while other tasks keep flowing past them (SPEC §13).
func TestClaimSerializesOneTask(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	first := enqueueN(t, db, u.ID, "t1", 1)[0]
	second := enqueueN(t, db, u.ID, "t1", 1)[0]
	other := enqueueN(t, db, u.ID, "t2", 1)[0]

	claimed, ok, err := db.ClaimNext(t.Context(), time.Now())
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if claimed.ID != first.ID {
		t.Fatalf("claimed #%d first, want the oldest #%d", claimed.ID, first.ID)
	}

	// t1 is busy, so the next claim skips its queued row and takes t2.
	claimed, ok, err = db.ClaimNext(t.Context(), time.Now())
	if err != nil || !ok {
		t.Fatalf("second claim: ok=%v err=%v", ok, err)
	}
	if claimed.ID != other.ID {
		t.Fatalf("claimed #%d, want #%d: a busy task must not block other tasks",
			claimed.ID, other.ID)
	}

	// Nothing else is claimable while both tasks are running.
	if got, ok, err := db.ClaimNext(t.Context(), time.Now()); err != nil || ok {
		t.Fatalf("claimed #%d while its task was running", got.ID)
	}

	// Once the first finishes, the queued row of the same task follows it.
	if err := db.FinishSubmission(t.Context(), first.ID, SubmissionResult{Status: StatusDone}); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = db.ClaimNext(t.Context(), time.Now())
	if err != nil || !ok {
		t.Fatalf("third claim: ok=%v err=%v", ok, err)
	}
	if claimed.ID != second.ID {
		t.Fatalf("claimed #%d, want #%d", claimed.ID, second.ID)
	}
}

// TestRequeueRunning: startup recovery returns running rows to the queue.
func TestRequeueRunning(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	enqueueAcrossTasks(t, db, u.ID, 3)
	for range 2 {
		if _, ok, err := db.ClaimNext(t.Context(), time.Now()); err != nil || !ok {
			t.Fatalf("claim failed: ok=%v err=%v", ok, err)
		}
	}

	n, err := db.RequeueRunning(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("requeued %d, want 2", n)
	}
	// All three are claimable again.
	for i := range 3 {
		if _, ok, err := db.ClaimNext(t.Context(), time.Now()); err != nil || !ok {
			t.Fatalf("claim %d after requeue: ok=%v err=%v", i, ok, err)
		}
	}
}

// TestLastByUserTask: the newest row of the pair wins regardless of status,
// ties break on id, and an untouched task reports "nothing recorded".
func TestLastByUserTask(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	now := time.Now()

	if _, ok, err := db.LastByUserTask(t.Context(), u.ID, "t1"); err != nil || ok {
		t.Fatalf("untouched task: ok=%v err=%v, want false/nil", ok, err)
	}

	if _, err := db.Enqueue(t.Context(), NewSubmission{UserID: u.ID, TaskID: "t1",
		CommitSHA: "a", ReceivedAt: now.Add(-time.Hour), Counts: true}); err != nil {
		t.Fatal(err)
	}
	failed, err := db.Enqueue(t.Context(), NewSubmission{UserID: u.ID, TaskID: "t1",
		CommitSHA: "b", ReceivedAt: now, Counts: true})
	if err != nil {
		t.Fatal(err)
	}
	// A row that left the queue is still the last thing recorded for the pair.
	if err := db.ScheduleRetry(t.Context(), failed.ID, nil, "boom"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.LastByUserTask(t.Context(), u.ID, "t1")
	if err != nil || !ok {
		t.Fatalf("after two rows: ok=%v err=%v", ok, err)
	}
	if got.ID != failed.ID || got.Status != StatusInfraError {
		t.Errorf("last = #%d %q, want #%d %q", got.ID, got.Status, failed.ID, StatusInfraError)
	}

	// Equal received_at: the higher id is the later row.
	tie, err := db.Enqueue(t.Context(), NewSubmission{UserID: u.ID, TaskID: "t1",
		CommitSHA: "c", ReceivedAt: now, Counts: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := db.LastByUserTask(t.Context(), u.ID, "t1"); got.ID != tie.ID {
		t.Errorf("tie broken to #%d, want the higher id #%d", got.ID, tie.ID)
	}

	// Another task of the same student is a separate history.
	if _, ok, _ := db.LastByUserTask(t.Context(), u.ID, "t2"); ok {
		t.Error("t2 must report nothing recorded")
	}
}

// TestAdmitSubmission: decide sees the (user, task) history and its verdict
// decides both the row written and its Counts flag.
func TestAdmitSubmission(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	ns := func(sha string) NewSubmission {
		// Counts is deliberately wrong here: the verdict must win.
		return NewSubmission{UserID: u.ID, TaskID: "t1", CommitSHA: sha,
			ReceivedAt: time.Now(), Counts: true}
	}

	var seen int
	first, err := db.AdmitSubmission(t.Context(), ns("a"), func(h []Submission) Admission {
		seen = len(h)
		return Admission{Admit: true, Counts: true}
	})
	if err != nil || seen != 0 {
		t.Fatalf("first admit: history len %d, err %v", seen, err)
	}
	if first.Status != StatusQueued || !first.Counts {
		t.Errorf("first = %q counts=%v, want queued/true", first.Status, first.Counts)
	}

	// Non-counting verdict (teacher recheck) overrides the caller's flag.
	recheck, err := db.AdmitSubmission(t.Context(), ns("b"), func(h []Submission) Admission {
		seen = len(h)
		return Admission{Admit: true, Counts: false}
	})
	if err != nil || seen != 1 {
		t.Fatalf("second admit: history len %d, err %v", seen, err)
	}
	if recheck.Counts || recheck.AttemptNo != nil {
		t.Errorf("recheck = counts %v attempt %v, want false/nil", recheck.Counts, recheck.AttemptNo)
	}

	// The reject reason is persisted: it is all the student has to explain a
	// row that never ran, and nothing else ever writes this row's note.
	const reason = "attempt limit reached (2 of 2)"
	rejected, err := db.AdmitSubmission(t.Context(), ns("c"), func(h []Submission) Admission {
		return Admission{RejectStatus: StatusRejectedLimit, RejectReason: reason}
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != StatusRejectedLimit || rejected.Counts {
		t.Errorf("rejected = %q counts=%v, want rejected_limit/false", rejected.Status, rejected.Counts)
	}
	if rejected.WorkerNote != reason {
		t.Errorf("rejected note = %q, want %q", rejected.WorkerNote, reason)
	}
	if reread, _, err := db.GetSubmission(t.Context(), rejected.ID); err != nil || reread.WorkerNote != reason {
		t.Errorf("reread note = %q (err %v), want %q", reread.WorkerNote, err, reason)
	}

	// An invalid verdict must not leave a row behind.
	if _, err := db.AdmitSubmission(t.Context(), ns("d"), func(h []Submission) Admission {
		return Admission{RejectStatus: "nonsense"}
	}); err == nil {
		t.Fatal("invalid reject status must fail")
	}
	all, err := db.ListByUserTask(t.Context(), u.ID, "t1")
	if err != nil || len(all) != 3 {
		t.Fatalf("history has %d rows, want 3 (the failed admit rolled back): %v", len(all), err)
	}
}

// TestEnqueueAttemptNumbering: counting submissions get a gap-free sequence;
// teacher rechecks get nil and do not bump it.
func TestEnqueueAttemptNumbering(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	s1, _ := db.Enqueue(t.Context(), NewSubmission{UserID: u.ID, TaskID: "t1", CommitSHA: "a", ReceivedAt: time.Now(), Counts: true})
	recheck, _ := db.Enqueue(t.Context(), NewSubmission{UserID: u.ID, TaskID: "t1", CommitSHA: "b", ReceivedAt: time.Now(), Counts: false})
	s2, _ := db.Enqueue(t.Context(), NewSubmission{UserID: u.ID, TaskID: "t1", CommitSHA: "c", ReceivedAt: time.Now(), Counts: true})
	other, _ := db.Enqueue(t.Context(), NewSubmission{UserID: u.ID, TaskID: "t2", CommitSHA: "d", ReceivedAt: time.Now(), Counts: true})

	if s1.AttemptNo == nil || *s1.AttemptNo != 1 {
		t.Errorf("s1 attempt: %v", s1.AttemptNo)
	}
	if recheck.AttemptNo != nil {
		t.Errorf("recheck must have nil attempt, got %v", *recheck.AttemptNo)
	}
	if s2.AttemptNo == nil || *s2.AttemptNo != 2 {
		t.Errorf("s2 attempt: %v", s2.AttemptNo)
	}
	if other.AttemptNo == nil || *other.AttemptNo != 1 {
		t.Errorf("t2 sequence must be independent: %v", other.AttemptNo)
	}
}

// TestFinishSubmissionPersistsScoresAndChecks.
func TestFinishSubmissionPersistsScoresAndChecks(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	sub := enqueueN(t, db, u.ID, "t1", 1)[0]
	if _, ok, _ := db.ClaimNext(t.Context(), time.Now()); !ok {
		t.Fatal("claim failed")
	}

	err := db.FinishSubmission(t.Context(), sub.ID, SubmissionResult{
		Status: StatusDone, Raw: 60, Penalty: 10, Final: 54,
		Checks: []CheckRow{
			{Name: "build", Passed: true, Weight: 0, Duration: 120 * time.Millisecond},
			{Name: "basic", Passed: true, Weight: 60, Duration: 300 * time.Millisecond},
			{Name: "advanced", Passed: false, ExitCode: 1, Weight: 40, LogExcerpt: "boom"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, checks, err := db.GetSubmission(t.Context(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.RawScore == nil || *got.RawScore != 60 ||
		*got.PenaltyPercent != 10 || *got.FinalScore != 54 {
		t.Errorf("submission: %+v", got)
	}
	if len(checks) != 3 || checks[2].LogExcerpt != "boom" || checks[1].Duration != 300*time.Millisecond {
		t.Errorf("checks: %+v", checks)
	}
}

// TestScheduleRetryEligibility: an infra_error row is claimable only after
// retry_at, and never when terminal (retry_at nil).
func TestScheduleRetryEligibility(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	sub := enqueueN(t, db, u.ID, "t1", 1)[0]
	if _, ok, _ := db.ClaimNext(t.Context(), time.Now()); !ok {
		t.Fatal("claim failed")
	}

	at := time.Now().Add(time.Hour)
	if err := db.ScheduleRetry(t.Context(), sub.ID, &at, "docker down"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.ClaimNext(t.Context(), time.Now()); ok {
		t.Fatal("must not be claimable before retry_at")
	}
	if _, ok, _ := db.ClaimNext(t.Context(), time.Now().Add(2*time.Hour)); !ok {
		t.Fatal("must be claimable after retry_at")
	}

	// Terminal: retry_at nil.
	if err := db.ScheduleRetry(t.Context(), sub.ID, nil, "retries exhausted"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.ClaimNext(t.Context(), time.Now().Add(24*time.Hour)); ok {
		t.Fatal("terminal infra_error must never be claimable")
	}
	got, _, _ := db.GetSubmission(t.Context(), sub.ID)
	if got.Retries != 2 || got.Status != StatusInfraError {
		t.Errorf("terminal row: %+v", got)
	}
}
