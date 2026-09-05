package web

import (
	"context"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/i18n"
	"github.com/ekalinin/anygrade/internal/intake"
)

// TestCodeDownloadEncodesFileName: the name comes out of the student's commit,
// so it is arbitrary by definition. Hand-building the header let a quote in it
// break out and hand the teacher a download under some other name entirely.
func TestCodeDownloadEncodesFileName(t *testing.T) {
	const hostile = `solution.go"; filename="grades.csv`
	h, teacher := newTestSite(t)
	h.Local = &teacher
	h.ListStudentFiles = func(context.Context, string, string, string) ([]string, error) {
		return []string{hostile}, nil
	}
	h.ReadStudentFile = func(context.Context, string, string, string) ([]byte, bool, error) {
		return []byte("package main\n"), true, nil
	}
	student, _ := newSession(t, h, "bob", "student")
	sub := enqueue(t, h, student.ID, "t1", time.Now())

	// url.URL renders the path the way html/template does in an href.
	ref := &url.URL{
		Path:     "/students/bob/submissions/" + itoa(sub) + "/code/" + hostile,
		RawQuery: "download=1",
	}
	req := httptest.NewRequest(http.MethodGet, ref.String(), nil)
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	cd := rec.Header().Get("Content-Disposition")
	disp, params, err := mime.ParseMediaType(cd)
	if err != nil {
		t.Fatalf("Content-Disposition %q does not parse: %v", cd, err)
	}
	if disp != "attachment" {
		t.Errorf("disposition = %q, want attachment", disp)
	}
	if params["filename"] != hostile {
		t.Errorf("filename = %q, want the whole name %q (header injection)", params["filename"], hostile)
	}
}

// TestCodeFileTooLargeIsNotAMissingFile: the reader refuses a blob it will not
// buffer. Reporting that as 404 would send the teacher hunting for a bug in the
// listing that just showed them the file, so the page says what is wrong and
// the download is refused rather than served empty.
func TestCodeFileTooLargeIsNotAMissingFile(t *testing.T) {
	const path = "big.bin"
	h, teacher := newTestSite(t)
	h.Local = &teacher
	h.ListStudentFiles = func(context.Context, string, string, string) ([]string, error) {
		return []string{path}, nil
	}
	h.ReadStudentFile = func(context.Context, string, string, string) ([]byte, bool, error) {
		return nil, true, ErrFileTooLarge
	}
	student, _ := newSession(t, h, "bob", "student")
	sub := enqueue(t, h, student.ID, "t1", time.Now())
	base := "/students/bob/submissions/" + itoa(sub) + "/code/" + path

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("page status %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "too large") {
		t.Errorf("the page does not explain the size: %q", body)
	}

	rec = httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"?download=1", nil))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("download status %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if rec.Body.Len() == 0 {
		t.Error("the refused download says nothing")
	}
}

// codeSite builds a teacher site over one task whose only solution file is
// main.go, serving `student` as the submitted blob and `course` as the
// authoritative one (empty = absent at the course head). It returns the site
// and the URL of the file's page.
func codeSite(t *testing.T, path, student, course string) (*Handler, string) {
	t.Helper()
	h, teacher := newTestSite(t)
	h.Local = &teacher
	h.Course.Set(&intake.Course{Resolved: &config.Resolved{
		Course: config.ResolvedCourse{Name: "Test course"},
		Tasks: []config.ResolvedTask{
			{ID: "t1", Name: "Task one", Score: 10, SolutionFiles: []string{"main.go"}},
		},
	}})
	h.ListStudentFiles = func(context.Context, string, string, string) ([]string, error) {
		return []string{path}, nil
	}
	h.ReadStudentFile = func(context.Context, string, string, string) ([]byte, bool, error) {
		return []byte(student), true, nil
	}
	h.ReadCourseFile = func(context.Context, string, string) ([]byte, bool, error) {
		return []byte(course), course != "", nil
	}
	bob, _ := newSession(t, h, "bob", "student")
	sub := enqueue(t, h, bob.ID, "t1", time.Now())
	return h, "/students/bob/submissions/" + itoa(sub) + "/code/" + path
}

