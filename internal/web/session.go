package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ekalinin/anygrade/internal/store"
)

const (
	sessionCookie = "ag_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type ctxKey int

const (
	userKey ctxKey = iota
	secureKey
)

// secureContext records, once per request, whether the browser's connection to
// the site is encrypted. It is a middleware rather than a check inside
// setSessionCookie so that every cookie writer - login, activation,
// registration, token regeneration - agrees without carrying the Handler.
func (h *Handler) secureContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), secureKey, h.isSecure(r))))
	})
}

// isSecure reports whether the client reached the site over TLS: either this
// process terminated it, or the operator explicitly opted into trusting a
// reverse proxy's X-Forwarded-Proto. The header is trivially forgeable by
// anyone who can reach the port directly, so it is honoured only behind that
// opt-in - there is no trusted-CIDR machinery to get wrong.
func (h *Handler) isSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return h.BehindProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// secure reads the flag secureContext placed on the request.
func secure(r *http.Request) bool {
	ok, _ := r.Context().Value(secureKey).(bool)
	return ok
}

// currentUser reads the session cookie and resolves it to an active user.
// In local mode (SPEC §8) there are no sessions: every request is the single
// implicit user.
func (h *Handler) currentUser(r *http.Request) (store.User, bool) {
	if h.Local != nil {
		return *h.Local, true
	}
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
			h.httpError(w, r, "error.auth_required", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			h.httpError(w, r, "error.cross_origin", http.StatusForbidden)
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
		Secure:   secure(r),
	})
}
