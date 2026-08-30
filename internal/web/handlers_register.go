package web

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ekalinin/anygrade/internal/ident"
	"github.com/ekalinin/anygrade/internal/ratelimit"
	"github.com/ekalinin/anygrade/internal/store"
)

type inviteData struct {
	CourseName string
	User       userView // always zero on public pages
	Token      string
	Login      string
	Invalid    bool
}

// gitURLs are the SPEC §7 suggested setup, shown on activation. The SSH
// fields are empty when the SSH transport is not configured.
type gitURLs struct {
	Clone       string
	Upstream    string
	SSHClone    string
	SSHUpstream string
}

func (h *Handler) gitURLs(login string) gitURLs {
	base := strings.TrimSuffix(h.BaseURL, "/")
	u := gitURLs{
		Clone:    base + "/git/" + login + "/course.git",
		Upstream: base + "/git/course.git",
	}
	if ssh := h.sshBase(); ssh != "" {
		u.SSHClone = ssh + "/" + login + "/course.git"
		u.SSHUpstream = ssh + "/course.git"
	}
	return u
}

// sshBase derives "ssh://git@<host>:<port>" from the web base URL's host and
// the SSH listen address (the SSH server binds a port, not a public name).
func (h *Handler) sshBase() string {
	if h.SSHAddr == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(h.SSHAddr)
	if err != nil || port == "" {
		return ""
	}
	host := "localhost"
	if u, err := url.Parse(h.BaseURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return "ssh://git@" + net.JoinHostPort(host, port)
}

// resolveInvite maps the URL token to its pending user; every failure mode
// renders the same neutral "invalid" page.
func (h *Handler) resolveInvite(r *http.Request) (store.Invite, store.User, bool) {
	inv, ok, err := h.DB.VerifyInvite(r.Context(), r.PathValue("token"))
	if err != nil || !ok {
		return store.Invite{}, store.User{}, false
	}
	target, err := h.DB.GetUserByID(r.Context(), inv.UserID)
	if err != nil || target.State != "active" {
		return store.Invite{}, store.User{}, false
	}
	return inv, target, true
}

func (h *Handler) invitePage(w http.ResponseWriter, r *http.Request) {
	data := inviteData{CourseName: h.Course.Get().Resolved.Course.Name, Token: r.PathValue("token")}
	_, target, ok := h.resolveInvite(r)
	if !ok {
		data.Invalid = true
		h.renderPage(w, r, "invite", data)
		return
	}
	data.Login = target.Login
	h.renderPage(w, r, "invite", data)
}

// inviteSubmit activates the account: burns the one-shot invite, issues the
// personal token (shown once), and logs the browser in (SPEC §8).
//
// It used to take an optional SSH key too. It no longer does, because a key
// accepted here could not be proven: the invite proves possession of the
// invite, not of the private half, so this page would have stayed the one path
// on which a student could register a classmate's public key and lock them out
// of it. Folding a sign-and-paste round trip into activation is worse than
// moving it - the link is one-shot, so a student who fumbles the signature on
// this page has no second attempt at activating at all. The student lands
// logged in on the very next page, where settings is one click away.
func (h *Handler) inviteSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		h.httpError(w, r, "error.cross_origin", http.StatusForbidden)
		return
	}
	invalid := func() {
		h.renderPage(w, r, "invite", inviteData{
			CourseName: h.Course.Get().Resolved.Course.Name, Invalid: true,
		})
	}
	inv, target, ok := h.resolveInvite(r)
	if !ok {
		invalid()
		return
	}
	// Burn the invite before any side effect. VerifyInvite only proves the
	// link was unused when it was read, so two concurrent activations of one
	// link would both rotate the token - and the second rotation invalidates
	// the token the first student was just shown (SPEC §8: the link is
	// one-shot).
	if used, err := h.DB.ConsumeInvite(r.Context(), inv.ID, time.Now()); err != nil || !used {
		invalid()
		return
	}
	token, err := h.DB.IssueToken(r.Context(), target.ID)
	if err != nil {
		h.httpError(w, r, "error.activation_failed", http.StatusInternalServerError)
		return
	}
	h.ensureRepo(r.Context(), target.Login)
	_ = h.DB.Log(r.Context(), store.Event{
		ActorID: &target.ID, Kind: "user.activate", Target: target.Login,
	})
	if sid, serr := h.DB.CreateSession(r.Context(), target.ID, token, sessionTTL); serr == nil {
		setSessionCookie(w, r, sid, sessionTTL)
	}
	h.renderTokenOnce(w, r, target.Login, token, true)
}

// ensureRepo provisions the personal repo as part of activation, so the clone
// command shown on the very next page already works (SPEC §7).
//
// A failure is deliberately not surfaced: the account is activated and its
// token is about to be shown once, and there is no way back from here. The git
// transports create the repo on first access anyway, so the cost of a failure
// is one slow first clone, not a broken account. The injected implementation
// logs it.
func (h *Handler) ensureRepo(ctx context.Context, login string) {
	if h.EnsureRepo == nil {
		return
	}
	_ = h.EnsureRepo(ctx, login)
}

type registerData struct {
	CourseName string
	User       userView
	Login      string
	Name       string
	Error      string
	// Closed hides the form: enrolment is over, or the course has no places
	// left, so the only thing the form could do is fail (SPEC §8).
	Closed bool
}

// registerLimitKey is the fixed login half of the open-registration failure
// key, so the budget is per client IP rather than per submitted login.
const registerLimitKey = "\x00register"

// registerEventKind is the audit kind logged for every self-registration, and
// therefore the counter `registration.max_accounts` is compared against: it
// counts exactly the accounts this form created, while the teacher's own
// roster - created by invite, which logs `user.activate` - never consumes a
// student's place (SPEC §8).
const registerEventKind = "user.register"

