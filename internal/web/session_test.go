package web

import (
	"net/http"
	"net/http/httptest"
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
