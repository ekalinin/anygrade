package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/i18n"
	"github.com/ekalinin/anygrade/internal/intake"
)

// inLocale sends one request with the UI language cookie set.
func inLocale(t *testing.T, h *Handler, lang string, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req.AddCookie(&http.Cookie{Name: langCookie, Value: lang})
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)
	return rec
}

// TestHTTPErrorsAreLocalized: the plain-text error bodies are UI strings, so
// they follow the reader's locale like every other rendered string (SPEC
// §10.1). Both routes here are public - no session needed to reach them.
func TestHTTPErrorsAreLocalized(t *testing.T) {
	h, _ := newTestSite(t)

	cases := []struct {
		name string
		req  func() *http.Request
		code int
		key  string
	}{
		{
			name: "unsupported language",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/lang",
					strings.NewReader(url.Values{"lang": {"de"}}.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				return r
			},
			code: http.StatusBadRequest,
			key:  "error.unsupported_language",
		},
		{
			name: "cross-origin",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/lang", nil)
				r.Header.Set("Origin", "http://evil.example.org")
				return r
			},
			code: http.StatusForbidden,
			key:  "error.cross_origin",
		},
	}

	for _, tc := range cases {
		for _, lang := range i18n.Locales() {
			rec := inLocale(t, h, lang, tc.req())
			if rec.Code != tc.code {
				t.Errorf("%s [%s]: status %d, want %d", tc.name, lang, rec.Code, tc.code)
				continue
			}
			want := i18n.For(lang).T(tc.key)
			if got := strings.TrimSpace(rec.Body.String()); got != want {
				t.Errorf("%s [%s]: body %q, want %q", tc.name, lang, got, want)
			}
		}
		// The point of the change: the two locales must not read alike.
		if i18n.For("en").T(tc.key) == i18n.For("ru").T(tc.key) {
			t.Errorf("%s: %q is identical in en and ru - the catalog is not translated", tc.name, tc.key)
		}
	}
}

// TestMissingReadmePlaceholderIsLocalized: the "no README.md" placeholder on a
// task page was a hard-coded English literal, unlike everything around it.
func TestMissingReadmePlaceholderIsLocalized(t *testing.T) {
	h, teacher := newTestSite(t)
	h.Local = &teacher
	holder := &intake.Holder{}
	holder.Set(&intake.Course{Resolved: &config.Resolved{
		Course: config.ResolvedCourse{Name: "Test course"},
		Tasks:  []config.ResolvedTask{{ID: "t1", Name: "Task one", Score: 100}},
	}})
	h.Course = holder
	// No README at this commit: the placeholder path.
	h.ReadCourseFile = func(context.Context, string, string) ([]byte, bool, error) {
		return nil, false, nil
	}

	for _, lang := range i18n.Locales() {
		rec := inLocale(t, h, lang, httptest.NewRequest(http.MethodGet, "/tasks/t1", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /tasks/t1 [%s]: status %d", lang, rec.Code)
		}
		if want := i18n.For(lang).T("task.no_readme"); !strings.Contains(rec.Body.String(), want) {
			t.Errorf("task page [%s]: missing placeholder %q", lang, want)
		}
	}
}
