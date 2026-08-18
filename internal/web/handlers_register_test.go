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
	h.Course.Set(&intake.Course{Resolved: &config.Resolved{
		Course: config.ResolvedCourse{
			Name:         "Test course",
			Registration: config.Registration{Mode: "open", CourseCode: code},
		},
	}})
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
