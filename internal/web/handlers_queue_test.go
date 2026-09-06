package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// terminal infra_error state the queue view shows as `error` the way a worker
// does: claim it, then ScheduleRetry with a nil retryAt (retries exhausted).
// The note is an operator's, as an unclassified infra failure leaves it.
func erroredRow(t *testing.T, h *Handler, login string) (store.User, store.Submission) {
	t.Helper()
	return infraRow(t, h, login, nil, "boom", "")
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

// TestQueueHidesRecheckForRemovedTask: the row keeps its terminal `error`
// status, but the course has no task left to re-grade the commit against, so
// the button is not offered - neither on the page nor on the row the stream
// re-renders over it (SPEC §13).
func TestQueueHidesRecheckForRemovedTask(t *testing.T) {
	h, teacher := newTestSite(t)
	h.Local = &teacher
	_, sub := erroredRow(t, h, "bob") // the bare test course has no tasks at all

	form := "/queue/" + itoa(sub.ID) + "/recheck"
	page := pageBody(t, h, "/queue")
	if !strings.Contains(page, "#"+itoa(sub.ID)) {
		t.Fatalf("the queue dropped the row itself:\n%s", page)
	}
	if strings.Contains(page, form) {
		t.Errorf("the queue offers a recheck for a task the course lost:\n%s", page)
	}
	marker := "event: sub-" + itoa(sub.ID)
	if got := stream(t, h, "/queue/stream?ids="+itoa(sub.ID), marker); strings.Contains(got, form) {
		t.Errorf("the stream put the recheck button back:\n%s", got)
	}
	// The same row with the task present is the control: the snapshot is the
	// only thing that decides this.
	setCourse(h)
	if page := pageBody(t, h, "/queue"); !strings.Contains(page, form) {
		t.Errorf("no recheck button while the task exists:\n%s", page)
	}
}

// captureLog swaps the default slog sink for the test's own buffer. The
// handlers log through the package-level logger, and what reaches it is half of
// what a refused recheck is about: httpError renders a catalog string and keeps
// nothing, so the log is where the task id and the cause have to land.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestRecheckRemovedTaskIsNotFound: a POST that reaches a recheck for a task
// the snapshot no longer has (a page held open across the teacher's removal
// push, or a hand-written request) is refused with a 404 that says why - the
// course losing a task is a state the server understands, not an internal
// failure. All three routes have to answer the same, and each has to leave the
// original error in the log.
func TestRecheckRemovedTaskIsNotFound(t *testing.T) {
	// The error intake builds for this, id and all (recheck.go).
	gone := fmt.Errorf("retired: %w", queue.ErrTaskGone)
	for _, tc := range []struct {
		name string
		path func(student store.User, sub store.Submission) string
	}{
		{"queue row", func(_ store.User, sub store.Submission) string {
			return "/queue/" + itoa(sub.ID) + "/recheck"
		}},
		{"task page", func(store.User, store.Submission) string {
			return "/tasks/retired/recheck"
		}},
		{"student page", func(student store.User, _ store.Submission) string {
			return "/students/" + student.Login + "/tasks/retired/recheck"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, teacher := newTestSite(t)
			setCourse(h)
			student, sub := erroredRow(t, h, "bob")
			h.Local = &teacher
			h.Recheck = &fakeRechecker{err: gone}
			logged := captureLog(t)

			rec := post(t, h, tc.path(student, sub))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d, want 404", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "no longer in the course") {
				t.Errorf("the refusal does not say why:\n%s", rec.Body.String())
			}
			if !strings.Contains(logged.String(), "retired") {
				t.Errorf("the log does not name the task:\n%s", logged.String())
			}
		})
	}
}

// TestRecheckStoreFailureStaysOpaque: everything that is not the missing task
// is still one 500 with nothing in the body. A store failure describes the
// server, and /tasks/{id}/recheck is the student's own route - so the detail
// goes to the log instead of being thrown away with it.
func TestRecheckStoreFailureStaysOpaque(t *testing.T) {
	h, teacher := newTestSite(t)
	setCourse(h)
	_, sub := erroredRow(t, h, "bob")
	h.Local = &teacher
	h.Recheck = &fakeRechecker{err: errors.New("sqlite: disk I/O error reading anygrade.db")}
	logged := captureLog(t)

	rec := post(t, h, "/queue/"+itoa(sub.ID)+"/recheck")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "sqlite") || strings.Contains(body, "anygrade.db") {
		t.Errorf("the 500 body leaks the cause:\n%s", body)
	}
	if !strings.Contains(logged.String(), "disk I/O error") {
		t.Errorf("the cause did not reach the log:\n%s", logged.String())
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
