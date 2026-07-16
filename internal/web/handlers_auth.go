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
	renderPage(w, "login", h.loginData(r, ""))
}

func (h *Handler) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	login := strings.TrimSpace(r.FormValue("login"))
	token := strings.TrimSpace(r.FormValue("token"))
	key := ratelimit.AuthKey(r.RemoteAddr, login)
	if h.Limit != nil && h.Limit.Blocked(key) {
		w.WriteHeader(http.StatusTooManyRequests)
		renderPage(w, "login", h.loginData(r, "too many failed attempts, try again later"))
		return
	}
	u, ok, err := h.DB.VerifyToken(r.Context(), token)
	if err != nil {
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	if !ok || u.Login != login {
		if h.Limit != nil {
			h.Limit.Fail(key)
		}
		w.WriteHeader(http.StatusUnauthorized)
		renderPage(w, "login", h.loginData(r, "unknown login or token"))
		return
	}
	if h.Limit != nil {
		h.Limit.Clear(key)
	}
	sid, err := h.DB.CreateSession(r.Context(), u.ID, token, sessionTTL)
	if err != nil {
		http.Error(w, "login failed", http.StatusInternalServerError)
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
