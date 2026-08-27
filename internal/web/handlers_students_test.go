package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/store"
)

// setCourse gives the test site two tasks: the student page's task table and
// the ?task= filter both key off course metadata.
func setCourse(h *Handler) {
	h.Course.Set(&intake.Course{Resolved: &config.Resolved{
		Course: config.ResolvedCourse{Name: "Test course"},
		Tasks: []config.ResolvedTask{
			{ID: "t1", Name: "Task one", Score: 10},
			{ID: "t2", Name: "Task two", Score: 10},
		},
	}})
}

// newSession creates a user and a live session cookie for them, so tests drive
// the real auth path instead of the local-mode bypass.
func newSession(t *testing.T, h *Handler, login, role string) (store.User, *http.Cookie) {
	t.Helper()
	u, err := h.DB.CreateUser(t.Context(), login, "", role)
	if err != nil {
		t.Fatalf("create user %s: %v", login, err)
	}
	token, err := h.DB.IssueToken(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("issue token for %s: %v", login, err)
	}
	sid, err := h.DB.CreateSession(t.Context(), u.ID, token, sessionTTL)
	if err != nil {
		t.Fatalf("create session for %s: %v", login, err)
	}
	return u, &http.Cookie{Name: sessionCookie, Value: sid}
}

// do issues one request as the session owner.
func do(h *Handler, method, target string, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)
	return rec
}

// doForm is do with a urlencoded body, for the POSTs that carry form fields.
func doForm(h *Handler, target string, c *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)
	return rec
}

// enqueue records one submission and returns its id.
func enqueue(t *testing.T, h *Handler, userID int64, taskID string, at time.Time) int64 {
	t.Helper()
	sub, err := h.DB.Enqueue(t.Context(), store.NewSubmission{
		UserID: userID, TaskID: taskID, CommitSHA: "deadbeef",
		ReceivedAt: at, Counts: true,
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", taskID, err)
	}
	return sub.ID
}

// TestStudentPageListsEverySubmission: the teacher's student page links to
// every submission of the student, not just the latest one per task.
func TestStudentPageListsEverySubmission(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, teacher := newSession(t, h, "teacher", "teacher")
	alice, _ := newSession(t, h, "alice", "student")

	now := time.Now()
	first := enqueue(t, h, alice.ID, "t1", now.Add(-2*time.Hour))
	second := enqueue(t, h, alice.ID, "t1", now.Add(-time.Hour))
	other := enqueue(t, h, alice.ID, "t2", now)

	rec := do(h, http.MethodGet, "/students/alice", teacher)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /students/alice: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, id := range []int64{first, second, other} {
		if want := fmt.Sprintf(`href="/submissions/%d"`, id); !strings.Contains(body, want) {
			t.Errorf("student page is missing %s:\n%s", want, body)
		}
	}
	// Newest first, so the t2 submission leads the table.
	if i, j := strings.Index(body, `href="/submissions/3"`), strings.Index(body, `href="/submissions/1"`); i > j {
		t.Errorf("submissions are not newest first (#3 at %d, #1 at %d)", i, j)
	}
}

// TestStudentPageTaskFilter: ?task= narrows the list to one (student, task)
// pair - the URL a matrix cell drills down to.
func TestStudentPageTaskFilter(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, teacher := newSession(t, h, "teacher", "teacher")
	alice, _ := newSession(t, h, "alice", "student")

	now := time.Now()
	first := enqueue(t, h, alice.ID, "t1", now.Add(-2*time.Hour))
	second := enqueue(t, h, alice.ID, "t1", now.Add(-time.Hour))
	other := enqueue(t, h, alice.ID, "t2", now)

	rec := do(h, http.MethodGet, "/students/alice?task=t1", teacher)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /students/alice?task=t1: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, id := range []int64{first, second} {
		if want := fmt.Sprintf(`href="/submissions/%d"`, id); !strings.Contains(body, want) {
			t.Errorf("t1 history is missing %s:\n%s", want, body)
		}
	}
	if got := fmt.Sprintf(`href="/submissions/%d"`, other); strings.Contains(body, got) {
		t.Errorf("t1 history leaked the t2 submission %s:\n%s", got, body)
	}
}

// TestStudentPageForeignTaskFilterIsEmpty: an unknown task id lists nothing
// rather than falling back to every submission.
func TestStudentPageForeignTaskFilterIsEmpty(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, teacher := newSession(t, h, "teacher", "teacher")
	alice, _ := newSession(t, h, "alice", "student")
	enqueue(t, h, alice.ID, "t1", time.Now())

	rec := do(h, http.MethodGet, "/students/alice?task=nope", teacher)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, `href="/submissions/1"`) {
		t.Errorf("unknown task filter still listed submissions:\n%s", body)
	}
}

// TestStudentRoutesAreTeacherOnly: a student sees 404 (not 403) on every
// student-management route, including the new ones (SPEC §14).
func TestStudentRoutesAreTeacherOnly(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	alice, _ := newSession(t, h, "alice", "student")
	_, bob := newSession(t, h, "bob", "student")
	key, err := h.DB.AddSSHKey(t.Context(), alice.ID, "SHA256:aaa", "ssh-ed25519 AAA alice")
	if err != nil {
		t.Fatalf("add key: %v", err)
	}

	cases := []struct{ method, target string }{
		{http.MethodGet, "/students/alice"},
		{http.MethodGet, "/students/alice?task=t1"},
		{http.MethodPost, fmt.Sprintf("/students/alice/keys/%d/delete", key.ID)},
	}
	for _, c := range cases {
		rec := do(h, c.method, c.target, bob)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as a student: status %d, want 404", c.method, c.target, rec.Code)
		}
	}
	// The key survived the forbidden POST.
	if keys, _ := h.DB.ListSSHKeys(t.Context(), alice.ID); len(keys) != 1 {
		t.Errorf("alice has %d keys, want 1", len(keys))
	}
}