func (h *Handler) openMode(w http.ResponseWriter, r *http.Request) bool {
	if h.Course.Get().Resolved.Course.Registration.Mode != "open" {
		http.NotFound(w, r)
		return false
	}
	return true
}

func (h *Handler) registerPage(w http.ResponseWriter, r *http.Request) {
	if !h.openMode(w, r) {
		return
	}
	course := h.Course.Get().Resolved.Course
	data := registerData{CourseName: course.Name}
	// The window is a clock comparison, so saying so here is free. The account
	// cap deliberately is not checked: it needs a COUNT, and this page is
	// public and unthrottled - a full course still shows the form and is
	// refused on submit, where the failure budget bounds the cost.
	if !course.Registration.OpenAt(time.Now()) {
		data.Closed, data.Error = true, "registration_closed"
	}
	h.renderPage(w, r, "register", data)
}

// registerSubmit is open-mode self-registration, gated by the course code
// (SPEC §8).
func (h *Handler) registerSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.openMode(w, r) {
		return
	}
	if !sameOrigin(r) {
		h.httpError(w, r, "error.cross_origin", http.StatusForbidden)
		return
	}
	course := h.Course.Get()
	login := strings.TrimSpace(r.FormValue("login"))
	name := strings.TrimSpace(r.FormValue("name"))
	// closed hides the form as well as reporting the error: a refusal by the
	// enrolment window or the account cap cannot be retried into a success,
	// and every retry spends the shared per-IP budget (SPEC §8).
	fail := func(msg string, closed bool) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		h.renderPage(w, r, "register", registerData{
			CourseName: course.Resolved.Course.Name,
			Login:      login, Name: name, Error: msg, Closed: closed,
		})
	}
	// The course code is a short shared secret, so this is a credential check
	// and belongs on the same failure budget as the login form. The key's login
	// half is a constant: the attacker picks the submitted login freely, and a
	// budget per (IP, attacker-chosen login) would be no budget at all.
	rv, allowed := h.Limit.Reserve(ratelimit.AuthKey(h.clientAddr(r), registerLimitKey))
	if !allowed {
		w.WriteHeader(http.StatusTooManyRequests)
		h.renderPage(w, r, "register", registerData{
			CourseName: course.Resolved.Course.Name,
			Login:      login, Name: name, Error: "too_many_attempts",
		})
		return
	}
	defer rv.Release() // no-op once the outcome below is reported
	// The enrolment window and the account cap are decided before the code is
	// compared, so a refusal outside them never depends on whether the code
	// was right: the code lives in the repo every student clones, and this is
	// what keeps a leaked one worthless once enrolment is over (SPEC §8).
	//
	// Both charge the failure budget. A rejection here is not a wrong
	// credential, but a free retry would leave a shut course answering an
	// unbounded poll - waiting for the window to open or for the teacher to
	// raise the cap, then racing in - and each poll costs a COUNT over the
	// audit log. Nothing legitimate is lost: while registration is shut no
	// attempt can succeed anyway, so the only cost of a spent budget is that
	// the page says "too many attempts" instead of "closed".
	reg := course.Resolved.Course.Registration
	if !reg.OpenAt(time.Now()) {
		rv.Fail()
		fail("registration_closed", true)
		return
	}
	// The count and the CreateUser below are not one transaction, so a burst
	// of simultaneous valid registrations can overshoot the cap by the number
	// in flight. That is deliberate: this is an abuse bound, not a licence
	// count, and making it exact would mean holding one write transaction
	// across the whole handler, repo provisioning included. A read error
	// counts as "no room" - a cap that fails open is not a cap.
	if reg.MaxAccounts > 0 {
		used, err := h.DB.CountEventsByKind(r.Context(), registerEventKind)
		if err != nil || used >= reg.MaxAccounts {
			rv.Fail()
			fail("registration_full", true)
			return
		}
	}
	if r.FormValue("course_code") != reg.CourseCode {
		rv.Fail()
		fail("wrong_course_code", false)
		return
	}
	rv.Success()
	if !ident.ValidLogin(login) {
		fail("invalid_login", false)
		return
	}
	target, err := h.DB.CreateUser(r.Context(), login, name, "student")
	if err != nil {
		fail("login_taken", false)
		return
	}
	token, err := h.DB.IssueToken(r.Context(), target.ID)
	if err != nil {
		h.httpError(w, r, "error.registration_failed", http.StatusInternalServerError)
		return
	}
	h.ensureRepo(r.Context(), target.Login)
	_ = h.DB.Log(r.Context(), store.Event{
		ActorID: &target.ID, Kind: registerEventKind, Target: target.Login,
	})
	if sid, serr := h.DB.CreateSession(r.Context(), target.ID, token, sessionTTL); serr == nil {
		setSessionCookie(w, r, sid, sessionTTL)
	}
	h.renderTokenOnce(w, r, target.Login, token, true)
}

type tokenOnceData struct {
	CourseName string
	User       userView
	Login      string
	Token      string
	// WithGitSetup shows the SPEC §7 clone/upstream instructions (activation
	// and registration; a plain reset skips them).
	WithGitSetup bool
	URLs         gitURLs
}

// renderTokenOnce is the shared one-time token display (activation,
// registration, self-service regen, teacher reset).
func (h *Handler) renderTokenOnce(w http.ResponseWriter, r *http.Request, login, token string, withGit bool) {
	u, _ := h.currentUser(r)
	h.renderPage(w, r, "token_once", tokenOnceData{
		CourseName:   h.Course.Get().Resolved.Course.Name,
		User:         h.userViewOf(u),
		Login:        login,
		Token:        token,
		WithGitSetup: withGit,
		URLs:         h.gitURLs(login),
	})
}
