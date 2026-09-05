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

	"github.com/ekalinin/anygrade/internal/i18n"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/runner"
	"github.com/ekalinin/anygrade/internal/store"
)

// finishedWithChecks seeds one finished submission whose results carry the
// given check names, and writes each check's log file where the runner puts it.
func finishedWithChecks(t *testing.T, h *Handler, names ...string) (store.User, store.Submission) {
	t.Helper()
	rows := make([]store.CheckRow, len(names))
	for i, n := range names {
		rows[i] = store.CheckRow{Name: n, Passed: true, Weight: 1}
	}
	return finishedWithRows(t, h, rows...)
}

// finishedWithRows is finishedWithChecks over explicit result rows. A row that
// failed in its build phase gets no run-phase log, exactly as the runner leaves
// it: that check never reached the phase students can read.
func finishedWithRows(t *testing.T, h *Handler, rows ...store.CheckRow) (store.User, store.Submission) {
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

	if err := h.DB.FinishSubmission(t.Context(), sub.ID, store.SubmissionResult{
		Status: store.StatusDone, Checks: rows,
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	dir := intake.SubmissionLogDir(h.DataDir, sub.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range rows {
		if c.BuildFailed {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, runner.LogFileName(c.Name)), []byte("log of "+c.Name), 0o644); err != nil {
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

// infraRow seeds one submission in the state a worker leaves behind when no
// check ever ran: the operator note the teacher reads, the student-safe
// projection its owner reads, and retryAt nil for "retries exhausted" or set
// while a retry is still armed.
func infraRow(t *testing.T, h *Handler, login string, retryAt *time.Time,
	note, studentNote string) (store.User, store.Submission) {

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
	if _, ok, err := h.DB.ClaimNext(t.Context(), time.Now()); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if ok, err := h.DB.ScheduleRetry(t.Context(), sub.ID, retryAt, note, studentNote); err != nil || !ok {
		t.Fatalf("schedule retry: ok=%v err=%v", ok, err)
	}
	return student, sub
}

// pageBody renders one path as whoever the handler currently logs in.
func pageBody(t *testing.T, h *Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, rec.Code)
	}
	return rec.Body.String()
}

// TestSubmissionPageExplainsItselfWithoutChecks: a submission that recorded no
// check results has nothing but its note to explain itself - a teacher cancel,
// a terminal prepare failure, an unreachable hidden-tests overlay - and SPEC
// §14 makes the scrubbed hidden-tests message the one such failure a student is
// meant to read. The note has to reach the student in both shapes the queue
// leaves the row in: terminal, and still waiting on a retry, where the bare
// "waiting for a worker" would otherwise hold for the whole backoff.
func TestSubmissionPageExplainsItselfWithoutChecks(t *testing.T) {
	const note = "hidden tests temporarily unavailable"
	at := time.Now().Add(time.Minute)
	for _, c := range []struct {
		name    string
		retryAt *time.Time
	}{
		{"terminal", nil},
		{"retrying", &at},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestSite(t)
			h.DataDir = t.TempDir()
			student, sub := infraRow(t, h, "bob", c.retryAt, note, note)
			h.Local = &student

			body := pageBody(t, h, "/submissions/"+itoa(sub.ID))
			if !strings.Contains(body, note) {
				t.Errorf("student page does not carry the note:\n%s", body)
			}
			// The note is the explanation, so neither status hint may stand in
			// for it: one would say there is nothing to show, the other that a
			// worker is still on its way.
			for _, key := range []string{"sub.no_results", "sub.waiting"} {
				if hint := i18n.For("en").T(key); strings.Contains(body, hint) {
					t.Errorf("page still shows %q next to the note:\n%s", key, body)
				}
			}
		})
	}
}

// TestSubmissionPageKeepsOperatorNoteFromStudents: the worker note is not
// student-safe on every path. A docker failure names the image and quotes the
// daemon, a prepare failure quotes a path inside the data dir; that detail is
// the teacher's (SPEC §14), and the student is left with "no results", which is
// exactly what the row has for them.
func TestSubmissionPageKeepsOperatorNoteFromStudents(t *testing.T) {
	const note = "infra error (image_pull): docker pull golang:1.26: exit status 1: " +
		"Cannot connect to the Docker daemon at unix:///var/run/docker.sock"
	h, _ := newTestSite(t)
	h.DataDir = t.TempDir()
	student, sub := infraRow(t, h, "bob", nil, note, "")

	h.Local = &student
	body := pageBody(t, h, "/submissions/"+itoa(sub.ID))
	for _, leak := range []string{"image_pull", "docker.sock", "golang:1.26"} {
		if strings.Contains(body, leak) {
			t.Errorf("student page leaks %q:\n%s", leak, body)
		}
	}
	if want := i18n.For("en").T("sub.no_results"); !strings.Contains(body, want) {
		t.Errorf("student page says nothing at all:\n%s", body)
	}

	teacher, err := h.DB.GetUserByLogin(t.Context(), "local")
	if err != nil {
		t.Fatal(err)
	}
	h.Local = &teacher
	if body := pageBody(t, h, "/submissions/"+itoa(sub.ID)); !strings.Contains(body, note) {
		t.Errorf("teacher page lost the operator note:\n%s", body)
	}
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

// TestSubmissionLogViewIsTeacherOnly: `?view=1` is the same file past the same
// role check with one header less, so it inherits the download's gate
// unchanged (SPEC §14) - for the run phase, and for the build phase, whose log
// no student may read in any form.
func TestSubmissionLogViewIsTeacherOnly(t *testing.T) {
	const buildOut = "hidden_test.go:7: undefined: Solve"
	h, _ := newTestSite(t)
	h.DataDir = t.TempDir()
	student, sub := finishedWithChecks(t, h, "vet")
	// The same check with a build phase behind it: two files, one route.
	dir := runner.BuildLogDir(intake.SubmissionLogDir(h.DataDir, sub.ID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runner.LogFileName("vet")), []byte(buildOut), 0o600); err != nil {
		t.Fatal(err)
	}

	base := "/submissions/" + itoa(sub.ID) + "/logs/vet"
	phases := []struct{ name, path, want string }{
		{"run", base + "?view=1", "log of vet"},
		{"build", base + "?phase=build&view=1", buildOut},
	}

	h.Local = &student
	for _, p := range phases {
		if rec := getLog(t, h, p.path); rec.Code != http.StatusNotFound {
			t.Errorf("student viewed the %s log: status %d, body %q", p.name, rec.Code, rec.Body.String())
		}
	}

	teacher, err := h.DB.GetUserByLogin(t.Context(), "local")
	if err != nil {
		t.Fatal(err)
	}
	h.Local = &teacher
	for _, p := range phases {
		rec := getLog(t, h, p.path)
		if rec.Code != http.StatusOK || rec.Body.String() != p.want {
			t.Errorf("teacher, %s phase: status %d, body %q, want 200 and %q",
				p.name, rec.Code, rec.Body.String(), p.want)
			continue
		}
		// Read, not saved - and the declared type has to bind on its own: the
		// disposition that used to stop a browser treating student-written
		// bytes as a page is exactly what the viewer drops.
		if cd := rec.Header().Get("Content-Disposition"); cd != "" {
			t.Errorf("teacher, %s phase: viewer still forces a download: %q", p.name, cd)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("teacher, %s phase: Content-Type %q, want text/plain", p.name, ct)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("teacher, %s phase: X-Content-Type-Options %q, want nosniff", p.name, got)
		}
	}
	// The download is untouched: `?view=1` is the only thing that drops the
	// attachment.
	if cd := getLog(t, h, base).Header().Get("Content-Disposition"); cd == "" {
		t.Errorf("the plain download lost its attachment disposition")
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
	if !strings.Contains(body, i18n.For("en").T("sub.logs_teacher_only")) {
		t.Errorf("student page does not explain the missing download:\n%s", body)
	}

	teacher, err := h.DB.GetUserByLogin(t.Context(), "local")
	if err != nil {
		t.Fatal(err)
	}
	h.Local = &teacher
	rec = httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/submissions/"+itoa(sub.ID), nil))
	// Both ways into the same file: the saved copy and the one read in place.
	for _, want := range []string{`/logs/build"`, "/logs/build?view=1"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("teacher page lost %q:\n%s", want, rec.Body.String())
		}
	}
}

// buildFailedSubmission seeds one finished submission whose single check
// failed in its build phase, with the build log where the runner puts it: in
// the build subdirectory, which nothing student-facing ever reads.
func buildFailedSubmission(t *testing.T, h *Handler, output string) (store.User, store.Submission) {
	t.Helper()
	student, sub := finishedWithRows(t, h,
		store.CheckRow{Name: "compiled", Weight: 1, ExitCode: 2, BuildFailed: true})
	dir := runner.BuildLogDir(intake.SubmissionLogDir(h.DataDir, sub.ID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runner.LogFileName("compiled")), []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	return student, sub
}

// TestBuildLogDownloadIsTeacherOnly: the build phase is the one that compiles
// against the hidden tests, so a compiler quoting a hidden source line lands in
// its log. It is a separate file from the run phase's, and only a teacher may
// read it (SPEC §14).
func TestBuildLogDownloadIsTeacherOnly(t *testing.T) {
	const secret = "hidden_test.go:7: undefined: Solve"
	h, _ := newTestSite(t)
	h.DataDir = t.TempDir()
	student, sub := buildFailedSubmission(t, h, secret)

	h.Local = &student
	rec := getLog(t, h, "/submissions/"+itoa(sub.ID)+"/logs/compiled?phase=build")
	if rec.Code != http.StatusNotFound {
		t.Errorf("student read the build log: status %d, body %q", rec.Code, rec.Body.String())
	}

	teacher, err := h.DB.GetUserByLogin(t.Context(), "local")
	if err != nil {
		t.Fatal(err)
	}
	h.Local = &teacher
	rec = getLog(t, h, "/submissions/"+itoa(sub.ID)+"/logs/compiled?phase=build")
	if rec.Code != http.StatusOK || rec.Body.String() != secret {
		t.Fatalf("teacher: status %d, body %q, want 200 and the build log", rec.Code, rec.Body.String())
	}
	// The two phases are two downloads, not one file under two names.
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "build-compiled.log") {
		t.Errorf("build log offered as %q, want a name distinct from the run log", cd)
	}
	// The run phase never happened, so its log does not exist - and asking for
	// it must not fall through to the build one.
	if rec := getLog(t, h, "/submissions/"+itoa(sub.ID)+"/logs/compiled"); rec.Code == http.StatusOK &&
		strings.Contains(rec.Body.String(), secret) {
		t.Errorf("the run-phase download served the build log: %q", rec.Body.String())
	}
}

// TestBuildFailurePageExplainsItself: with no excerpt to show - by design, the
// output is teacher-only - the page has to say why, in the reader's language,
// and offer the teacher the log it withheld.
func TestBuildFailurePageExplainsItself(t *testing.T) {
	h, _ := newTestSite(t)
	h.DataDir = t.TempDir()
	student, sub := buildFailedSubmission(t, h, "secret compiler output")

	page := func(lang string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/submissions/"+itoa(sub.ID), nil)
		req.AddCookie(&http.Cookie{Name: langCookie, Value: lang})
		rec := httptest.NewRecorder()
		New(h).ServeHTTP(rec, req)
		return rec.Body.String()
	}

	h.Local = &student
	for _, lang := range []string{"en", "ru"} {
		body := page(lang)
		if want := i18n.For(lang).T("sub.build_failed"); !strings.Contains(body, want) {
			t.Errorf("student page [%s] does not explain the build failure:\n%s", lang, body)
		}
		if strings.Contains(body, "secret compiler output") || strings.Contains(body, "phase=build") {
			t.Errorf("student page [%s] leaks the build phase:\n%s", lang, body)
		}
	}

	teacher, err := h.DB.GetUserByLogin(t.Context(), "local")
	if err != nil {
		t.Fatal(err)
	}
	h.Local = &teacher
	body := page("en")
	// The build phase is offered both ways too - the viewer is the reason to
	// open a build log at all, since no excerpt of it is stored anywhere.
	for _, want := range []string{`phase=build"`, "phase=build&amp;view=1"} {
		if !strings.Contains(body, want) {
			t.Errorf("teacher page lost the build log link %q:\n%s", want, body)
		}
	}
	// The run phase never happened, so its download would only 404.
	if strings.Contains(body, `/logs/compiled"`) {
		t.Errorf("teacher page offers a run log that does not exist:\n%s", body)
	}
}

// TestSubmissionPageListsTestCases: a check that declared a `parser:` shows
// the cases its report carried and the tally its score was a fraction of. The
// names come out of the student's own test run, so the page has to render them
// as text - a case called "<img onerror=...>" is a name, not markup.
func TestSubmissionPageListsTestCases(t *testing.T) {
	h, _ := newTestSite(t)
	h.DataDir = t.TempDir()
	student, sub := finishedWithRows(t, h, store.CheckRow{
		Name: "unit", Weight: 100, ExitCode: 1, Cases: store.CaseRows{
			{Name: "TestAdd", Status: "passed"},
			{Name: `<img src=x onerror="alert(1)">`, Status: "failed", Message: "want <b>2</b>, got 1"},
			{Name: "TestNet", Status: "skipped", Message: "needs network"},
		},
	})
	h.Local = &student

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/submissions/"+itoa(sub.ID), nil))
	body := rec.Body.String()

	if !strings.Contains(body, "TestAdd") || !strings.Contains(body, "needs network") {
		t.Errorf("the case list is missing from the page:\n%s", body)
	}
	// One passed of two scored: the skip counts for neither side.
	if !strings.Contains(body, "1/2") {
		t.Errorf("the tally the score was made of is missing:\n%s", body)
	}
	if strings.Contains(body, "<img src=x") || strings.Contains(body, "<b>2</b>") {
		t.Errorf("student-controlled text reached the page as markup:\n%s", body)
	}
	if !strings.Contains(body, "&lt;img src=x") {
		t.Errorf("the case name is not rendered at all:\n%s", body)
	}
}

// A parser that read nothing says so, in the reader's language, and leaves the
// check scored by its exit code - the excerpt stays the fallback.
func TestSubmissionPageExplainsAnUnreadableReport(t *testing.T) {
	h, _ := newTestSite(t)
	h.DataDir = t.TempDir()
	student, sub := finishedWithRows(t, h, store.CheckRow{
		Name: "unit", Weight: 100, ExitCode: 1, ParseFailed: true, LogExcerpt: "not a report",
	})
	h.Local = &student

	for _, lang := range []string{"en", "ru"} {
		req := httptest.NewRequest(http.MethodGet, "/submissions/"+itoa(sub.ID), nil)
		req.AddCookie(&http.Cookie{Name: langCookie, Value: lang})
		rec := httptest.NewRecorder()
		New(h).ServeHTTP(rec, req)
		body := rec.Body.String()
		if want := i18n.For(lang).T("sub.parse_failed"); !strings.Contains(body, want) {
			t.Errorf("page [%s] does not explain the unreadable report:\n%s", lang, body)
		}
		if !strings.Contains(body, "not a report") {
			t.Errorf("page [%s] dropped the excerpt that stands in for the cases:\n%s", lang, body)
		}
	}
}
