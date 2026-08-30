package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/ratelimit"
)

func TestGitURLs(t *testing.T) {
	h := &Handler{BaseURL: "http://grade.example.org:8080/", SSHAddr: ":2222"}
	u := h.gitURLs("alice")
	if u.Clone != "http://grade.example.org:8080/git/alice/course.git" {
		t.Errorf("clone: %s", u.Clone)
	}
	if u.Upstream != "http://grade.example.org:8080/git/course.git" {
		t.Errorf("upstream: %s", u.Upstream)
	}
	if u.SSHClone != "ssh://git@grade.example.org:2222/alice/course.git" {
		t.Errorf("ssh clone: %s", u.SSHClone)
	}
	if u.SSHUpstream != "ssh://git@grade.example.org:2222/course.git" {
		t.Errorf("ssh upstream: %s", u.SSHUpstream)
	}
}

func TestGitURLsNoSSH(t *testing.T) {
	h := &Handler{BaseURL: "http://localhost:8080"}
	if u := h.gitURLs("alice"); u.SSHClone != "" || u.SSHUpstream != "" {
		t.Errorf("ssh urls must be empty without an ssh addr: %+v", u)
	}
}

// TestGitURLsSSHHostFallback: an unparseable base URL still yields a usable
// localhost SSH hint rather than none.
func TestGitURLsSSHHostFallback(t *testing.T) {
	h := &Handler{BaseURL: "", SSHAddr: "0.0.0.0:2222"}
	if got := h.gitURLs("bob").SSHClone; got != "ssh://git@localhost:2222/bob/course.git" {
		t.Errorf("ssh clone: %s", got)
	}
}

// openCourse puts the site in open-registration mode behind a course code.
func openCourse(h *Handler, code string) {
	boundedCourse(h, config.Registration{Mode: "open", CourseCode: code})
}

// boundedCourse is openCourse with an enrolment window and/or an account cap.
func boundedCourse(h *Handler, reg config.Registration) {
	h.Course.Set(&intake.Course{Resolved: &config.Resolved{
		Course: config.ResolvedCourse{Name: "Test course", Registration: reg},
	}})
}

// stamp is the config.Timestamp of a moment relative to now, so a window test
// never depends on the wall clock being any particular date.
func stamp(d time.Duration) *config.Timestamp {
	t := config.Timestamp(time.Now().Add(d))
	return &t
}

func postRegister(h *Handler, login, code string) *httptest.ResponseRecorder {
	form := url.Values{"login": {login}, "name": {"Student"}, "course_code": {code}}
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)
	return rec
}

// TestRegisterCourseCodeIsRateLimited: the course code is a short shared
// secret, and the registration form compared it without ever consulting the
// limiter that already guards the login form - so it could be brute-forced at
// full speed. The budget is per client IP: the submitted login is the
// attacker's to vary.
func TestRegisterCourseCodeIsRateLimited(t *testing.T) {
	const max = 3
	h, _ := newTestSite(t)
	openCourse(h, "s3cret")
	h.Limit = ratelimit.New(max, time.Minute)

	for i := range max {
		// A different login every time: the old key would hand each attempt a
		// budget of its own.
		if rec := postRegister(h, "guess"+itoa(int64(i)), "wrong"); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("attempt %d: status %d, want 422", i, rec.Code)
		}
	}
	rec := postRegister(h, "guess-again", "wrong")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 once the budget is spent", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "too many failed attempts") {
		t.Errorf("no throttling message on the page:\n%s", rec.Body.String())
	}
	// The right code is still refused while the budget is spent.
	if rec := postRegister(h, "bob", "s3cret"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("status %d, want 429: throttling must not be bypassable by guessing right", rec.Code)
	}
}

// TestRegisterSuccessClearsTheBudget: a correct code is not a failed attempt,
// so it must not leave the next student throttled.
func TestRegisterSuccessClearsTheBudget(t *testing.T) {
	h, _ := newTestSite(t)
	openCourse(h, "s3cret")
	h.Limit = ratelimit.New(3, time.Minute)

	if rec := postRegister(h, "alice", "wrong"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", rec.Code)
	}
	if rec := postRegister(h, "alice", "s3cret"); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if rec := postRegister(h, "bob", "s3cret"); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: a success must clear the budget", rec.Code)
	}
}

// mustNotExist asserts that a refused registration created nothing.
func mustNotExist(t *testing.T, h *Handler, login string) {
	t.Helper()
	if u, err := h.DB.GetUserByLogin(t.Context(), login); err == nil {
		t.Fatalf("refused registration still created %q (id %d)", login, u.ID)
	}
}

// TestRegisterEnrolmentWindow: the course code is public to everybody who can
// clone the repo, so `opens`/`closes` bound how long it is worth anything
// (SPEC §8). Before, during and after the window.
func TestRegisterEnrolmentWindow(t *testing.T) {
	tests := []struct {
		name   string
		reg    config.Registration
		status int
	}{
		{"before it opens", config.Registration{Opens: stamp(time.Hour)}, http.StatusUnprocessableEntity},
		{"inside", config.Registration{Opens: stamp(-time.Hour), Closes: stamp(time.Hour)}, http.StatusOK},
		{"after it closes", config.Registration{Closes: stamp(-time.Hour)}, http.StatusUnprocessableEntity},
		{"unbounded", config.Registration{}, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestSite(t)
			tc.reg.Mode, tc.reg.CourseCode = "open", "s3cret"
			boundedCourse(h, tc.reg)

			rec := postRegister(h, "alice", "s3cret")
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status != http.StatusOK {
				if !strings.Contains(rec.Body.String(), "registration is closed") {
					t.Errorf("no closed-window message on the page:\n%s", rec.Body.String())
				}
				// Retrying cannot turn this into a success, and every retry
				// spends the shared per-IP budget, so the form goes away.
				if strings.Contains(rec.Body.String(), `name="course_code"`) {
					t.Errorf("the form is offered again after a closed-window refusal:\n%s", rec.Body.String())
				}
				mustNotExist(t, h, "alice")
			}
		})
	}
}