// TestCodeViewDiffsAgainstTheCourseVersion: the job the page exists for is
// "what did the student change", so a solution file opens on the delta against
// the template and the whole file is one click away.
func TestCodeViewDiffsAgainstTheCourseVersion(t *testing.T) {
	h, base := codeSite(t, "main.go",
		"package main\n\nfunc solve() int { return 42 }\n",
		"package main\n\nfunc solve() int { return 0 }\n")

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("diff view: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<pre class="diff">`,
		`<span class="dl del">`,
		`<span class="dl add">`,
		`href="?view=full"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("diff view lacks %s:\n%s", want, body)
		}
	}

	rec = httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"?view=full", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("full view: status %d, want 200", rec.Code)
	}
	body = rec.Body.String()
	if strings.Contains(body, `<pre class="diff">`) {
		t.Errorf("?view=full still rendered the diff:\n%s", body)
	}
	if !strings.Contains(body, `href="?view=diff"`) {
		t.Errorf("?view=full lost the way back to the diff:\n%s", body)
	}
	// Highlighted, so the number is its own span: the full view is the whole
	// file, not the raw file.
	if !strings.Contains(body, `<span class="tok-num">42</span>`) {
		t.Errorf("?view=full does not show the highlighted file:\n%s", body)
	}
}

// TestCodeViewWithoutACourseCounterpartStaysPlain: a file the student added and
// a solution file missing upstream both have nothing to diff against, so the
// page keeps the plain view and offers no toggle at all.
func TestCodeViewWithoutACourseCounterpartStaysPlain(t *testing.T) {
	cases := []struct{ name, path, course string }{
		{"not a solution file", "notes.txt", "whatever\n"},
		{"absent at the course head", "main.go", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, base := codeSite(t, tc.path, "mine\n", tc.course)

			rec := httptest.NewRecorder()
			New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if strings.Contains(body, "view=diff") || strings.Contains(body, "view=full") {
				t.Errorf("the toggle is offered without a counterpart:\n%s", body)
			}
			if !strings.Contains(body, "mine") {
				t.Errorf("the plain view does not show the file:\n%s", body)
			}
		})
	}
}

// TestCodeViewNeverRendersStudentMarkup is the handler-level half of
// TestHighlightNeverEmitsMarkupFromTheFile: whatever the student committed
// reaches the teacher's browser as text on both views, never as markup.
func TestCodeViewNeverRendersStudentMarkup(t *testing.T) {
	const payload = `<script>alert("pwn")</script>`
	h, base := codeSite(t, "main.go", "package main // "+payload+"\n", "package main\n")

	for _, target := range []string{base, base + "?view=full"} {
		rec := httptest.NewRecorder()
		New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", target, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, payload) || strings.Contains(body, "<script>alert") {
			t.Errorf("%s: the student's markup reached the page verbatim:\n%s", target, body)
		}
		if !strings.Contains(body, "&lt;script&gt;") {
			t.Errorf("%s: the payload is not on the page at all:\n%s", target, body)
		}
	}
}

// TestCodeViewSaysWhenTheFileIsUnchanged: an untouched solution file has an
// empty diff, and a page of unchanged lines would not say so.
func TestCodeViewSaysWhenTheFileIsUnchanged(t *testing.T) {
	const same = "package main\n"
	h, base := codeSite(t, "main.go", same, same)

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if want := i18n.For(i18n.Default).T("code.diff_none"); !strings.Contains(body, want) {
		t.Errorf("the page does not say the file is unchanged (%q):\n%s", want, body)
	}
	if !strings.Contains(body, `href="?view=full"`) {
		t.Errorf("the toggle is missing on an unchanged file:\n%s", body)
	}
}
