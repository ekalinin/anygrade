package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/runner"
	"github.com/ekalinin/anygrade/internal/store"
)

// The two rights of the role table (store/roles.go), spelled the way the
// middleware names them - once for the pages, once for the JSON API, which
// authenticates differently and asks the same predicate.
const (
	rightReview    = "requireReview"
	rightAdmin     = "requireTeacher"
	rightAPI       = "requireAPI"
	rightAPIReview = "requireAPIReview"
)

// staffRoute is one gated route: the pattern exactly as web.go registers it,
// the same URL with its wildcards filled in, and the right it must demand.
type staffRoute struct {
	pattern string
	url     string
	right   string
}

// method is the verb the pattern already carries ("POST /queue/{id}/cancel").
func (r staffRoute) method() string { return strings.Fields(r.pattern)[0] }

// staffRoutes is the authorization contract. Every gated route is here with
// the right it needs, and TestRouteTableMatchesTheMux fails if the mux and
// this table ever disagree - so a route added without a gate, or gated
// differently, shows up as a test that was never extended.
var staffRoutes = []staffRoute{
	{"GET /matrix", "/matrix", rightReview},
	{"GET /matrix/stream", "/matrix/stream", rightReview},
	{"GET /export/scores.csv", "/export/scores.csv", rightReview},
	{"GET /queue", "/queue", rightReview},
	{"GET /queue/stream", "/queue/stream", rightReview},
	{"POST /queue/{id}/cancel", "/queue/1/cancel", rightReview},
	{"POST /queue/{id}/recheck", "/queue/1/recheck", rightReview},
	{"GET /students", "/students", rightReview},
	{"GET /students/{login}", "/students/alice", rightReview},
	{"POST /students/{login}/tasks/{id}/recheck", "/students/alice/tasks/t1/recheck", rightReview},
	{"GET /students/{login}/submissions/{id}/code", "/students/alice/submissions/1/code", rightReview},
	{"GET /students/{login}/submissions/{id}/code/{path...}", "/students/alice/submissions/1/code/main.go", rightReview},

	{"POST /students/{login}/tasks/{id}/override", "/students/alice/tasks/t1/override", rightAdmin},
	{"POST /students/{login}/tasks/{id}/override/delete", "/students/alice/tasks/t1/override/delete", rightAdmin},
	{"POST /students/{login}/token/reset", "/students/alice/token/reset", rightAdmin},
	{"POST /students/{login}/state", "/students/alice/state", rightAdmin},
	{"POST /students/{login}/keys/{id}/delete", "/students/alice/keys/1/delete?fingerprint=SHA256:none", rightAdmin},
	{"GET /audit", "/audit", rightAdmin},
}

// apiRoutes is the same contract for the JSON API (SPEC §10.2). It is a
// separate list because the assertions are: an API route is driven by a bearer
// token, not by a session, so TestStaffRoutesByRole cannot reach it and
// TestAPIRoleEndpointMatrix owns the role table for these. What this list is
// for is the scan below - the API must not be a second place where a route can
// appear ungated.
var apiRoutes = []staffRoute{
	{"GET /api/v1/me", "/api/v1/me", rightAPI},
	{"GET /api/v1/tasks", "/api/v1/tasks", rightAPI},
	{"GET /api/v1/submissions/{id}", "/api/v1/submissions/1", rightAPI},

	{"GET /api/v1/matrix", "/api/v1/matrix", rightAPIReview},
	{"GET /api/v1/queue", "/api/v1/queue", rightAPIReview},
}

// ungatedRoutes are the rest of the mux: public pages and the ones every
// authenticated account reaches. They are listed only so that the scan below
// can account for every registration, which is what makes a new route
// impossible to add without deciding which of the four lists it joins.
var ungatedRoutes = []string{
	"GET /login", "POST /login",
	"GET /invite/{token}", "POST /invite/{token}",
	"GET /register", "POST /register",
	"POST /lang", "GET /static/",
	"POST /logout", "GET /{$}", "GET /dashboard/stream",
	"GET /tasks/{id}", "POST /tasks/{id}/recheck",
	"GET /submissions/{id}", "GET /submissions/{id}/fragment",
	"GET /submissions/{id}/stream", "GET /submissions/{id}/logs/{check...}",
	"GET /leaderboard", "GET /settings", "POST /settings/token",
	"POST /settings/keys", "POST /settings/keys/verify",
	"POST /settings/keys/{id}/delete",
}

