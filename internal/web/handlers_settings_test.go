package web

import (
	"crypto/ed25519"
	"net/http"
	"net/url"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

// authorizedKey generates one throwaway authorized_keys line.
func authorizedKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(signer)))
}

// TestDuplicateSSHKeyIsReported: public keys are public and the fingerprint is
// globally unique, so posting a classmate's key locks them out of their own -
// their SSH sessions resolve to the squatter and are refused on their own repo.
// Proof of possession is the real fix (follow-up); until then the attempt is
// audited with both accounts named, so a teacher can find and delete the key.
func TestDuplicateSSHKeyIsReported(t *testing.T) {
	h, _ := newTestSite(t)
	key := authorizedKey(t)

	squatter, squatterSession := newSession(t, h, "mallory", "student")
	victim, victimSession := newSession(t, h, "alice", "student")

	if rec := doForm(h, "/settings/keys", squatterSession, url.Values{"key": {key}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("first registration: status %d", rec.Code)
	}
	rec := doForm(h, "/settings/keys", victimSession, url.Values{"key": {key}})
	if want := "/settings?flash=key_registered_elsewhere"; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}

	// The event is filed against the account that holds the key, which is the
	// page the teacher deletes it from.
	events, err := h.DB.ListEventsByTarget(t.Context(), squatter.Login, 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "key.duplicate" {
			found = true
			if e.ActorLogin != victim.Login || !strings.Contains(e.Detail, victim.Login) {
				t.Errorf("event does not name the requester: actor=%q detail=%q", e.ActorLogin, e.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("no key.duplicate event against %q: %+v", squatter.Login, events)
	}

	// The teacher sees the offending key on the holder's page and can remove it.
	_, teacher := newSession(t, h, "teacher", "teacher")
	page := do(h, http.MethodGet, "/students/"+squatter.Login, teacher)
	keys, err := h.DB.ListSSHKeys(t.Context(), squatter.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListSSHKeys: %v (%d keys)", err, len(keys))
	}
	// html/template escapes '+' and '=' in text content, so compare like for like.
	shown := strings.NewReplacer("+", "&#43;", "=", "&#61;").Replace(keys[0].Fingerprint)
	if !strings.Contains(page.Body.String(), shown) {
		t.Errorf("the teacher page does not show the key %q", keys[0].Fingerprint)
	}
	if want := "/students/" + squatter.Login + "/keys/" + itoa(keys[0].ID) + "/delete"; !strings.Contains(page.Body.String(), want) {
		t.Errorf("the teacher page offers no way to remove the key (%s)", want)
	}
	del := doForm(h, "/students/"+squatter.Login+"/keys/"+itoa(keys[0].ID)+"/delete",
		teacher, url.Values{"fingerprint": {keys[0].Fingerprint}})
	if del.Code != http.StatusSeeOther {
		t.Fatalf("delete: status %d", del.Code)
	}

	// With the squat cleared, the rightful owner can register the key.
	if rec := doForm(h, "/settings/keys", victimSession, url.Values{"key": {key}}); rec.Header().Get("Location") != "/settings" {
		t.Errorf("owner still cannot register the key: %q", rec.Header().Get("Location"))
	}
}

// TestOwnDuplicateSSHKeyIsNotAudited: re-adding your own key is a typo, not an
// incident - it keeps the plain message and writes no event.
func TestOwnDuplicateSSHKeyIsNotAudited(t *testing.T) {
	h, _ := newTestSite(t)
	key := authorizedKey(t)
	u, session := newSession(t, h, "alice", "student")

	doForm(h, "/settings/keys", session, url.Values{"key": {key}})
	rec := doForm(h, "/settings/keys", session, url.Values{"key": {key}})
	if want := "/settings?flash=key_already_registered"; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
	events, err := h.DB.ListEventsByTarget(t.Context(), u.Login, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "key.duplicate" {
			t.Errorf("re-adding your own key was audited as a squat: %+v", e)
		}
	}
}

// TestDuplicateSSHKeyReportsDisabledHolder is the case a state-filtered lookup
// silently dropped: the squatter's account is disabled, so the victim is still
// refused their own key while nothing at all reaches the teacher.
func TestDuplicateSSHKeyReportsDisabledHolder(t *testing.T) {
	h, _ := newTestSite(t)
	key := authorizedKey(t)

	squatter, squatterSession := newSession(t, h, "mallory", "student")
	victim, victimSession := newSession(t, h, "alice", "student")

	if rec := doForm(h, "/settings/keys", squatterSession, url.Values{"key": {key}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("first registration: status %d", rec.Code)
	}
	if err := h.DB.SetUserState(t.Context(), squatter.Login, "disabled"); err != nil {
		t.Fatal(err)
	}

	rec := doForm(h, "/settings/keys", victimSession, url.Values{"key": {key}})
	if want := "/settings?flash=key_registered_elsewhere"; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
	events, err := h.DB.ListEventsByTarget(t.Context(), squatter.Login, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "key.duplicate" && strings.Contains(e.Detail, victim.Login) {
			return
		}
	}
	t.Fatalf("no key.duplicate event against the disabled holder %q: %+v", squatter.Login, events)
}
