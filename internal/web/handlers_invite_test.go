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