// TestAdminDeleteKey: the teacher removes the target's key (not their own),
// and the action lands in the audit log.
func TestAdminDeleteKey(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, teacher := newSession(t, h, "teacher", "teacher")
	alice, _ := newSession(t, h, "alice", "student")
	bob, _ := newSession(t, h, "bob", "student")

	aliceKey, err := h.DB.AddSSHKey(t.Context(), alice.ID, "SHA256:aaa", "ssh-ed25519 AAA alice")
	if err != nil {
		t.Fatalf("add alice key: %v", err)
	}
	bobKey, err := h.DB.AddSSHKey(t.Context(), bob.ID, "SHA256:bbb", "ssh-ed25519 BBB bob")
	if err != nil {
		t.Fatalf("add bob key: %v", err)
	}

	aliceKeyURL := fmt.Sprintf("/students/alice/keys/%d/delete", aliceKey.ID)

	// A stale fingerprint is a 404: the id may have been handed to a key the
	// student added after the page was rendered (SQLite reuses freed rowids),
	// and deleting that one would be the wrong key with a lying audit entry.
	rec := doForm(h, aliceKeyURL, teacher, url.Values{"fingerprint": {"SHA256:stale"}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("stale-fingerprint delete: status %d, want 404", rec.Code)
	}
	if keys, _ := h.DB.ListSSHKeys(t.Context(), alice.ID); len(keys) != 1 {
		t.Errorf("stale-fingerprint delete removed the key")
	}
	if events, _ := h.DB.ListEvents(t.Context(), "key.delete", "alice", 10, 0); len(events) != 0 {
		t.Errorf("stale-fingerprint delete wrote %d audit events, want 0", len(events))
	}

	rec = doForm(h, aliceKeyURL, teacher, url.Values{"fingerprint": {aliceKey.Fingerprint}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/students/alice" {
		t.Fatalf("delete: status %d, Location %q, want 303 to /students/alice",
			rec.Code, rec.Header().Get("Location"))
	}
	if keys, _ := h.DB.ListSSHKeys(t.Context(), alice.ID); len(keys) != 0 {
		t.Errorf("alice still has %d keys, want 0", len(keys))
	}
	if keys, _ := h.DB.ListSSHKeys(t.Context(), bob.ID); len(keys) != 1 {
		t.Errorf("bob has %d keys, want his own untouched", len(keys))
	}

	events, err := h.DB.ListEvents(t.Context(), "key.delete", "alice", 10, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d key.delete events, want 1", len(events))
	}
	if events[0].ActorLogin != "teacher" || events[0].Detail != aliceKey.Fingerprint {
		t.Errorf("audit event = %+v, want actor teacher and the fingerprint as detail", events[0])
	}

	// A key id belonging to somebody else is a 404, and bob keeps his key.
	rec = doForm(h, fmt.Sprintf("/students/alice/keys/%d/delete", bobKey.ID), teacher,
		url.Values{"fingerprint": {bobKey.Fingerprint}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-user delete: status %d, want 404", rec.Code)
	}
	if keys, _ := h.DB.ListSSHKeys(t.Context(), bob.ID); len(keys) != 1 {
		t.Errorf("cross-user delete removed bob's key")
	}
}

// TestStudentPageRendersKeyDeleteForm: the teacher's key list is actionable,
// not read-only.
func TestStudentPageRendersKeyDeleteForm(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, teacher := newSession(t, h, "teacher", "teacher")
	alice, _ := newSession(t, h, "alice", "student")
	key, err := h.DB.AddSSHKey(t.Context(), alice.ID, "SHA256:aaa", "ssh-ed25519 AAA alice")
	if err != nil {
		t.Fatalf("add key: %v", err)
	}

	body := do(h, http.MethodGet, "/students/alice", teacher).Body.String()
	if want := fmt.Sprintf(`action="/students/alice/keys/%d/delete"`, key.ID); !strings.Contains(body, want) {
		t.Errorf("student page has no key delete form (%s):\n%s", want, body)
	}
	// The form carries the fingerprint, which is what makes the delete safe.
	if want := fmt.Sprintf(`name="fingerprint" value="%s"`, key.Fingerprint); !strings.Contains(body, want) {
		t.Errorf("key delete form does not carry the fingerprint (%s):\n%s", want, body)
	}
}

// TestAccountStateIsNotAVerdict: an account's state is not a check result. A
// disabled account used to render with st-failed, borrowing the failure color
// from the verdict palette; it is a warn stamp now, and an active account a
// neutral one (SPEC.ui.md 5).
func TestAccountStateIsNotAVerdict(t *testing.T) {
	h, _ := newTestSite(t)
	setCourse(h)
	_, teacherCookie := newSession(t, h, "prof", "teacher")
	newSession(t, h, "stud", "student")
	// SetUserState is keyed by login, not id (store/tokens.go:73).
	if err := h.DB.SetUserState(t.Context(), "stud", "disabled"); err != nil {
		t.Fatalf("disable student: %v", err)
	}

	for _, target := range []string{"/students", "/students/stud"} {
		body := do(h, http.MethodGet, target, teacherCookie).Body.String()
		if strings.Contains(body, "st-failed") {
			t.Errorf("%s: a disabled account must not borrow the failure stamp:\n%s", target, body)
		}
		if !strings.Contains(body, "st-partial") {
			t.Errorf("%s: a disabled account should carry the warn stamp:\n%s", target, body)
		}
	}
}
