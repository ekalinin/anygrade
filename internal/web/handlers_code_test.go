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
