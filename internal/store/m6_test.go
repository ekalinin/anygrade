package store

import (
	"testing"
	"time"
)

// TestCancelSubmissionQueued: a teacher can cancel a queued submission.
func TestCancelSubmissionQueued(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	sub := enqueueN(t, db, u.ID, "t1", 1)[0]

	got, ok, err := db.CancelSubmission(t.Context(), sub.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("cancel of queued submission must succeed")
	}
	if got.Status != StatusInfraError {
		t.Errorf("status = %q, want %q", got.Status, StatusInfraError)
	}
	if got.RetryAt != nil {
		t.Errorf("retry_at = %v, want nil", got.RetryAt)
	}
	if got.Counts {
		t.Error("counts must be false after cancel")
	}
	if got.CanceledAt == nil {
		t.Error("canceled_at must be set")
	}
	if got.WorkerNote != "canceled by teacher" {
		t.Errorf("worker_note = %q", got.WorkerNote)
	}

	if _, ok, _ := db.ClaimNext(t.Context(), time.Now().Add(24*time.Hour)); ok {
		t.Fatal("canceled submission must not be claimable")
	}

	if _, ok, err := db.CancelSubmission(t.Context(), sub.ID, time.Now()); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("canceling an already-canceled submission must return ok=false")
	}
}

// TestCancelSubmissionRunning: a teacher can cancel a running submission.
func TestCancelSubmissionRunning(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	sub := enqueueN(t, db, u.ID, "t1", 1)[0]
	if _, ok, err := db.ClaimNext(t.Context(), time.Now()); err != nil || !ok {
		t.Fatalf("claim failed: ok=%v err=%v", ok, err)
	}

	got, ok, err := db.CancelSubmission(t.Context(), sub.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("cancel of running submission must succeed")
	}
	if got.Status != StatusInfraError {
		t.Errorf("status = %q, want %q", got.Status, StatusInfraError)
	}
	if got.RetryAt != nil {
		t.Errorf("retry_at = %v, want nil", got.RetryAt)
	}
	if got.Counts {
		t.Error("counts must be false after cancel")
	}
	if got.CanceledAt == nil {
		t.Error("canceled_at must be set")
	}
	if got.WorkerNote != "canceled by teacher" {
		t.Errorf("worker_note = %q", got.WorkerNote)
	}
}

// TestCancelSubmissionDone: a terminal (done) submission cannot be canceled.
func TestCancelSubmissionDone(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	sub := enqueueN(t, db, u.ID, "t1", 1)[0]
	if _, ok, err := db.ClaimNext(t.Context(), time.Now()); err != nil || !ok {
		t.Fatalf("claim failed: ok=%v err=%v", ok, err)
	}
	if err := db.FinishSubmission(t.Context(), sub.ID, SubmissionResult{
		Status: StatusDone, Raw: 100, Penalty: 0, Final: 100,
	}); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := db.CancelSubmission(t.Context(), sub.ID, time.Now()); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("canceling a done submission must return ok=false")
	}
	got, _, err := db.GetSubmission(t.Context(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone {
		t.Errorf("status = %q, want %q (unchanged)", got.Status, StatusDone)
	}
}

// TestListActiveAndAll: ListActive covers queued/running/infra_error;
// ListAllSubmissions returns everything ordered by user, task, time.
func TestListActiveAndAll(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	subs1 := enqueueN(t, db, u.ID, "t1", 2)
	subs2 := enqueueN(t, db, u.ID, "t2", 1)

	// Claim the oldest submission (t1's first) -> running.
	claimed, ok, err := db.ClaimNext(t.Context(), time.Now())
	if err != nil || !ok {
		t.Fatalf("claim failed: ok=%v err=%v", ok, err)
	}
	if claimed.ID != subs1[0].ID {
		t.Fatalf("claimed %d, want %d (oldest first)", claimed.ID, subs1[0].ID)
	}

	// Claim and finish a second submission (FinishSubmission only accepts
	// running rows: the status guard closes the teacher-cancel race).
	second, ok, err := db.ClaimNext(t.Context(), time.Now())
	if err != nil || !ok {
		t.Fatalf("second claim: ok=%v err=%v", ok, err)
	}
	if err := db.FinishSubmission(t.Context(), second.ID, SubmissionResult{
		Status: StatusDone, Raw: 100, Penalty: 0, Final: 100,
	}); err != nil {
		t.Fatal(err)
	}
	_ = subs2

	active, err := db.ListActive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("ListActive returned %d, want 2 (t1's running + t2's queued)", len(active))
	}
	if active[0].ReceivedAt.Before(active[1].ReceivedAt) {
		t.Error("ListActive must be newest first")
	}

	all, err := db.ListAllSubmissions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("ListAllSubmissions returned %d, want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].UserID < all[i-1].UserID {
			t.Fatal("ListAllSubmissions must be ordered by user_id")
		}
	}
	if all[0].TaskID != "t1" || all[1].TaskID != "t1" || all[2].TaskID != "t2" {
		t.Errorf("ListAllSubmissions task order: %q, %q, %q", all[0].TaskID, all[1].TaskID, all[2].TaskID)
	}
}