// muxRoute reads one registration out of web.go: the pattern, and whatever the
// handler expression starts with.
var muxRoute = regexp.MustCompile(`mux\.Handle(?:Func)?\("([^"]+)",\s*(\S+)`)

// TestRouteTableMatchesTheMux keeps the table above honest against the source
// it describes. The mux offers no way to enumerate its patterns, so this reads
// the registrations back out of web.go: a new route, a moved gate, or a
// deleted one all land here first.
func TestRouteTableMatchesTheMux(t *testing.T) {
	src, err := os.ReadFile("web.go")
	if err != nil {
		t.Fatal(err)
	}
	gated := slices.Concat(staffRoutes, apiRoutes)
	want := map[string]string{}
	for _, r := range gated {
		want[r.pattern] = r.right
	}

	seen := map[string]bool{}
	for _, m := range muxRoute.FindAllStringSubmatch(string(src), -1) {
		pattern, handler := m[1], m[2]
		seen[pattern] = true
		right := ""
		// The trailing paren is what keeps these apart: "h.requireAPIReview("
		// does not start with "h.requireAPI(".
		for _, r := range []string{rightReview, rightAdmin, rightAPIReview, rightAPI} {
			if strings.HasPrefix(handler, "h."+r+"(") {
				right = r
				break
			}
		}
		switch {
		case right == "" && slices.Contains(ungatedRoutes, pattern):
		case right == "":
			t.Errorf("route %q has no rights check and is not listed as ungated: "+
				"add it to staffRoutes, apiRoutes or ungatedRoutes", pattern)
		case want[pattern] != right:
			t.Errorf("route %q is gated by %s, the table says %q", pattern, right, want[pattern])
		}
	}
	for _, r := range gated {
		if !seen[r.pattern] {
			t.Errorf("the route table lists %q, which web.go no longer registers", r.pattern)
		}
	}
}

// TestStaffRoutesByRole is the split itself: what each of the three roles gets
// on every gated route. A TA reaches the reviewing half and is refused the
// account-management half with 404, exactly like a student - never a 403,
// which would confirm the route exists (SPEC §14).
func TestStaffRoutesByRole(t *testing.T) {
	for _, role := range []string{store.RoleStudent, store.RoleTA, store.RoleTeacher} {
		t.Run(role, func(t *testing.T) {
			h, session := newRoleSite(t, role)
			for _, r := range staffRoutes {
				allowed := (role == store.RoleTeacher) ||
					(role == store.RoleTA && r.right == rightReview)
				code := drive(t, h, r.method(), r.url, session)
				switch {
				case allowed && code == http.StatusNotFound:
					t.Errorf("%s: %s is refused (404)", role, r.pattern)
				case !allowed && code != http.StatusNotFound:
					t.Errorf("%s: %s answered %d, want 404 - a refusal must not "+
						"tell the caller the route exists", role, r.pattern, code)
				}
			}
		})
	}
}

// TestNavByRole: the top bar offers exactly the pages the role may open. It is
// drawn from the same two rights the routes are gated by, so a nav link can
// never lead to a 404.
func TestNavByRole(t *testing.T) {
	for _, c := range []struct {
		role   string
		review bool
		admin  bool
	}{
		{store.RoleStudent, false, false},
		{store.RoleTA, true, false},
		{store.RoleTeacher, true, true},
	} {
		t.Run(c.role, func(t *testing.T) {
			h, session := newRoleSite(t, c.role)
			body := do(h, http.MethodGet, "/", session).Body.String()
			for _, link := range []string{`href="/matrix"`, `href="/queue"`, `href="/students"`} {
				if strings.Contains(body, link) != c.review {
					t.Errorf("%s: nav has %s = %v, want %v", c.role, link,
						strings.Contains(body, link), c.review)
				}
			}
			if got := strings.Contains(body, `href="/audit"`); got != c.admin {
				t.Errorf("%s: nav has the audit link = %v, want %v", c.role, got, c.admin)
			}
		})
	}
}

