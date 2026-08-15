package web

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/ekalinin/anygrade/internal/ident"
	"github.com/ekalinin/anygrade/internal/store"
)

type inviteData struct {
	CourseName string
	User       userView // always zero on public pages
	Token      string
	Login      string
	Invalid    bool
	Error      string
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

// inviteSubmit activates the account: issues the personal token (shown
// once), stores an optional SSH key, burns the invite, and logs the browser
// in (SPEC §8).
func (h *Handler) inviteSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		h.httpError(w, r, "error.cross_origin", http.StatusForbidden)
		return
	}
	inv, target, ok := h.resolveInvite(r)
	if !ok {
		h.renderPage(w, r, "invite", inviteData{
			CourseName: h.Course.Get().Resolved.Course.Name, Invalid: true,
		})
		return
	}
	if keyText := strings.TrimSpace(r.FormValue("key")); keyText != "" {
		pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(keyText))
		if err != nil {
			h.renderPage(w, r, "invite", inviteData{
				CourseName: h.Course.Get().Resolved.Course.Name,
				Token:      r.PathValue("token"), Login: target.Login,
				Error: "unparseable_ssh_key",
			})
			return
		}
		if _, err := h.DB.AddSSHKey(r.Context(), target.ID, gossh.FingerprintSHA256(pk), keyText); err != nil {
			// Duplicate key: not worth failing the activation over.
			_ = err
		}
	}
	token, err := h.DB.IssueToken(r.Context(), target.ID)
	if err != nil {
		h.httpError(w, r, "error.activation_failed", http.StatusInternalServerError)
		return
	}
	_ = h.DB.MarkInviteUsed(r.Context(), inv.ID, time.Now())
	_ = h.DB.Log(r.Context(), store.Event{
		ActorID: &target.ID, Kind: "user.activate", Target: target.Login,
	})
	if sid, serr := h.DB.CreateSession(r.Context(), target.ID, token, sessionTTL); serr == nil {
		setSessionCookie(w, r, sid, sessionTTL)
	}
	h.renderTokenOnce(w, r, target.Login, token, true)
}

type registerData struct {
	CourseName string
	User       userView
	Login      string
	Name       string
	Error      string
}

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
	h.renderPage(w, r, "register", registerData{CourseName: h.Course.Get().Resolved.Course.Name})
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
	fail := func(msg string) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		h.renderPage(w, r, "register", registerData{
			CourseName: course.Resolved.Course.Name,
			Login:      login, Name: name, Error: msg,
		})
	}
	if r.FormValue("course_code") != course.Resolved.Course.Registration.CourseCode {
		fail("wrong_course_code")
		return
	}
	if !ident.ValidLogin(login) {
		fail("invalid_login")
		return
	}
	target, err := h.DB.CreateUser(r.Context(), login, name, "student")
	if err != nil {
		fail("login_taken")
		return
	}
	token, err := h.DB.IssueToken(r.Context(), target.ID)
	if err != nil {
		h.httpError(w, r, "error.registration_failed", http.StatusInternalServerError)
		return
	}
	_ = h.DB.Log(r.Context(), store.Event{
		ActorID: &target.ID, Kind: "user.register", Target: target.Login,
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
