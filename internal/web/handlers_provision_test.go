package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/intake"
)

// recordingProvisioner stands in for the RepoManager-backed EnsureRepo closure.
type recordingProvisioner struct {
	logins []string
	err    error
}

func (p *recordingProvisioner) ensure(_ context.Context, login string) error {
	p.logins = append(p.logins, login)
	return p.err
}

func postForm(t *testing.T, h *Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)
	return rec
}

// openModeSite switches the test course to open registration.
func openModeSite(t *testing.T) (*Handler, *recordingProvisioner) {
	t.Helper()
	h, _ := newTestSite(t)
	holder := &intake.Holder{}
	holder.Set(&intake.Course{Resolved: &config.Resolved{Course: config.ResolvedCourse{
		Name:         "Test course",
		Registration: config.Registration{Mode: "open", CourseCode: "go-2026"},
	}}})
	h.Course = holder
	p := &recordingProvisioner{}
	h.EnsureRepo = p.ensure
	return h, p
}

// invitedSite seeds a pending student plus a live invite token for them.
func invitedSite(t *testing.T) (*Handler, *recordingProvisioner, string) {
	t.Helper()
	h, _ := newTestSite(t)
	p := &recordingProvisioner{}
	h.EnsureRepo = p.ensure
	student, err := h.DB.CreateUser(t.Context(), "alice", "Alice", "student")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const token = "invite-token"
	if err := h.DB.CreateInvite(t.Context(), student.ID, token, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	return h, p, token
}

// TestActivationProvisionsRepo: SPEC §7 puts repo creation at account
// activation, so the clone command printed on the very next page must already
// work rather than provisioning on the student's first git access.
func TestActivationProvisionsRepo(t *testing.T) {
	h, p, token := invitedSite(t)

	rec := postForm(t, h, "/invite/"+token, url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("activation: status %d, want 200", rec.Code)
	}
	if len(p.logins) != 1 || p.logins[0] != "alice" {
		t.Errorf("provisioned %v, want [alice]", p.logins)
	}
}

// TestOpenRegistrationProvisionsRepo: the same for self-registration, which is
// the other way an account becomes usable.
func TestOpenRegistrationProvisionsRepo(t *testing.T) {
	h, p := openModeSite(t)

	rec := postForm(t, h, "/register", url.Values{
		"login": {"bob"}, "name": {"Bob"}, "course_code": {"go-2026"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("registration: status %d, want 200", rec.Code)
	}
	if len(p.logins) != 1 || p.logins[0] != "bob" {
		t.Errorf("provisioned %v, want [bob]", p.logins)
	}
}

// TestRegistrationRejectedDoesNotProvision: a wrong course code must not
// create a repo for a login that never got an account.
func TestRegistrationRejectedDoesNotProvision(t *testing.T) {
	h, p := openModeSite(t)

	rec := postForm(t, h, "/register", url.Values{
		"login": {"mallory"}, "name": {"M"}, "course_code": {"wrong"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("registration: status %d, want 422", rec.Code)
	}
	if len(p.logins) != 0 {
		t.Errorf("provisioned %v on a rejected registration", p.logins)
	}
}

// TestActivationSurvivesProvisioningFailure: the account is activated and its
// token is shown exactly once, so a failure to clone the repo must not abort
// the flow - the git transports still provision on first access.
func TestActivationSurvivesProvisioningFailure(t *testing.T) {
	h, p, token := invitedSite(t)
	p.err = errors.New("disk full")

	rec := postForm(t, h, "/invite/"+token, url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("activation: status %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ag_") {
		t.Error("activation should still show the personal token")
	}
	// The invite is burned, i.e. the activation really completed.
	if _, ok, err := h.DB.VerifyInvite(t.Context(), token); err == nil && ok {
		t.Error("invite still valid: activation did not complete")
	}
	if _, err := h.DB.GetUserByLogin(t.Context(), "alice"); err != nil {
		t.Errorf("activated user missing: %v", err)
	}
}

// TestActivationWithoutProvisionerWorks: EnsureRepo is optional (unit tests,
// and any caller that leaves it nil), and its zero value must not panic.
func TestActivationWithoutProvisionerWorks(t *testing.T) {
	h, _, token := invitedSite(t)
	h.EnsureRepo = nil

	if rec := postForm(t, h, "/invite/"+token, url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("activation: status %d, want 200", rec.Code)
	}
	if _, err := h.DB.GetUserByLogin(t.Context(), "alice"); err != nil {
		t.Errorf("activated user missing: %v", err)
	}
}
