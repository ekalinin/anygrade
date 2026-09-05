package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/oidc"
	"github.com/ekalinin/anygrade/internal/oidc/oidctest"
	"github.com/ekalinin/anygrade/internal/ratelimit"
)

// newOIDCSite is newTestSite with an identity provider wired to a fake issuer.
func newOIDCSite(t *testing.T) (*Handler, *oidctest.Issuer) {
	t.Helper()
	h, _ := newTestSite(t)
	is, err := oidctest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(is.Close)
	p, err := oidc.New(oidc.Config{
		Issuer:       is.URL(),
		ClientID:     oidctest.ClientID,
		ClientSecret: oidctest.ClientSecret,
		RedirectURL:  "https://grade.example.org" + oidc.CallbackPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.OIDC = p
	return h, is
}

// startOIDC performs GET /oidc/start and returns the URL the browser is sent
// to plus the flow cookie it was handed.
func startOIDC(t *testing.T, h *Handler, next string) (string, *http.Cookie) {
	t.Helper()
	target := "/oidc/start"
	if next != "" {
		target += "?next=" + url.QueryEscape(next)
	}
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET %s: status %d, want 302 (body %q)", target, rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcCookie && c.Value != "" {
			return rec.Header().Get("Location"), c
		}
	}
	t.Fatalf("GET %s set no flow cookie (Set-Cookie: %q)", target, rec.Header().Get("Set-Cookie"))
	return "", nil
}

// callbackOIDC performs the provider's redirect back, with the flow cookie the
// browser would still be carrying.
func callbackOIDC(h *Handler, c *http.Cookie, code, state string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet,
		oidc.CallbackPath+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)
	return rec
}

// signIn drives the whole flow: start, mint a token at the issuer, come back.
func signIn(t *testing.T, h *Handler, is *oidctest.Issuer, tok oidctest.Token) *httptest.ResponseRecorder {
	t.Helper()
	authURL, flow := startOIDC(t, h, "")
	code, state, err := is.AuthCode(authURL, tok)
	if err != nil {
		t.Fatal(err)
	}
	return callbackOIDC(h, flow, code, state)
}

// sessionOf reads the session cookie a response set, or nil when it set none.
func sessionOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

// mustNoSession fails when a response handed out a session.
func mustNoSession(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code == http.StatusFound {
		t.Fatalf("%s: status 302, want a refusal", what)
	}
	if c := sessionOf(rec); c != nil {
		t.Fatalf("%s: a session was issued anyway (%s)", what, c.Value)
	}
}

