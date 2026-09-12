package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var tokenRE = regexp.MustCompile(`ag_[0-9a-f]{64}`)

// TestInviteActivatesOnce: one invite link, several simultaneous activations.
// VerifyInvite only proves the link was unused when it was read, so without an
// atomic consume every request got through: each rotated the token, and the
// last rotation killed the token the first student had already been shown
// (SPEC §8: the link is one-shot).
func TestInviteActivatesOnce(t *testing.T) {
	h, _ := newTestSite(t)
	target, err := h.DB.CreateUser(t.Context(), "bob", "Bob", "student")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := h.DB.CreateInvite(t.Context(), target.ID, "inv-tok", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invite: %v", err)
	}

	const n = 8
	site := New(h)
	codes := make([]int, n)
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			site.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/invite/inv-tok", nil))
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		})
	}
	wg.Wait()

	var shown string
	activated := 0
	for i := range n {
		if codes[i] != http.StatusOK {
			t.Fatalf("activation %d: status %d, want 200", i, codes[i])
		}
		if tok := tokenRE.FindString(bodies[i]); tok != "" {
			activated++
			shown = tok
		}
	}
	if activated != 1 {
		t.Fatalf("%d activations issued a token, want exactly 1", activated)
	}

	// The one student who saw a token still owns the account.
	u, ok, err := h.DB.VerifyToken(t.Context(), shown)
	if err != nil || !ok {
		t.Fatalf("the token shown to the student no longer works: ok=%v err=%v", ok, err)
	}
	if u.Login != target.Login {
		t.Fatalf("token belongs to %q, want %q", u.Login, target.Login)
	}
	if _, ok, err := h.DB.VerifyInvite(t.Context(), "inv-tok"); err != nil || ok {
		t.Fatalf("the invite must be spent: ok=%v err=%v", ok, err)
	}
}

