package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/store"
)

// fakeRechecker stands in for intake.Server: it records the pair it was asked
// to recheck and hands back a canned outcome.
type fakeRechecker struct {
	sub  store.Submission
	warn intake.RecheckWarning
	err  error

	gotUserID int64
	gotTaskID string
	calls     int
}

func (f *fakeRechecker) Recheck(ctx context.Context, userID int64, taskID string) (store.Submission, queue.Decision, intake.RecheckWarning, error) {
	sub, warn, err := f.TeacherRecheck(ctx, store.User{Role: "teacher"}, userID, taskID)
	return sub, queue.Decision{Admit: true}, warn, err
}

func (f *fakeRechecker) TeacherRecheck(_ context.Context, _ store.User, targetUserID int64, taskID string) (store.Submission, intake.RecheckWarning, error) {
	f.calls++
	f.gotUserID, f.gotTaskID = targetUserID, taskID
	return f.sub, f.warn, f.err
}

// erroredRow seeds one submission for a fresh student and drives it to the
// terminal infra_error state the queue view shows as `error` (retries
// exhausted: ScheduleRetry with a nil retryAt).
func erroredRow(t *testing.T, h *Handler, login string) (store.User, store.Submission) {
	t.Helper()
	student, err := h.DB.CreateUser(t.Context(), login, "Student", "student")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	sub, err := h.DB.Enqueue(t.Context(), store.NewSubmission{
		UserID: student.ID, TaskID: "t1", CommitSHA: "deadbeef",
		ReceivedAt: time.Now(), Counts: true,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := h.DB.ScheduleRetry(t.Context(), sub.ID, nil, "boom"); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}
	return student, sub
}

func post(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec
}

// TestQueueRecheckIsTeacherOnly: a student must see 404, never 403 - the route
// does not leak its own existence (SPEC §14).
func TestQueueRecheckIsTeacherOnly(t *testing.T) {
	h, _ := newTestSite(t)
	student, sub := erroredRow(t, h, "bob")
	h.Local = &student
	h.Recheck = &fakeRechecker{}

	rec := post(t, h, "/queue/"+itoa(sub.ID)+"/recheck")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if h.Recheck.(*fakeRechecker).calls != 0 {
		t.Error("a student's POST must not reach the rechecker")
	}
}

// TestQueueRecheckTeacherQueuesAndRedirects: the teacher's click rechecks the
// row's (student, task) pair and lands on the newly queued submission.
func TestQueueRecheckTeacherQueuesAndRedirects(t *testing.T) {
	h, teacher := newTestSite(t)
	student, sub := erroredRow(t, h, "bob")
	h.Local = &teacher

	fresh, err := h.DB.Enqueue(t.Context(), store.NewSubmission{
		UserID: student.ID, TaskID: "t1", CommitSHA: "deadbeef", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	f := &fakeRechecker{sub: fresh}
	h.Recheck = f

	rec := post(t, h, "/queue/"+itoa(sub.ID)+"/recheck")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/submissions/"+itoa(fresh.ID) {
		t.Errorf("Location = %q, want /submissions/%d", got, fresh.ID)
	}
	if f.gotUserID != student.ID || f.gotTaskID != "t1" {
		t.Errorf("rechecked (%d, %q), want (%d, %q)", f.gotUserID, f.gotTaskID, student.ID, "t1")
	}
	if _, _, err := h.DB.GetSubmission(t.Context(), fresh.ID); err != nil {
		t.Errorf("the queued submission is gone: %v", err)
	}
}

// TestQueueRecheckSurfacesPinWarning: a failed pin does not fail the recheck -
// the submission stands and the warning rides along as a flash code.
func TestQueueRecheckSurfacesPinWarning(t *testing.T) {
	h, teacher := newTestSite(t)
	student, sub := erroredRow(t, h, "bob")
	h.Local = &teacher

	fresh, err := h.DB.Enqueue(t.Context(), store.NewSubmission{
		UserID: student.ID, TaskID: "t1", CommitSHA: "deadbeef", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	h.Recheck = &fakeRechecker{sub: fresh, warn: intake.WarnCommitNotPinned}

	rec := post(t, h, "/queue/"+itoa(sub.ID)+"/recheck")
	want := "/submissions/" + itoa(fresh.ID) + "?flash=commit_not_pinned"
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
		t.Fatalf("status %d, Location %q, want 303 to %q",
			rec.Code, rec.Header().Get("Location"), want)
	}
	if _, _, err := h.DB.GetSubmission(t.Context(), fresh.ID); err != nil {
		t.Errorf("the submission must survive a failed pin: %v", err)
	}
}

// TestSubmissionPageRendersRecheckWarning closes the loop: the flash code the
// redirect carries is rendered as localized text, not echoed raw.
func TestSubmissionPageRendersRecheckWarning(t *testing.T) {
	h, teacher := newTestSite(t)
	_, sub := erroredRow(t, h, "bob")
	h.Local = &teacher

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/submissions/"+itoa(sub.ID)+"?flash=commit_not_pinned", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "the commit could not be pinned") {
		t.Errorf("submission page shows no pin warning:\n%s", rec.Body.String())
	}
}

// TestQueueRecheckNothingToRecheck: the pair has no counting commit (a teacher
// POSTing at a canceled row can reach this), so the queue view says so.
func TestQueueRecheckNothingToRecheck(t *testing.T) {
	h, teacher := newTestSite(t)
	_, sub := erroredRow(t, h, "bob")
	h.Local = &teacher
	h.Recheck = &fakeRechecker{err: intake.ErrNothingToRecheck}

	rec := post(t, h, "/queue/"+itoa(sub.ID)+"/recheck")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/queue?flash=nothing_to_recheck" {
		t.Fatalf("status %d, Location %q, want 303 to /queue?flash=nothing_to_recheck",
			rec.Code, rec.Header().Get("Location"))
	}
}

// TestQueueRecheckUnknownSubmission: an id with no row is a 404, like cancel.
func TestQueueRecheckUnknownSubmission(t *testing.T) {
	h, teacher := newTestSite(t)
	h.Local = &teacher
	h.Recheck = &fakeRechecker{}

	if rec := post(t, h, "/queue/404/recheck"); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

// TestQueueRowActions pins which display statuses offer which button. Recheck
// belongs to the terminal `error` rows only: `retrying` re-runs by itself and
// `canceled` no longer counts, so a recheck there would grade another commit.
func TestQueueRowActions(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name        string
		sub         store.Submission
		wantStatus  string
		wantCancel  bool
		wantRecheck bool
	}{
		{name: "queued", sub: store.Submission{ID: 1, Status: store.StatusQueued},
			wantStatus: store.StatusQueued, wantCancel: true},
		{name: "running", sub: store.Submission{ID: 2, Status: store.StatusRunning},
			wantStatus: store.StatusRunning, wantCancel: true},
		{name: "error", sub: store.Submission{ID: 3, Status: store.StatusInfraError},
			wantStatus: gradebook.StatusError, wantRecheck: true},
		{name: "retrying", sub: store.Submission{ID: 4, Status: store.StatusInfraError, RetryAt: &now},
			wantStatus: gradebook.StatusRetrying},
		{name: "canceled", sub: store.Submission{ID: 5, Status: store.StatusInfraError, CanceledAt: &now},
			wantStatus: gradebook.StatusCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := queueRow{Sub: tc.sub, Login: "bob", Status: subDisplayStatus(tc.sub)}
			if row.Status != tc.wantStatus {
				t.Fatalf("display status = %q, want %q", row.Status, tc.wantStatus)
			}
			html, err := renderPartial("en", "queue-row", row)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			cancel := strings.Contains(html, "/cancel")
			recheck := strings.Contains(html, "/recheck")
			if cancel != tc.wantCancel || recheck != tc.wantRecheck {
				t.Errorf("cancel=%v recheck=%v, want cancel=%v recheck=%v\n%s",
					cancel, recheck, tc.wantCancel, tc.wantRecheck, html)
			}
		})
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
