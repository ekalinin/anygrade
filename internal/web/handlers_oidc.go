package web

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/ekalinin/anygrade/internal/ident"
	"github.com/ekalinin/anygrade/internal/oidc"
	"github.com/ekalinin/anygrade/internal/ratelimit"
	"github.com/ekalinin/anygrade/internal/store"
)

// oidcCookie carries the in-flight login between the redirect out to the
// provider and the callback back. It is what binds the flow to one browser:
// the `state` in the callback URL is public - it travels through the provider
// and through the address bar - so on its own it would let anyone who obtained
// a code complete a login into somebody else's browser. Only the browser that
// started the flow holds the matching cookie.
//
// Path is /oidc so it is not attached to every request on the site, and
// SameSite=Lax rather than Strict because the callback *is* a cross-site
// top-level navigation: Strict would withhold the cookie exactly when it is
// needed.
const oidcCookie = "ag_oidc"

// oidcFlowTTL is how long a student has to finish authenticating at the
// provider. Long enough for a password, a second factor and a consent screen;
// short enough that an abandoned flow does not stay redeemable.
const oidcFlowTTL = 10 * time.Minute

// oidcLimitKey is the fixed login half of the callback's failure-budget key, so
// the budget is per client IP. See oidcCallback for why the callback is on the
// limiter at all.
const oidcLimitKey = "\x00oidc"

// Audit kinds. A successful repeat login is not audited - no other credential
// path audits one either - but the moment an identity is first attached to an
// account is, and so is every verified identity that was refused one.
const (
	oidcBindEvent    = "user.oidc_bind"
	oidcRefusedEvent = "user.oidc_refused"
)