// TestOIDCLoginBindsAndIssuesASession is the happy path: an account a teacher
// already invited, with no token and no activation behind it, gets a session
// from the provider and carries the subject afterwards.
func TestOIDCLoginBindsAndIssuesASession(t *testing.T) {
	h, is := newOIDCSite(t)
	alice, err := h.DB.CreateUser(t.Context(), "alice", "Alice", "student")
	if err != nil {
		t.Fatal(err)
	}

	rec := signIn(t, h, is, oidctest.Token{Subject: "sub-42", Login: "alice"})
	if rec.Code != http.StatusFound {
		t.Fatalf("callback: status %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	c := sessionOf(rec)
	if c == nil {
		t.Fatal("callback issued no session cookie")
	}
	u, ok, err := h.DB.LookupSession(t.Context(), c.Value)
	if err != nil || !ok || u.ID != alice.ID {
		t.Fatalf("session resolves to %+v (ok=%v err=%v), want alice", u, ok, err)
	}
	// The binding is the subject, not the login the claim happened to spell.
	bound, ok, err := h.DB.UserByOIDC(t.Context(), is.URL(), "sub-42")
	if err != nil || !ok || bound.ID != alice.ID {
		t.Fatalf("UserByOIDC = %+v (ok=%v err=%v), want alice", bound, ok, err)
	}
	// And it is on the audit log, so a teacher can see when the link was made.
	events, err := h.DB.ListEventsByTarget(t.Context(), "alice", 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == oidcBindEvent && strings.Contains(e.Detail, "sub-42") {
			found = true
		}
	}
	if !found {
		t.Errorf("the binding was not audited: %+v", events)
	}
}

// TestOIDCBindingSurvivesALoginChange is why the subject is what gets stored:
// the provider renames the student and the account is still theirs, and the new
// name does not have to exist as an account at all.
func TestOIDCBindingSurvivesALoginChange(t *testing.T) {
	h, is := newOIDCSite(t)
	alice, err := h.DB.CreateUser(t.Context(), "alice", "Alice", "student")
	if err != nil {
		t.Fatal(err)
	}
	if rec := signIn(t, h, is, oidctest.Token{Subject: "sub-42", Login: "alice"}); rec.Code != http.StatusFound {
		t.Fatalf("first login: status %d", rec.Code)
	}

	rec := signIn(t, h, is, oidctest.Token{Subject: "sub-42", Login: "alice.newname"})
	if rec.Code != http.StatusFound {
		t.Fatalf("login after a rename: status %d (body %q)", rec.Code, rec.Body.String())
	}
	u, ok, err := h.DB.LookupSession(t.Context(), sessionOf(rec).Value)
	if err != nil || !ok || u.ID != alice.ID {
		t.Fatalf("session resolves to %+v (ok=%v err=%v), want alice", u, ok, err)
	}
}

// TestOIDCUnboundSubjectGetsNoAccount: a verified identity is not enrolment.
// Creating an account here would make the provider the enrolment gate and
// quietly void `registration.mode: invite`, which is the teacher's decision.
func TestOIDCUnboundSubjectGetsNoAccount(t *testing.T) {
	h, is := newOIDCSite(t)

	rec := signIn(t, h, is, oidctest.Token{Subject: "sub-99", Login: "stranger"})
	mustNoSession(t, rec, "a subject with no account")
	if u, err := h.DB.GetUserByLogin(t.Context(), "stranger"); err == nil {
		t.Fatalf("an account was created for an unbound subject: %+v", u)
	}
	// The student is told what to do next, and nothing else.
	if !strings.Contains(rec.Body.String(), "ask your teacher for an invite") {
		t.Errorf("the refusal does not name the next step:\n%s", rec.Body.String())
	}
}

// TestOIDCRejectsAnUnusableLoginClaim: the claim is whatever the provider put
// in it. Nothing that cannot be an account login may reach the database or the
// audit log, where it would be unbounded provider-controlled text.
func TestOIDCRejectsAnUnusableLoginClaim(t *testing.T) {
	for _, claim := range []string{
		"Not A Login",
		"../../etc/passwd",
		strings.Repeat("a", 300),
	} {
		t.Run(claim[:min(len(claim), 20)], func(t *testing.T) {
			h, is := newOIDCSite(t)
			rec := signIn(t, h, is, oidctest.Token{Subject: "sub-1", Login: claim})
			mustNoSession(t, rec, "an unusable login claim")
			events, err := h.DB.ListEvents(t.Context(), "", "", 50, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if len(e.Target) > 64 || strings.Contains(e.Target, " ") || strings.Contains(e.Target, "..") {
					t.Errorf("an unusable claim reached the audit log: %+v", e)
				}
			}
		})
	}
}

// TestOIDCDisabledAccountGetsNoSession is the rule the other three credential
// paths carry (VerifyToken, LookupSession, UserByFingerprint all filter on
// state): a deactivated account gets no session, whichever credential it
// presents. Both before and after the account is bound.
func TestOIDCDisabledAccountGetsNoSession(t *testing.T) {
	for _, bindFirst := range []bool{false, true} {
		name := "never bound"
		if bindFirst {
			name = "bound, then deactivated"
		}
		t.Run(name, func(t *testing.T) {
			h, is := newOIDCSite(t)
			if _, err := h.DB.CreateUser(t.Context(), "alice", "Alice", "student"); err != nil {
				t.Fatal(err)
			}
			if bindFirst {
				if rec := signIn(t, h, is, oidctest.Token{Subject: "sub-42", Login: "alice"}); rec.Code != http.StatusFound {
					t.Fatalf("first login: status %d", rec.Code)
				}
			}
			if err := h.DB.SetUserState(t.Context(), "alice", "disabled"); err != nil {
				t.Fatal(err)
			}

			rec := signIn(t, h, is, oidctest.Token{Subject: "sub-42", Login: "alice"})
			mustNoSession(t, rec, "a disabled account")
		})
	}
}

// TestOIDCSecondSubjectDoesNotTakeOverAnAccount: once an account carries one
// provider identity, another may not have it. Whoever the provider hands the
// login "alice" to next would otherwise be one sign-in away from the real
// alice's submissions. Clearing the binding is a teacher's job
// (`anygrade user unbind-oidc`).
func TestOIDCSecondSubjectDoesNotTakeOverAnAccount(t *testing.T) {
	h, is := newOIDCSite(t)
	alice, err := h.DB.CreateUser(t.Context(), "alice", "Alice", "student")
	if err != nil {
		t.Fatal(err)
	}
	if rec := signIn(t, h, is, oidctest.Token{Subject: "sub-42", Login: "alice"}); rec.Code != http.StatusFound {
		t.Fatalf("first login: status %d", rec.Code)
	}

	rec := signIn(t, h, is, oidctest.Token{Subject: "sub-impostor", Login: "alice"})
	mustNoSession(t, rec, "a second subject claiming a bound account")
	bound, ok, err := h.DB.UserByOIDC(t.Context(), is.URL(), "sub-42")
	if err != nil || !ok || bound.ID != alice.ID {
		t.Fatalf("the original binding was disturbed: %+v ok=%v err=%v", bound, ok, err)
	}
	if _, ok, _ := h.DB.UserByOIDC(t.Context(), is.URL(), "sub-impostor"); ok {
		t.Fatal("the second subject was bound anyway")
	}
	// The refusal is on record: it is either a mistake to fix or two people
	// claiming one account, and both need a teacher.
	events, err := h.DB.ListEventsByTarget(t.Context(), "alice", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == oidcRefusedEvent {
			return
		}
	}
	t.Fatalf("the refusal was not audited: %+v", events)
}

// TestOIDCStateIsSingleUseAndBoundToTheBrowser: the state travels through the
// provider and the address bar, so it is public. What makes it a credential is
// the cookie only the browser that started the flow holds - and reading that
// cookie clears it, so nothing can be replayed.
func TestOIDCStateIsSingleUseAndBoundToTheBrowser(t *testing.T) {
	h, is := newOIDCSite(t)
	if _, err := h.DB.CreateUser(t.Context(), "alice", "Alice", "student"); err != nil {
		t.Fatal(err)
	}

	t.Run("no flow cookie", func(t *testing.T) {
		authURL, _ := startOIDC(t, h, "")
		code, state, err := is.AuthCode(authURL, oidctest.Token{Subject: "sub-42", Login: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		// Exactly the login-CSRF case: an attacker who started their own flow
		// feeds the victim's browser the callback URL.
		mustNoSession(t, callbackOIDC(h, nil, code, state), "a callback with no flow cookie")
	})

	t.Run("state does not match the cookie", func(t *testing.T) {
		authURL, flow := startOIDC(t, h, "")
		code, _, err := is.AuthCode(authURL, oidctest.Token{Subject: "sub-42", Login: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		mustNoSession(t, callbackOIDC(h, flow, code, "some-other-state"), "a mismatched state")
	})

	t.Run("replayed callback", func(t *testing.T) {
		authURL, flow := startOIDC(t, h, "")
		code, state, err := is.AuthCode(authURL, oidctest.Token{Subject: "sub-42", Login: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		first := callbackOIDC(h, flow, code, state)
		if first.Code != http.StatusFound {
			t.Fatalf("first callback: status %d (body %q)", first.Code, first.Body.String())
		}
		// The browser was told to drop the cookie; a client that ignored that
		// still gets nowhere, because the code is spent at the provider.
		if cleared := clearedFlowCookie(first); cleared == nil {
			t.Error("the flow cookie was not cleared, so the state stays replayable")
		}
		mustNoSession(t, callbackOIDC(h, flow, code, state), "a replayed callback")
	})

	t.Run("replayed nonce", func(t *testing.T) {
		// A perfectly valid ID token from an earlier login of the same student,
		// presented against a fresh flow: the nonce inside it belongs to the
		// other login.
		authURL, flow := startOIDC(t, h, "")
		code, state, err := is.AuthCode(authURL, oidctest.Token{
			Subject: "sub-42", Login: "alice", Nonce: "a-nonce-from-another-login",
		})
		if err != nil {
			t.Fatal(err)
		}
		mustNoSession(t, callbackOIDC(h, flow, code, state), "an id token minted for another login")
	})
}

func clearedFlowCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcCookie && c.Value == "" && c.MaxAge < 0 {
			return c
		}
	}
	return nil
}

// TestOIDCCallbackIsNotAnOpenRedirect: the post-login target is remembered
// across the round trip to the provider, so it is attacker-supplied input that
// comes back after an authentication - the most convincing possible moment to
// land somebody on another site.
func TestOIDCCallbackIsNotAnOpenRedirect(t *testing.T) {
	for _, next := range []string{
		"https://evil.example/phish",
		"//evil.example/phish",
		`/\evil.example/phish`,
		"http://evil.example",
		"/ok\r\nSet-Cookie: x=1",
	} {
		t.Run(next, func(t *testing.T) {
			h, is := newOIDCSite(t)
			if _, err := h.DB.CreateUser(t.Context(), "alice", "Alice", "student"); err != nil {
				t.Fatal(err)
			}
			authURL, flow := startOIDC(t, h, next)
			code, state, err := is.AuthCode(authURL, oidctest.Token{Subject: "sub-42", Login: "alice"})
			if err != nil {
				t.Fatal(err)
			}
			rec := callbackOIDC(h, flow, code, state)
			if rec.Code != http.StatusFound {
				t.Fatalf("callback: status %d (body %q)", rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != "/" {
				t.Errorf("Location = %q, want /: the callback must stay on this site", loc)
			}
		})
	}
	// A genuine same-site target is still honoured.
	h, is := newOIDCSite(t)
	if _, err := h.DB.CreateUser(t.Context(), "alice", "Alice", "student"); err != nil {
		t.Fatal(err)
	}
	authURL, flow := startOIDC(t, h, "/tasks/01-intro")
	code, state, err := is.AuthCode(authURL, oidctest.Token{Subject: "sub-42", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if loc := callbackOIDC(h, flow, code, state).Header().Get("Location"); loc != "/tasks/01-intro" {
		t.Errorf("Location = %q, want the requested page", loc)
	}
}

// TestOIDCDisabledByDefault: with no provider configured the site is exactly
// what it was - no button, and no route to reach at all.
func TestOIDCDisabledByDefault(t *testing.T) {
	h, _ := newTestSite(t)

	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login: status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "/oidc/start") {
		t.Errorf("the login page offers a provider button with none configured:\n%s", rec.Body.String())
	}
	for _, target := range []string{"/oidc/start", oidc.CallbackPath} {
		rec := httptest.NewRecorder()
		New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404 with no provider configured", target, rec.Code)
		}
	}
}

// TestOIDCLoginPageShowsTheProvider: configured, the button is there and names
// the provider, and the token form is still on the page - the token is the git
// password and nothing replaces it.
func TestOIDCLoginPageShowsTheProvider(t *testing.T) {
	h, _ := newOIDCSite(t)
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "/oidc/start") {
		t.Errorf("no provider button on the login page:\n%s", body)
	}
	if !strings.Contains(body, `name="token"`) {
		t.Errorf("the token login form is gone:\n%s", body)
	}
}

// TestOIDCCallbackChargesTheFailureBudget: the callback is the one
// unauthenticated route that makes the server open a connection to a third
// party, so a peer must not be able to extract those without bound.
func TestOIDCCallbackChargesTheFailureBudget(t *testing.T) {
	const max = 3
	h, is := newOIDCSite(t)
	h.Limit = ratelimit.New(max, time.Minute)
	if _, err := h.DB.CreateUser(t.Context(), "alice", "Alice", "student"); err != nil {
		t.Fatal(err)
	}

	for i := range max {
		if rec := callbackOIDC(h, nil, "nope", "nope"); rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: status %d, want 403", i, rec.Code)
		}
	}
	if rec := callbackOIDC(h, nil, "nope", "nope"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 once the budget is spent", rec.Code)
	}
	// A valid login clears it again, so a student who fumbled their password at
	// the provider a few times is not locked out afterwards.
	if rec := signIn(t, h, is, oidctest.Token{Subject: "sub-42", Login: "alice"}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429: the budget must not be bypassable", rec.Code)
	}
}

// TestTokenAfterOIDCLogin is the other half of the feature. A student who never
// saw an activation page has a session and no token, and the token is what git
// asks for - so settings has to offer one, show it once, and say what a
// rotation costs.
func TestTokenAfterOIDCLogin(t *testing.T) {
	h, is := newOIDCSite(t)
	alice, err := h.DB.CreateUser(t.Context(), "alice", "Alice", "student")
	if err != nil {
		t.Fatal(err)
	}
	rec := signIn(t, h, is, oidctest.Token{Subject: "sub-42", Login: "alice"})
	if rec.Code != http.StatusFound {
		t.Fatalf("login: status %d (body %q)", rec.Code, rec.Body.String())
	}
	session := sessionOf(rec)

	// The account has no token yet, and the page says so instead of offering to
	// regenerate one that does not exist.
	page := do(h, http.MethodGet, "/settings", session)
	if !strings.Contains(page.Body.String(), "You have no personal access token yet") {
		t.Errorf("settings does not offer a first token:\n%s", page.Body.String())
	}

	issued := doForm(h, "/settings/token", session, url.Values{})
	if issued.Code != http.StatusOK {
		t.Fatalf("POST /settings/token: status %d", issued.Code)
	}
	token := reToken.FindString(issued.Body.String())
	if token == "" {
		t.Fatalf("no token on the page:\n%s", issued.Body.String())
	}
	if u, ok, err := h.DB.VerifyToken(t.Context(), token); err != nil || !ok || u.ID != alice.ID {
		t.Fatalf("the issued token does not authenticate: %+v ok=%v err=%v", u, ok, err)
	}
	// Shown once: the page that follows does not carry it.
	after := do(h, http.MethodGet, "/settings", sessionOf(issued))
	if strings.Contains(after.Body.String(), token) {
		t.Error("the token is shown again after the one-time page")
	}
	if !strings.Contains(after.Body.String(), "Regenerating it invalidates the old one") {
		t.Errorf("settings does not say what a rotation breaks:\n%s", after.Body.String())
	}

	// And a rotation really does invalidate the old one, which is the thing a
	// stored git password depends on.
	rotated := doForm(h, "/settings/token", sessionOf(issued), url.Values{})
	newToken := reToken.FindString(rotated.Body.String())
	if newToken == "" || newToken == token {
		t.Fatalf("rotation did not issue a new token (%q -> %q)", token, newToken)
	}
	if _, ok, err := h.DB.VerifyToken(t.Context(), token); err != nil || ok {
		t.Errorf("the old token still authenticates after a rotation (ok=%v err=%v)", ok, err)
	}
}

// TestOIDCSessionSurvivesATokenRotation states the consequence of binding a
// provider session to no token: it was not opened by one, so rotating the token
// does not end it. Deactivating the account still does, which is the lever a
// teacher actually reaches for.
func TestOIDCSessionSurvivesATokenRotation(t *testing.T) {
	h, is := newOIDCSite(t)
	alice, err := h.DB.CreateUser(t.Context(), "alice", "Alice", "student")
	if err != nil {
		t.Fatal(err)
	}
	session := sessionOf(signIn(t, h, is, oidctest.Token{Subject: "sub-42", Login: "alice"}))
	if session == nil {
		t.Fatal("no session")
	}
	if _, err := h.DB.IssueToken(t.Context(), alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.DB.LookupSession(t.Context(), session.Value); err != nil || !ok {
		t.Fatalf("the provider session died on a token rotation: ok=%v err=%v", ok, err)
	}
	if err := h.DB.SetUserState(t.Context(), "alice", "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.DB.LookupSession(t.Context(), session.Value); err != nil || ok {
		t.Fatalf("a deactivated account kept its provider session: ok=%v err=%v", ok, err)
	}
}

// TestTokenSessionsStillDieOnRotation: the change above must not weaken what a
// token reset already did to the sessions the token opened.
func TestTokenSessionsStillDieOnRotation(t *testing.T) {
	h, _ := newTestSite(t)
	u, err := h.DB.CreateUser(t.Context(), "bob", "Bob", "student")
	if err != nil {
		t.Fatal(err)
	}
	token, err := h.DB.IssueToken(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := h.DB.CreateSession(t.Context(), u.ID, token, sessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.IssueToken(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.DB.LookupSession(t.Context(), sid); err != nil || ok {
		t.Fatalf("a token-bound session survived the reset: ok=%v err=%v", ok, err)
	}
}

// reToken matches an issued personal access token on the one-time page.
var reToken = regexp.MustCompile(`ag_[0-9a-f]{64}`)