// TestRegisterWindowIsCheckedBeforeTheCourseCode: outside the window the reply
// must not depend on whether the code was right, or a closed course would be a
// free oracle for guessing it.
func TestRegisterWindowIsCheckedBeforeTheCourseCode(t *testing.T) {
	h, _ := newTestSite(t)
	boundedCourse(h, config.Registration{Mode: "open", CourseCode: "s3cret", Closes: stamp(-time.Hour)})
	h.Limit = ratelimit.New(10, time.Minute)

	right := postRegister(h, "alice", "s3cret")
	wrong := postRegister(h, "alice", "nope")
	if right.Code != http.StatusUnprocessableEntity || wrong.Code != right.Code {
		t.Fatalf("status right=%d wrong=%d, want both 422", right.Code, wrong.Code)
	}
	if right.Body.String() != wrong.Body.String() {
		t.Errorf("a right code is distinguishable from a wrong one outside the window:\n%s\n---\n%s",
			right.Body.String(), wrong.Body.String())
	}
	mustNotExist(t, h, "alice")
}

// TestRegisterCap: `max_accounts` bounds how many accounts self-registration
// may ever create. Under, at, and over the cap.
func TestRegisterCap(t *testing.T) {
	h, _ := newTestSite(t)
	boundedCourse(h, config.Registration{Mode: "open", CourseCode: "s3cret", MaxAccounts: 2})

	for _, login := range []string{"alice", "bob"} {
		if rec := postRegister(h, login, "s3cret"); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200 under the cap (body %q)", login, rec.Code, rec.Body.String())
		}
	}
	rec := postRegister(h, "carol", "s3cret")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422 over the cap", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "registration is closed") {
		t.Errorf("no cap message on the page:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `name="course_code"`) {
		t.Errorf("the form is offered again over the cap:\n%s", rec.Body.String())
	}
	mustNotExist(t, h, "carol")
}

// TestRegisterCapCountsOnlySelfRegistrations: the cap exists to bound what
// open mode gives away, so it counts `user.register` events - the accounts the
// form created. A teacher's own roster arrives by invite, which logs no such
// event, and must not eat the students' places.
func TestRegisterCapCountsOnlySelfRegistrations(t *testing.T) {
	h, _ := newTestSite(t)
	boundedCourse(h, config.Registration{Mode: "open", CourseCode: "s3cret", MaxAccounts: 1})
	// newTestSite already created the teacher; add an invited student too.
	if _, err := h.DB.CreateUser(t.Context(), "invited", "Invited", "student"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if rec := postRegister(h, "alice", "s3cret"); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: invited accounts must not consume the cap (body %q)", rec.Code, rec.Body.String())
	}
	if rec := postRegister(h, "bob", "s3cret"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422 once the cap is spent", rec.Code)
	}
}

// TestRegisterClosedChargesTheFailureBudget: a refusal by the window or the
// cap is not a wrong credential, but it must not be a free retry either -
// otherwise a shut course answers an unbounded poll waiting for it to reopen.
func TestRegisterClosedChargesTheFailureBudget(t *testing.T) {
	const max = 3
	h, _ := newTestSite(t)
	boundedCourse(h, config.Registration{Mode: "open", CourseCode: "s3cret", Closes: stamp(-time.Hour)})
	h.Limit = ratelimit.New(max, time.Minute)

	for i := range max {
		if rec := postRegister(h, "alice", "s3cret"); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("attempt %d: status %d, want 422", i, rec.Code)
		}
	}
	if rec := postRegister(h, "alice", "s3cret"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 once the budget is spent", rec.Code)
	}
}

// TestRegisterPageHidesTheFormWhenClosed: a student who bookmarked /register
// gets told the enrolment window is over rather than a form that can only fail.
func TestRegisterPageHidesTheFormWhenClosed(t *testing.T) {
	h, _ := newTestSite(t)
	boundedCourse(h, config.Registration{Mode: "open", CourseCode: "s3cret", Closes: stamp(-time.Hour)})

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/register", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /register: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "registration is closed") {
		t.Errorf("GET /register: no closed message:\n%s", body)
	}
	if strings.Contains(body, `name="course_code"`) {
		t.Errorf("GET /register: the form is still offered while closed:\n%s", body)
	}

	// The same page inside the window keeps the form.
	boundedCourse(h, config.Registration{Mode: "open", CourseCode: "s3cret", Closes: stamp(time.Hour)})
	rec = httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/register", nil))
	if !strings.Contains(rec.Body.String(), `name="course_code"`) {
		t.Errorf("GET /register: no form inside the window:\n%s", rec.Body.String())
	}
}