// TestStudentPageHidesAdminControlsFromTA: the page a TA and a teacher share
// must not offer the TA a button that 404s. The reviewing controls stay.
func TestStudentPageHidesAdminControlsFromTA(t *testing.T) {
	h, taSession := newRoleSite(t, store.RoleTA)
	_, teacherSession := newSession(t, h, "prof", store.RoleTeacher)

	admin := []string{
		`action="/students/alice/token/reset"`,
		`action="/students/alice/state"`,
		`action="/students/alice/tasks/t1/override"`,
		`/keys/1/delete`,
	}
	const review = `action="/students/alice/tasks/t1/recheck"`

	ta := do(h, http.MethodGet, "/students/alice", taSession).Body.String()
	for _, form := range admin {
		if strings.Contains(ta, form) {
			t.Errorf("the TA's student page still offers %s", form)
		}
	}
	if !strings.Contains(ta, review) {
		t.Errorf("the TA's student page lost the recheck button:\n%s", ta)
	}

	teacher := do(h, http.MethodGet, "/students/alice", teacherSession).Body.String()
	for _, form := range append(admin, review) {
		if !strings.Contains(teacher, form) {
			t.Errorf("the teacher's student page lost %s", form)
		}
	}
}

// TestCheckLogsByRole: the full run log and the build log by role. The build
// phase compiles against the hidden tests (SPEC §14), and the TA is exactly
// the person who needs the compiler's words while helping a student debug -
// so a reviewer reads both phases, and the student neither.
func TestCheckLogsByRole(t *testing.T) {
	const buildOut = "hidden_test.go:7: undefined: Solve"
	h, _ := newTestSite(t)
	h.DataDir = t.TempDir()
	owner, sub := finishedWithChecks(t, h, "vet")
	dir := runner.BuildLogDir(intake.SubmissionLogDir(h.DataDir, sub.ID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runner.LogFileName("vet")), []byte(buildOut), 0o600); err != nil {
		t.Fatal(err)
	}

	base := "/submissions/" + itoa(sub.ID) + "/logs/vet"
	phases := []struct{ name, path, want string }{
		{"run", base, "log of vet"},
		{"build", base + "?phase=build", buildOut},
	}
	// The owner is one of the roles under test: a student may not read the
	// full log even of their own submission.
	for _, role := range []string{store.RoleStudent, store.RoleTA, store.RoleTeacher} {
		t.Run(role, func(t *testing.T) {
			viewer := owner
			if role != store.RoleStudent {
				viewer, _ = newSession(t, h, "staff-"+role, role)
			}
			h.Local = &viewer
			for _, p := range phases {
				rec := getLog(t, h, p.path)
				if role == store.RoleStudent {
					if rec.Code != http.StatusNotFound {
						t.Errorf("student read the %s log: status %d, body %q",
							p.name, rec.Code, rec.Body.String())
					}
					continue
				}
				if rec.Code != http.StatusOK || rec.Body.String() != p.want {
					t.Errorf("%s, %s phase: status %d, body %q, want 200 and %q",
						role, p.name, rec.Code, rec.Body.String(), p.want)
				}
			}
		})
	}
}

// TestLeaderboardNamesByRole: anonymization exists so students cannot rank each
// other, not to keep staff from their job - and a TA who may open every
// submission would only be sent to the matrix for the same names. Both halves
// of the switch are checked: with anonymize off nobody sees an alias.
func TestLeaderboardNamesByRole(t *testing.T) {
	for _, anonymize := range []bool{true, false} {
		for _, role := range []string{store.RoleStudent, store.RoleTA, store.RoleTeacher} {
			name := role + "/plain"
			if anonymize {
				name = role + "/anonymized"
			}
			t.Run(name, func(t *testing.T) {
				h, _ := newTestSite(t)
				h.Course.Set(&intake.Course{Resolved: &config.Resolved{
					Course: config.ResolvedCourse{
						Name:        "Test course",
						Leaderboard: config.Leaderboard{Enabled: true, Anonymize: anonymize},
					},
				}})
				newSession(t, h, "alice", store.RoleStudent)
				_, session := newSession(t, h, "viewer", role)

				body := do(h, http.MethodGet, "/leaderboard", session).Body.String()
				hidden := anonymize && role == store.RoleStudent
				if got := strings.Contains(body, ">alice<"); got == hidden {
					t.Errorf("%s sees the login %v on an anonymize=%v board:\n%s",
						role, got, anonymize, body)
				}
				if got := strings.Contains(body, h.Alias.Alias("alice")); got != hidden {
					t.Errorf("%s sees the alias %v on an anonymize=%v board:\n%s",
						role, got, anonymize, body)
				}
			})
		}
	}
}

