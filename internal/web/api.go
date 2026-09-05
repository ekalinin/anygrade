package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ekalinin/anygrade/internal/ratelimit"
	"github.com/ekalinin/anygrade/internal/store"
)

// This file is the transport half of the JSON API (SPEC §10.2): bearer
// authentication, the error shape, and the JSON writer. The endpoints
// themselves are in handlers_api.go, and they encode the very read models the
// pages render - the API is a second encoder, not a second query layer.
//
// The contract inside /api/v1/ is: fields may be added, and an existing field
// never changes type or disappears, so a client that ignores unknown fields
// keeps working. Anything that would break that gets /api/v2/ instead.

// apiLimitKey is the fixed login half of the bearer failure budget. A bearer
// carries no login to key on - and the token itself must never become part of
// a key, since keys are held in memory and logged nowhere but are still the
// credential - so every API attempt from one address shares one budget with
// that address's failed logins and git basic auth. Without it the API would be
// the unthrottled way to guess a token (SPEC §14).
const apiLimitKey = "\x00api"

// Error codes are the machine-readable half of a failure; the message beside
// one is for a human reading a log. Neither goes through the i18n catalogs:
// those cover what a browser renders (SPEC §10.1), and a script parses the
// code, not the prose.
const (
	codeUnauthorized = "unauthorized"
	codeNotFound     = "not_found"
	codeRateLimited  = "rate_limited"
	codeInternal     = "internal"
)

// apiErrorBody is the failure envelope: one key, the same shape on every
// endpoint, so a client tells an error from a payload without consulting the
// status code first.
type apiErrorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON encodes one response. A failure here can only be a dead client
// connection: the DTOs are plain structs with no marshaler of their own.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiFail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiErrorBody{apiError{Code: code, Message: msg}})
}

// apiNotFound is the only answer the API gives about a resource the caller may
// not have: the UI's rule that a student sees 404 rather than 403 holds here
// too, so an id cannot be probed for existence (SPEC §14).
func apiNotFound(w http.ResponseWriter) {
	apiFail(w, http.StatusNotFound, codeNotFound, "not found")
}

// requireAPI authenticates one request with the personal token as a bearer -
// the same token that is the git basic-auth password and the web login
// credential (SPEC §8), so scripts need no second kind of secret.
//
// Nothing of the browser session applies: no cookie is read, none is written,
// and a missing or bad token is a JSON 401 rather than the login redirect
// requireAuth serves, because a script has no form to follow.
func (h *Handler) requireAPI(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := h.apiUser(w, r)
		if !ok {
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

// requireAPIReview layers a right over requireAPI exactly as requireReview
// does over requireAuth, and asks the same predicate (store/roles.go) rather
// than comparing the role itself: the API is a second encoder over the pages'
// read models, so the two must answer the same question or one of them becomes
// the way around the other. The matrix and the queue are the reviewing half,
// which a TA already reads in the UI.
func (h *Handler) requireAPIReview(next http.HandlerFunc) http.Handler {
	return h.requireAPI(func(w http.ResponseWriter, r *http.Request) {
		if !user(r).CanReview() {
			apiNotFound(w) // do not leak the route's existence (SPEC §14)
			return
		}
		next(w, r)
	})
}

// apiUser resolves the bearer token, writing the failure itself.
func (h *Handler) apiUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	if h.Local != nil {
		return *h.Local, true // serve --local: one implicit user, no credentials (SPEC §8)
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		h.apiUnauthorized(w, "bearer token required")
		return store.User{}, false
	}
	// Reserve, not Blocked, for the reason the login form and git basic auth
	// hold the budget this way: the slot is taken before the token is compared,
	// so a burst of simultaneous guesses cannot all pass the check while the
	// first failure is still being recorded.
	rv, allowed := h.Limit.Reserve(ratelimit.AuthKey(h.clientAddr(r), apiLimitKey))
	if !allowed {
		apiFail(w, http.StatusTooManyRequests, codeRateLimited, "too many failed attempts, try again later")
		return store.User{}, false
	}
	defer rv.Release() // no-op once the outcome below is reported
	u, valid, err := h.DB.VerifyToken(r.Context(), token)
	if err != nil {
		apiFail(w, http.StatusInternalServerError, codeInternal, "authentication failed")
		return store.User{}, false
	}
	if !valid {
		rv.Fail()
		h.apiUnauthorized(w, "invalid token")
		return store.User{}, false
	}
	rv.Success()
	return u, true
}

// apiUnauthorized answers a credential failure. The challenge header names the
// scheme the API accepts and nothing else: Basic would make a browser that
// wandered onto an endpoint pop a password box for a token it cannot use.
func (h *Handler) apiUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="anygrade"`)
	apiFail(w, http.StatusUnauthorized, codeUnauthorized, msg)
}

// bearerToken extracts the credential from an Authorization header. The scheme
// is matched case-insensitively (RFC 7235); anything else - Basic, a bare
// token, no header at all - is simply not an API credential.
func bearerToken(header string) (string, bool) {
	scheme, rest, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	return token, token != ""
}