// TestInviteLifecycle exercises create, verify, expiry, and one-shot use.
func TestInviteLifecycle(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	if err := db.CreateInvite(t.Context(), u.ID, "inv-tok", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	inv, ok, err := db.VerifyInvite(t.Context(), "inv-tok")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("verify of fresh invite must succeed")
	}
	if inv.UserID != u.ID {
		t.Errorf("invite user = %d, want %d", inv.UserID, u.ID)
	}

	if _, ok, err := db.VerifyInvite(t.Context(), "nope"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("unknown token must not verify")
	}

	if used, err := db.ConsumeInvite(t.Context(), inv.ID, time.Now()); err != nil || !used {
		t.Fatalf("consume: used=%v err=%v", used, err)
	}
	if _, ok, err := db.VerifyInvite(t.Context(), "inv-tok"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("used invite must not verify")
	}
	// Two requests can both pass VerifyInvite before either writes; exactly
	// one of them may then consume the link.
	if used, err := db.ConsumeInvite(t.Context(), inv.ID, time.Now()); err != nil || used {
		t.Fatalf("second consume: used=%v err=%v, want false/nil", used, err)
	}

	if err := db.CreateInvite(t.Context(), u.ID, "expired-tok", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.VerifyInvite(t.Context(), "expired-tok"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("expired invite must not verify")
	}
}

// TestDeleteSSHKeyScoped: deletion is scoped to the key's owner.
func TestDeleteSSHKeyScoped(t *testing.T) {
	db := openTestDB(t)
	a := testUser(t, db)
	b, err := db.CreateUser(t.Context(), "student2", "Student Two", "student")
	if err != nil {
		t.Fatal(err)
	}

	key, err := db.AddSSHKey(t.Context(), a.ID, "SHA256:abc", "ssh-ed25519 AAAA a")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := db.DeleteSSHKey(t.Context(), b.ID, key.ID, key.Fingerprint); err != nil || ok {
		t.Fatalf("cross-user delete = ok %v, err %v; want false/nil", ok, err)
	}
	keys, err := db.ListSSHKeys(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("cross-user delete must not remove the key; got %d keys", len(keys))
	}

	// A stale fingerprint must not delete whatever now holds that id: SQLite
	// hands freed rowids to the next insert, so id alone is not an identity.
	if ok, err := db.DeleteSSHKey(t.Context(), a.ID, key.ID, "SHA256:stale"); err != nil || ok {
		t.Fatalf("stale-fingerprint delete = ok %v, err %v; want false/nil", ok, err)
	}
	if keys, err := db.ListSSHKeys(t.Context(), a.ID); err != nil || len(keys) != 1 {
		t.Fatalf("stale-fingerprint delete removed the key; got %d keys, err %v", len(keys), err)
	}

	if ok, err := db.DeleteSSHKey(t.Context(), a.ID, key.ID, key.Fingerprint); err != nil || !ok {
		t.Fatalf("owner delete = ok %v, err %v; want true/nil", ok, err)
	}
	keys, err = db.ListSSHKeys(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("owner delete must remove the key; got %d keys", len(keys))
	}
}

// TestListEventsByTarget: events for "login" and "login/..." are returned
// newest first, with the actor's login populated when set.
func TestListEventsByTarget(t *testing.T) {
	db := openTestDB(t)
	alice := testUser(t, db)
	bob, err := db.CreateUser(t.Context(), "bob", "Bob", "teacher")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Log(t.Context(), Event{Kind: "signup", Target: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Log(t.Context(), Event{ActorID: &bob.ID, Kind: "cancel", Target: "alice/t1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Log(t.Context(), Event{Kind: "signup", Target: "bob"}); err != nil {
		t.Fatal(err)
	}
	_ = alice

	got, err := db.ListEventsByTarget(t.Context(), "alice", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Target != "alice/t1" || got[0].ActorLogin != "bob" {
		t.Errorf("newest event = %+v", got[0])
	}
	if got[1].Target != "alice" || got[1].ActorLogin != "" {
		t.Errorf("oldest event = %+v", got[1])
	}
}

// TestListEvents: the global audit log filters by exact kind and target
// substring, orders newest first, and supports limit/offset paging.
func TestListEvents(t *testing.T) {
	db := openTestDB(t)
	alice := testUser(t, db)
	bob, err := db.CreateUser(t.Context(), "bob", "Bob", "teacher")
	if err != nil {
		t.Fatal(err)
	}
	_ = alice

	if err := db.Log(t.Context(), Event{Kind: "signup", Target: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Log(t.Context(), Event{ActorID: &bob.ID, Kind: "cancel", Target: "alice/t1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Log(t.Context(), Event{Kind: "signup", Target: "bob"}); err != nil {
		t.Fatal(err)
	}

	// Kind filter.
	got, err := db.ListEvents(t.Context(), "signup", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("kind filter: got %d events, want 2", len(got))
	}
	if got[0].Target != "bob" || got[1].Target != "alice" {
		t.Errorf("kind filter must be newest first: %+v", got)
	}

	// Target substring filter.
	got, err = db.ListEvents(t.Context(), "", "t1", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "cancel" {
		t.Errorf("target filter: got %+v, want one cancel event", got)
	}

	// No filters: everything, newest first.
	got, err = db.ListEvents(t.Context(), "", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("no filter: got %d events, want 3", len(got))
	}
	if got[0].Target != "bob" || got[2].Target != "alice" {
		t.Errorf("no filter order: %+v", got)
	}

	// Limit/offset paging.
	page, err := db.ListEvents(t.Context(), "", "", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Target != got[1].Target {
		t.Errorf("page(1,1) = %+v, want %+v", page, got[1])
	}
}

// TestListEventKinds: distinct kinds are returned in alphabetical order.
func TestListEventKinds(t *testing.T) {
	db := openTestDB(t)
	testUser(t, db)

	if err := db.Log(t.Context(), Event{Kind: "signup", Target: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Log(t.Context(), Event{Kind: "cancel", Target: "alice/t1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Log(t.Context(), Event{Kind: "signup", Target: "bob"}); err != nil {
		t.Fatal(err)
	}

	kinds, err := db.ListEventKinds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 2 || kinds[0] != "cancel" || kinds[1] != "signup" {
		t.Errorf("kinds = %v, want [cancel signup]", kinds)
	}
}