// TestAuditRecordsActorRole: an action carries the role its actor held, so a
// review can tell a TA's recheck from a teacher's. Rows written before the
// column existed stay empty rather than being backfilled with a guess.
func TestAuditRecordsActorRole(t *testing.T) {
	h, _ := newRoleSite(t, store.RoleTeacher)
	for _, role := range []string{store.RoleTA, store.RoleTeacher} {
		_, session := newSession(t, h, "staff-"+role, role)
		if code := drive(t, h, http.MethodPost, "/queue/1/cancel", session); code != http.StatusSeeOther {
			t.Fatalf("%s cancel: status %d, want 303", role, code)
		}
	}
	events, err := h.DB.ListEvents(t.Context(), "submission.cancel", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("%d cancel events, want 2", len(events))
	}
	// ListEvents is newest first, so the teacher's cancel leads.
	for i, want := range []string{store.RoleTeacher, store.RoleTA} {
		if events[i].ActorRole != want {
			t.Errorf("event by %s: actor role %q, want %q",
				events[i].ActorLogin, events[i].ActorRole, want)
		}
	}
}

// newRoleSite builds a site whose gated routes all have something to address:
// one student with a submission and a task table, plus the seams the handlers
// reach through. The returned cookie is a session for an account of the given
// role, so requests go through the real auth path.
func newRoleSite(t *testing.T, role string) (*Handler, *http.Cookie) {
	t.Helper()
	h, _ := newTestSite(t)
	h.DataDir = t.TempDir()
	setCourse(h)
	h.Recheck = &fakeRechecker{sub: store.Submission{ID: 1}}
	h.Cancel = fakeCanceler{}
	h.ListStudentFiles = func(context.Context, string, string, string) ([]string, error) {
		return []string{"main.go"}, nil
	}
	h.ReadStudentFile = func(context.Context, string, string, string) ([]byte, bool, error) {
		return []byte("package main\n"), true, nil
	}

	alice, _ := newSession(t, h, "alice", store.RoleStudent)
	enqueue(t, h, alice.ID, "t1", time.Now())
	// A key to delete: the admin route 404s on a fingerprint that matches
	// nothing, which would hide a rights failure behind a missing object.
	if _, err := h.DB.AddSSHKey(t.Context(), alice.ID, "SHA256:none", "ssh-ed25519 AAAA test"); err != nil {
		t.Fatalf("seed ssh key: %v", err)
	}

	login := "viewer"
	if role == store.RoleStudent {
		login = "bob" // a student actor of their own, not the object under review
	}
	_, session := newSession(t, h, login, role)
	return h, session
}

// fakeCanceler stands in for the queue: the cancel route only needs a verdict.
type fakeCanceler struct{}

func (fakeCanceler) Cancel(context.Context, int64) (bool, error) { return true, nil }

// drive issues one request as the session owner and reports the status code.
// The request context is canceled the moment the handler starts responding:
// an SSE route otherwise blocks until its client goes away, and the rights
// check is long decided by then.
func drive(t *testing.T, h *Handler, method, target string, c *http.Cookie) int {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stop := time.AfterFunc(5*time.Second, cancel) // a hang must fail, not wedge the suite
	defer stop.Stop()

	req := httptest.NewRequest(method, target, nil).WithContext(ctx)
	req.AddCookie(c)
	p := &rolesProbe{hdr: http.Header{}, cancel: cancel}
	New(h).ServeHTTP(p, req)
	return p.status()
}

// rolesProbe is a streaming ResponseWriter that records the status line and
// hangs up as soon as the handler reaches for a header - which every path,
// refusal or not, does before it can write anything.
type rolesProbe struct {
	mu     sync.Mutex
	hdr    http.Header
	code   int
	cancel context.CancelFunc
	once   sync.Once
}

func (p *rolesProbe) Header() http.Header {
	p.once.Do(func() { p.cancel() })
	return p.hdr
}

func (p *rolesProbe) WriteHeader(code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code == 0 {
		p.code = code
	}
}

func (p *rolesProbe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code == 0 {
		p.code = http.StatusOK
	}
	return len(b), nil
}

func (p *rolesProbe) Flush() {}

// status reports what net/http would have sent: a handler that returns without
// writing sends 200.
func (p *rolesProbe) status() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code == 0 {
		return http.StatusOK
	}
	return p.code
}
