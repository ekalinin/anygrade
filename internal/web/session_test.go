package web

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/store"
)

// newTestSite builds the site over a real (empty) store plus the implicit
// local account `serve --local` would create.
func newTestSite(t *testing.T) (*Handler, store.User) {
	t.Helper()
	db, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	u, err := db.CreateUser(t.Context(), "local", "Local User", "teacher")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	holder := &intake.Holder{}
	holder.Set(&intake.Course{Resolved: &config.Resolved{
		Course: config.ResolvedCourse{Name: "Test course"},
	}})
	return &Handler{DB: db, Course: holder, Hub: NewHub()}, u
}

// TestLocalModeServesDashboardWithoutSession: with Handler.Local set, an
// unauthenticated GET / renders the implicit user's dashboard (SPEC §8).
func TestLocalModeServesDashboardWithoutSession(t *testing.T) {
	h, u := newTestSite(t)
	h.Local = &u

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Local User") {
		t.Errorf("GET /: body does not name the implicit user:\n%s", body)
	}
	// The implicit user is a teacher, so the teacher nav is reachable.
	if !strings.Contains(body, `href="/matrix"`) {
		t.Errorf("GET /: body has no teacher nav:\n%s", body)
	}
	if strings.Contains(body, `action="/logout"`) {
		t.Errorf("GET /: logout form must be hidden in local mode:\n%s", body)
	}
}

// TestLocalModeRedirectsLoginForm: there is nothing to sign in to, so the
// login form is not shown in local mode.
func TestLocalModeRedirectsLoginForm(t *testing.T) {
	h, u := newTestSite(t)
	h.Local = &u

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("GET /login: status %d, Location %q, want 302 to /",
			rec.Code, rec.Header().Get("Location"))
	}
}

// TestWithoutLocalModeDashboardNeedsSession: the default zero value keeps the
// session gate - the bypass is unreachable unless the composition root sets it.
func TestWithoutLocalModeDashboardNeedsSession(t *testing.T) {
	h, _ := newTestSite(t)

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("GET /: status %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("GET /: Location %q, want a /login redirect", loc)
	}
}

// TestSessionCookieSecureFlag: the session cookie is a bearer credential, so it
// is marked Secure whenever the browser's connection actually is encrypted -
// and X-Forwarded-Proto only counts when the operator vouched for the proxy,
// because anyone reaching the port can forge that header.
func TestSessionCookieSecureFlag(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tls         bool
		forwarded   string
		behindProxy bool
		want        bool
	}{
		{name: "plain http"},
		{name: "direct tls", tls: true, want: true},
		{name: "forged header without the opt-in", forwarded: "https"},
		{name: "trusted proxy", forwarded: "https", behindProxy: true, want: true},
		{name: "trusted proxy on plain http", forwarded: "http", behindProxy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestSite(t)
			h.BehindProxy = tc.behindProxy
			u, err := h.DB.CreateUser(t.Context(), "bob", "Student", "student")
			if err != nil {
				t.Fatal(err)
			}
			token, err := h.DB.IssueToken(t.Context(), u.ID)
			if err != nil {
				t.Fatal(err)
			}

			form := url.Values{"login": {"bob"}, "token": {token}}
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-Proto", tc.forwarded)
			}
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			rec := httptest.NewRecorder()
			New(h).ServeHTTP(rec, req)

			if rec.Code != http.StatusFound {
				t.Fatalf("POST /login: status %d, want 302 (body %q)", rec.Code, rec.Body.String())
			}
			cookies := (&http.Response{Header: rec.Header()}).Cookies()
			if len(cookies) == 0 {
				t.Fatal("no session cookie was set")
			}
			if got := cookies[0].Secure; got != tc.want {
				t.Errorf("Secure = %v, want %v (Set-Cookie: %q)",
					got, tc.want, rec.Header().Get("Set-Cookie"))
			}
		})
	}
}
