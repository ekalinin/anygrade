package web

import (
	"net/http"
	"strings"

	"github.com/ekalinin/anygrade/internal/ratelimit"
)

type loginData struct {
	CourseName string
	User       userView // always zero: base template needs the field
	Next       string
	Error      string
}

func (h *Handler) loginData(r *http.Request, errMsg string) loginData {
	next := r.FormValue("next")
	// Only same-site relative targets: never an open redirect.
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	return loginData{
		CourseName: h.Course.Get().Resolved.Course.Name,
		Next:       next,
		Error:      errMsg,
	}
}

func (h *Handler) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.currentUser(r); ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	h.renderPage(w, r, "login", h.loginData(r, ""))
}

func (h *Handler) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		h.httpError(w, r, "error.cross_origin", http.StatusForbidden)
		return
	}
	login := strings.TrimSpace(r.FormValue("login"))
	token := strings.TrimSpace(r.FormValue("token"))
	// Reserve, not Blocked: the budget slot is taken before the token is
	// compared, so a simultaneous burst cannot all pass the check while the
	// first failure is still being recorded.
	rv, allowed := h.Limit.Reserve(ratelimit.AuthKey(h.clientAddr(r), login))
	if !allowed {
		w.WriteHeader(http.StatusTooManyRequests)
		h.renderPage(w, r, "login", h.loginData(r, "too_many_attempts"))
		return
	}
	defer rv.Release() // no-op once the outcome below is reported
	u, ok, err := h.DB.VerifyToken(r.Context(), token)
	if err != nil {
		h.httpError(w, r, "error.login_failed", http.StatusInternalServerError)
		return
	}
	if !ok || u.Login != login {
		rv.Fail()
		w.WriteHeader(http.StatusUnauthorized)
		h.renderPage(w, r, "login", h.loginData(r, "unknown_login"))
		return
	}
	rv.Success()
	sid, err := h.DB.CreateSession(r.Context(), u.ID, token, sessionTTL)
	if err != nil {
		h.httpError(w, r, "error.login_failed", http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, r, sid, sessionTTL)
	http.Redirect(w, r, h.loginData(r, "").Next, http.StatusFound)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = h.DB.DeleteSession(r.Context(), c.Value)
	}
	setSessionCookie(w, r, "", 0)
	http.Redirect(w, r, "/login", http.StatusFound)
}
