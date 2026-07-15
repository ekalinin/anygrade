package web

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/ekalinin/anygrade/internal/store"
)

const (
	sessionCookie = "ag_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type ctxKey int

const userKey ctxKey = 0

// currentUser reads the session cookie and resolves it to an active user.
func (h *Handler) currentUser(r *http.Request) (store.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return store.User{}, false
	}
	u, ok, err := h.DB.LookupSession(r.Context(), c.Value)
	if err != nil || !ok {
		return store.User{}, false
	}
	return u, true
}

// requireAuth gates a handler behind a session: browsers get a redirect to
// the login form, non-navigation requests (POST, SSE) get a plain 401. POST
// requests additionally pass a same-origin check (CSRF, SameSite=Lax belt).
func (h *Handler) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := h.currentUser(r)
		if !ok {
			if r.Method == http.MethodGet && r.Header.Get("Accept") != "text/event-stream" {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

// user returns the authenticated user placed by requireAuth.
func user(r *http.Request) store.User {
	u, _ := r.Context().Value(userKey).(store.User)
	return u
}

// sameOrigin rejects a POST whose Origin (or, failing that, Referer) names
// another host. Absent headers pass: SameSite=Lax already keeps the session
// cookie off cross-site POSTs; this is defense in depth.
func sameOrigin(r *http.Request) bool {
	check := func(raw string) bool {
		u, err := url.Parse(raw)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	}
	if o := r.Header.Get("Origin"); o != "" {
		return check(o)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return check(ref)
	}
	return true
}

// setSessionCookie writes (or, with empty value, clears) the session cookie.
func setSessionCookie(w http.ResponseWriter, r *http.Request, value string, ttl time.Duration) {
	maxAge := int(ttl.Seconds())
	if value == "" {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}