// oidcFlow is the state cookie's payload.
type oidcFlow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"r"`
}

// oidcStart sends the browser to the provider. The route exists only when a
// provider is configured (see New), so with no configuration this is a 404 and
// the login page shows no button.
func (h *Handler) oidcStart(w http.ResponseWriter, r *http.Request) {
	flow, err := oidc.NewFlow()
	if err != nil {
		h.oidcRefuse(w, r, "oidc_failed", http.StatusInternalServerError, "generating the login state failed", err)
		return
	}
	target, err := h.OIDC.AuthCodeURL(r.Context(), flow)
	if err != nil {
		// Discovery is the usual failure here, i.e. the provider is down. The
		// student is told the provider could not be reached and can still use
		// the token form on the same page.
		h.oidcRefuse(w, r, "oidc_unavailable", http.StatusServiceUnavailable, "building the authorization url failed", err)
		return
	}
	payload, err := json.Marshal(oidcFlow{
		State: flow.State, Nonce: flow.Nonce, Verifier: flow.Verifier,
		Next: safeNext(r.FormValue("next")),
	})
	if err != nil {
		h.oidcRefuse(w, r, "oidc_failed", http.StatusInternalServerError, "encoding the login state failed", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oidcCookie,
		Value:    base64.RawURLEncoding.EncodeToString(payload),
		Path:     "/oidc",
		MaxAge:   int(oidcFlowTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure(r),
	})
	http.Redirect(w, r, target, http.StatusFound)
}

// oidcCallback is the whole verification. Everything before the ID token
// verifies is unauthenticated input, including the account the claims name.
//
// The callback is on the shared failure budget (SPEC §14), and not because the
// authorization code is guessable - it is not, and the provider has its own
// limits on redeeming one. It is because a failing callback is the only
// unauthenticated route on the site that makes the server open an outbound
// connection to a third party, so the budget is what bounds how many of those
// an anonymous peer can extract from it. A success clears the key, so a student
// who mistypes their password at the provider a few times is not affected.
func (h *Handler) oidcCallback(w http.ResponseWriter, r *http.Request) {
	rv, allowed := h.Limit.Reserve(ratelimit.AuthKey(h.clientAddr(r), oidcLimitKey))
	if !allowed {
		w.WriteHeader(http.StatusTooManyRequests)
		h.renderPage(w, r, "login", h.loginData(r, "too_many_attempts"))
		return
	}
	defer rv.Release() // no-op once the outcome below is reported

	// The flow cookie is read and cleared in one step: that is what makes the
	// state and the nonce single-use. A replayed callback finds no cookie.
	flow, ok := h.takeOIDCFlow(w, r)
	if !ok {
		rv.Fail()
		h.oidcRefuse(w, r, "oidc_failed", http.StatusForbidden,
			"callback without a live login state (expired, replayed, or not started here)", nil)
		return
	}
	// The provider's own refusal (the student cancelled, or consent was denied).
	// The code is echoed into the log clipped: it arrives in a URL anyone can
	// write, and the OAuth error codes are all short.
	if e := r.URL.Query().Get("error"); e != "" {
		rv.Fail()
		h.oidcRefuse(w, r, "oidc_failed", http.StatusForbidden,
			"provider returned error="+clip(e, 64), nil)
		return
	}
	if subtle.ConstantTimeCompare([]byte(flow.State), []byte(r.URL.Query().Get("state"))) != 1 {
		rv.Fail()
		h.oidcRefuse(w, r, "oidc_failed", http.StatusForbidden,
			"callback state does not match the one this browser started with", nil)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		rv.Fail()
		h.oidcRefuse(w, r, "oidc_failed", http.StatusForbidden, "callback carries no authorization code", nil)
		return
	}
	id, err := h.OIDC.Exchange(r.Context(), code, oidc.Flow{
		State: flow.State, Nonce: flow.Nonce, Verifier: flow.Verifier,
	})
	if err != nil {
		// Why the token was refused - wrong issuer, bad signature, replayed
		// nonce - is the operator's, not the browser's. The same scrubbing rule
		// internal/hidden applies to its errors applies here (SPEC §14).
		rv.Fail()
		h.oidcRefuse(w, r, "oidc_failed", http.StatusForbidden, "id token rejected", err)
		return
	}

	u, ok := h.oidcAccount(r, id)
	if !ok {
		rv.Fail()
		// The identity is genuine; it simply has no account here. This is the
		// one refusal a student can act on, so it names the next step and
		// nothing else - not whether the account exists, is disabled, or is
		// already linked to a different provider identity.
		h.oidcRefuse(w, r, "oidc_no_account", http.StatusForbidden, "", nil)
		return
	}
	rv.Success()

	// No token binding: this session was not opened by a token, and the account
	// may not even have one (SPEC §8).
	sid, err := h.DB.CreateSession(r.Context(), u.ID, "", sessionTTL)
	if err != nil {
		h.httpError(w, r, "error.login_failed", http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, r, sid, sessionTTL)
	http.Redirect(w, r, safeNext(flow.Next), http.StatusFound)
}

// oidcAccount maps a verified identity onto an account. It never creates one:
// enrolment is the course's policy (`registration.mode`), and configuring an
// identity provider is not a decision to hand that policy to the provider.
//
// The subject is looked up first, so a student whose login or email changed at
// the provider keeps their account. Only an identity nobody is bound to falls
// through to the login claim, and the binding it makes is recorded once.
func (h *Handler) oidcAccount(r *http.Request, id oidc.Identity) (store.User, bool) {
	if u, ok, err := h.DB.UserByOIDC(r.Context(), id.Issuer, id.Subject); err != nil {
		slog.Error("oidc: subject lookup failed", "err", err)
		return store.User{}, false
	} else if ok {
		return u, true
	}
	// The claim is whatever the provider chose to put in it, and no account can
	// be named anything else (internal/ident), so a value that fails the rule
	// could not match one. Refusing before the lookup also keeps it out of the
	// audit log, which is where an unbounded claim would otherwise end up.
	if !ident.ValidLogin(id.Login) {
		slog.Warn("oidc: the login claim is not a valid account login",
			"issuer", id.Issuer, "subject", id.Subject)
		return store.User{}, false
	}
	target, err := h.DB.GetUserByLogin(r.Context(), id.Login)
	if err != nil {
		slog.Warn("oidc: verified identity has no account",
			"claim", id.Login, "issuer", id.Issuer, "subject", id.Subject)
		h.logOIDCRefusal(r, id, "no account with this login")
		return store.User{}, false
	}
	if target.State != "active" {
		// The same rule the three other credential paths carry: a deactivated
		// account gets no session, whichever credential it presents.
		slog.Warn("oidc: refused a disabled account", "login", target.Login)
		h.logOIDCRefusal(r, id, "account is disabled")
		return store.User{}, false
	}
	bound, err := h.DB.BindOIDC(r.Context(), target.ID, id.Issuer, id.Subject)
	if err != nil {
		slog.Error("oidc: binding failed", "login", target.Login, "err", err)
		return store.User{}, false
	}
	if !bound {
		// The account already carries another subject, or this subject already
		// belongs to somebody else. Either is a case for a teacher - the first
		// is cleared with `anygrade user unbind-oidc`, the second is two people
		// claiming one account - and neither may be resolved by whoever
		// happens to log in next.
		slog.Warn("oidc: account is already bound to a different subject",
			"login", target.Login, "issuer", id.Issuer, "subject", id.Subject)
		h.logOIDCRefusal(r, id, "account is already linked to a different provider identity")
		return store.User{}, false
	}
	_ = h.DB.Log(r.Context(), store.Event{
		ActorID: &target.ID, Kind: oidcBindEvent, Target: target.Login,
		Detail: "linked to " + id.Issuer + " subject " + id.Subject,
	})
	return target, true
}

// logOIDCRefusal records a verified identity that was refused an account. The
// actor is nil - there is no account to attribute it to, which is the point -
// and the target is the login the claim asked for, so it shows up on the audit
// page next to that student's other events.
func (h *Handler) logOIDCRefusal(r *http.Request, id oidc.Identity, reason string) {
	_ = h.DB.Log(r.Context(), store.Event{
		Kind: oidcRefusedEvent, Target: id.Login,
		Detail: reason + " (" + id.Issuer + " subject " + id.Subject + ")",
	})
}

// takeOIDCFlow reads the flow cookie and clears it in the same response, which
// is what makes the state and the nonce inside it single-use.
func (h *Handler) takeOIDCFlow(w http.ResponseWriter, r *http.Request) (oidcFlow, bool) {
	c, err := r.Cookie(oidcCookie)
	// Clear unconditionally: a malformed cookie must not survive to be retried.
	http.SetCookie(w, &http.Cookie{
		Name: oidcCookie, Value: "", Path: "/oidc", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure(r),
	})
	if err != nil || c.Value == "" {
		return oidcFlow{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return oidcFlow{}, false
	}
	var f oidcFlow
	if err := json.Unmarshal(raw, &f); err != nil {
		return oidcFlow{}, false
	}
	if f.State == "" || f.Nonce == "" || f.Verifier == "" {
		return oidcFlow{}, false
	}
	return f, true
}

// clip bounds a string taken from the request before it reaches a log line.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// oidcRefuse renders the login page with a flash and logs the real reason for
// the operator. The browser is never told which check failed: a student cannot
// act on it, and an attacker would use it to find out which part of a forged
// token to fix (SPEC §14).
func (h *Handler) oidcRefuse(w http.ResponseWriter, r *http.Request, flash string, code int, detail string, err error) {
	if detail != "" || err != nil {
		slog.Warn("oidc login refused", "detail", detail, "err", err, "addr", h.clientAddr(r))
	}
	w.WriteHeader(code)
	h.renderPage(w, r, "login", h.loginData(r, flash))
}
