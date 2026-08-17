package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/runner"
	"github.com/ekalinin/anygrade/internal/store"
)

// finishedWithChecks seeds one finished submission whose results carry the
// given check names, and writes each check's log file where the runner puts it.
func finishedWithChecks(t *testing.T, h *Handler, names ...string) (store.User, store.Submission) {
	t.Helper()
	student, err := h.DB.CreateUser(t.Context(), "bob", "Student", "student")
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
	if _, ok, err := h.DB.ClaimNext(t.Context(), time.Now()); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	rows := make([]store.CheckRow, len(names))
	for i, n := range names {
		rows[i] = store.CheckRow{Name: n, Passed: true, Weight: 1}
	}
	if err := h.DB.FinishSubmission(t.Context(), sub.ID, store.SubmissionResult{
		Status: store.StatusDone, Checks: rows,
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	dir := intake.SubmissionLogDir(h.DataDir, sub.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, runner.LogFileName(n)), []byte("log of "+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return student, sub
}

func getLog(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestSubmissionLogAcceptsAnyValidCheckName: metadata allows any non-empty,
// unique check name, so the download must serve every name the run actually
// produced - including ones with a slash, a space, or non-ASCII, which the old
// `^[A-Za-z0-9._ -]+$` filter rejected outright.
func TestSubmissionLogAcceptsAnyValidCheckName(t *testing.T) {
	h, teacher := newTestSite(t)
	h.DataDir = t.TempDir()
	h.Local = &teacher
	names := []string{"build/all", "go vet", "тесты"}
	_, sub := finishedWithChecks(t, h, names...)

	for _, name := range names {
		// url.URL renders the path the way html/template does in an href: '/'
		// stays a separator, everything else is percent-encoded.
		ref := &url.URL{Path: "/submissions/" + itoa(sub.ID) + "/logs/" + name}
		rec := getLog(t, h, ref.String())
		if rec.Code != http.StatusOK {
			t.Errorf("GET log %q: status %d, want 200", name, rec.Code)
			continue
		}
		if got, want := rec.Body.String(), "log of "+name; got != want {
			t.Errorf("GET log %q: body %q, want %q", name, got, want)
		}
		if cd := rec.Header().Get("Content-Disposition"); cd == "" {
			t.Errorf("GET log %q: no Content-Disposition", name)
		}
	}
}

// TestSubmissionLogRejectsUnknownCheck: the name is validated by membership in
// this submission's results, so anything else is a 404 - including a traversal
// attempt, which cannot reach the filesystem because the name never enters the
// path (runner.LogFileName renders it).
func TestSubmissionLogRejectsUnknownCheck(t *testing.T) {
	h, teacher := newTestSite(t)
	h.DataDir = t.TempDir()
	h.Local = &teacher
	_, sub := finishedWithChecks(t, h, "build")

	// A real file the traversal would be aiming at, one level above the logs.
	secret := filepath.Join(h.DataDir, "logs", "secret.log")
	if err := os.WriteFile(secret, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"nosuch", "..%2fsecret", "build/all"} {
		rec := getLog(t, h, "/submissions/"+itoa(sub.ID)+"/logs/"+name)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET log %q: status %d, want 404 (body %q)", name, rec.Code, rec.Body.String())
		}
	}
}

// TestSubmissionLogDownloadIsTeacherOnly: student code runs under the same UID
// as the hidden tests copied into its workspace, so it can read them and print
// them - and the raw full log would hand that straight back to its author.
// SPEC §14 already draws the line: students see the log their tests produced,
// teachers see it whole.
func TestSubmissionLogDownloadIsTeacherOnly(t *testing.T) {
	h, _ := newTestSite(t)
	h.DataDir = t.TempDir()
	student, sub := finishedWithChecks(t, h, "build")

	h.Local = &student
	if rec := getLog(t, h, "/submissions/"+itoa(sub.ID)+"/logs/build"); rec.Code != http.StatusNotFound {
		t.Errorf("owner downloaded the full log: status %d, body %q", rec.Code, rec.Body.String())
	}

	teacher, err := h.DB.GetUserByLogin(t.Context(), "local")
	if err != nil {
		t.Fatal(err)
	}
	h.Local = &teacher
	rec := getLog(t, h, "/submissions/"+itoa(sub.ID)+"/logs/build")
	if rec.Code != http.StatusOK || rec.Body.String() != "log of build" {
		t.Errorf("teacher: status %d, body %q, want 200 and the log", rec.Code, rec.Body.String())
	}
}

// TestSubmissionPageHidesLogLinkFromStudent: the page must not offer a download
// it will 404, and must say where the full log went.
func TestSubmissionPageHidesLogLinkFromStudent(t *testing.T) {
	h, _ := newTestSite(t)
	h.DataDir = t.TempDir()
	student, sub := finishedWithChecks(t, h, "build")
	h.Local = &student

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/submissions/"+itoa(sub.ID), nil))
	body := rec.Body.String()
	if strings.Contains(body, "/logs/") {
		t.Errorf("student page still links to the full log:\n%s", body)
	}
	if !strings.Contains(body, "full logs are available to teachers") {
		t.Errorf("student page does not explain the missing download:\n%s", body)
	}

	teacher, err := h.DB.GetUserByLogin(t.Context(), "local")
	if err != nil {
		t.Fatal(err)
	}
	h.Local = &teacher
	rec = httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/submissions/"+itoa(sub.ID), nil))
	if !strings.Contains(rec.Body.String(), "/logs/build") {
		t.Errorf("teacher page lost the log download:\n%s", rec.Body.String())
	}
}