// TestInviteIgnoresPostedKey pins the decision in SPEC §8: activation no
// longer registers SSH keys at all. The invite proves possession of the
// invite, not of a private key, so a key accepted here would have stayed the
// one path on which a student could claim a classmate's public key.
func TestInviteIgnoresPostedKey(t *testing.T) {
	h, _ := newTestSite(t)
	h.SSHAddr = ":2222" // so the page renders its SSH section at all
	target, err := h.DB.CreateUser(t.Context(), "carol", "Carol", "student")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := h.DB.CreateInvite(t.Context(), target.ID, "inv-tok", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invite: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/invite/inv-tok",
		strings.NewReader(url.Values{"key": {newTestKey(t).authorized}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	New(h).ServeHTTP(rec, req)

	// The account still activates - the key field is simply not a thing here.
	if rec.Code != http.StatusOK || !tokenRE.MatchString(rec.Body.String()) {
		t.Fatalf("activation failed: status %d", rec.Code)
	}
	if keys, err := h.DB.ListSSHKeys(t.Context(), target.ID); err != nil || len(keys) != 0 {
		t.Fatalf("activation registered a key: %v (err %v)", keys, err)
	}
	// The page tells the student where keys are added instead.
	if !strings.Contains(rec.Body.String(), "/settings") {
		t.Errorf("the token page does not point at settings:\n%s", rec.Body.String())
	}
}

// TestInviteRefusedForActivatedAccount: a link whose account already holds a
// personal token is dead. Activating again issues a new token - revoking the
// one its owner is using - and logs whoever opened the link in as them, so the
// link renders the neutral invalid page on GET and on POST and changes nothing
// (SPEC §8). The page must stay neutral: naming the account would make the
// link an oracle for which accounts are live.
func TestInviteRefusedForActivatedAccount(t *testing.T) {
	h, _ := newTestSite(t)
	target, err := h.DB.CreateUser(t.Context(), "bob", "Bob", "student")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// The shape this backstops: the link was issued while the account had no
	// token, and `user reset-token` gave it one before the link was opened.
	if err := h.DB.CreateInvite(t.Context(), target.ID, "inv-tok", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	held, err := h.DB.IssueToken(t.Context(), target.ID)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	site := New(h)
	rec := httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/invite/inv-tok", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "invalid, expired, or already used") {
		t.Fatalf("GET: status %d, body:\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "bob") {
		t.Errorf("the page names the account behind the link:\n%s", rec.Body.String())
	}
	if _, ok, verr := h.DB.VerifyInvite(t.Context(), "inv-tok"); verr != nil || ok {
		t.Fatalf("the refused link is still live after the GET: ok=%v err=%v", ok, verr)
	}

	// The same again on POST, against a link the GET has not already spent.
	if err := h.DB.CreateInvite(t.Context(), target.ID, "inv-again", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("re-create invite: %v", err)
	}
	rec = httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/invite/inv-again", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "invalid, expired, or already used") {
		t.Fatalf("POST: status %d, body:\n%s", rec.Code, rec.Body.String())
	}
	if tok := tokenRE.FindString(rec.Body.String()); tok != "" {
		t.Errorf("the refused activation issued a token: %s", tok)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("the refused activation opened a session: %v", cookies)
	}
	events, err := h.DB.ListEvents(t.Context(), "user.activate", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("the refused activation was audited as one: %+v", events)
	}
	if u, ok, verr := h.DB.VerifyToken(t.Context(), held); verr != nil || !ok || u.Login != target.Login {
		t.Fatalf("the account's own token stopped working: ok=%v err=%v", ok, verr)
	}
	if _, ok, verr := h.DB.VerifyInvite(t.Context(), "inv-again"); verr != nil || ok {
		t.Fatalf("the refused link is still live after the POST: ok=%v err=%v", ok, verr)
	}
}

// TestReInviteDoesNotRevokeTheActiveToken replays the report behind the fix: a
// roster re-run issued a second link for an account that had already been
// activated, and whoever opened that link was logged in as the student while
// the student's own token stopped working (SPEC §8).
func TestReInviteDoesNotRevokeTheActiveToken(t *testing.T) {
	h, _ := newTestSite(t)
	target, err := h.DB.CreateUser(t.Context(), "bob", "Bob", "student")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := h.DB.CreateInvite(t.Context(), target.ID, "inv-first", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invite: %v", err)
	}

	site := New(h)
	rec := httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/invite/inv-first", nil))
	first := tokenRE.FindString(rec.Body.String())
	if rec.Code != http.StatusOK || first == "" {
		t.Fatalf("activation: status %d, body:\n%s", rec.Code, rec.Body.String())
	}

	// The teacher re-runs the roster and the account is on it again.
	if err := h.DB.CreateInvite(t.Context(), target.ID, "inv-second", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("second invite: %v", err)
	}
	rec = httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/invite/inv-second", nil))
	if !strings.Contains(rec.Body.String(), "invalid, expired, or already used") {
		t.Errorf("the second link did not render the neutral page:\n%s", rec.Body.String())
	}
	if tok := tokenRE.FindString(rec.Body.String()); tok != "" {
		t.Errorf("the second link activated the account again: %s", tok)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("the second link logged its opener in: %v", cookies)
	}
	events, err := h.DB.ListEvents(t.Context(), "user.activate", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Errorf("%d user.activate events, want only the student's own", len(events))
	}
	if u, ok, verr := h.DB.VerifyToken(t.Context(), first); verr != nil || !ok || u.Login != target.Login {
		t.Fatalf("the second link revoked the student's token: ok=%v err=%v", ok, verr)
	}
}

// TestInviteRefusedForOIDCBoundAccount: an account that signs in through the
// identity provider holds no personal token - it takes its first from the
// settings page (SPEC §8) - so a check for a token alone would leave every
// such student invitable, and the link would issue that first token to
// whoever opened it and log them in as the student.
func TestInviteRefusedForOIDCBoundAccount(t *testing.T) {
	h, _ := newTestSite(t)
	target, err := h.DB.CreateUser(t.Context(), "bob", "Bob", "student")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := h.DB.CreateInvite(t.Context(), target.ID, "inv-tok", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if bound, berr := h.DB.BindOIDC(t.Context(), target.ID, "https://idp.example", "sub-1"); berr != nil || !bound {
		t.Fatalf("bind: bound=%v err=%v", bound, berr)
	}

	site := New(h)
	rec := httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/invite/inv-tok", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "invalid, expired, or already used") {
		t.Fatalf("GET: status %d, body:\n%s", rec.Code, rec.Body.String())
	}

	if err := h.DB.CreateInvite(t.Context(), target.ID, "inv-again", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("re-create invite: %v", err)
	}
	rec = httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/invite/inv-again", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "invalid, expired, or already used") {
		t.Fatalf("POST: status %d, body:\n%s", rec.Code, rec.Body.String())
	}
	if tok := tokenRE.FindString(rec.Body.String()); tok != "" {
		t.Errorf("the refused activation issued a token: %s", tok)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("the refused activation opened a session: %v", cookies)
	}
	// The account is still exactly as its owner left it: no token was created
	// behind their back, and the link is spent.
	if held, herr := h.DB.HasToken(t.Context(), target.ID); herr != nil || held {
		t.Errorf("the refused activation gave the account a token: held=%v err=%v", held, herr)
	}
	if _, ok, verr := h.DB.VerifyInvite(t.Context(), "inv-again"); verr != nil || ok {
		t.Fatalf("the refused link is still live: ok=%v err=%v", ok, verr)
	}
}
