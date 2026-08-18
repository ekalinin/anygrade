package web

import (
	"crypto/ed25519"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/ekalinin/anygrade/internal/sshsig"
)

var (
	// nonceRE picks the challenge out of the rendered shell command; the page
	// is the only place the plaintext nonce ever exists (the DB holds its hash).
	nonceRE = regexp.MustCompile(`agc_[0-9a-f]{64}`)
	// proofCmdRE reads the message the page tells the student to sign, straight
	// out of the command it prints - the same string a student copies.
	proofCmdRE = regexp.MustCompile(`printf '%s' '([^']*)'`)
)

// challenge is what step one hands back: the nonce that identifies it and the
// exact line the student is told to sign.
type challenge struct {
	nonce   string
	message string
}

// testKey is one throwaway key pair: the authorized_keys line a student would
// paste, plus the signer that stands in for their private half.
type testKey struct {
	authorized string
	signer     gossh.Signer
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{
		authorized: strings.TrimSpace(string(gossh.MarshalAuthorizedKey(sshPub))),
		signer:     signer,
	}
}

func (k testKey) fingerprint(t *testing.T) string {
	t.Helper()
	pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(k.authorized))
	if err != nil {
		t.Fatal(err)
	}
	return gossh.FingerprintSHA256(pk)
}

// startProof performs step one (paste the public key) and reads the challenge
// back off the rendered page, unescaped - which is what a student copies, and
// what makes the page itself part of the contract.
func startProof(t *testing.T, h *Handler, c *http.Cookie, key testKey) challenge {
	t.Helper()
	rec := doForm(h, "/settings/keys", c, url.Values{"key": {key.authorized}})
	if rec.Code != http.StatusOK {
		t.Fatalf("challenge page: status %d, body %s", rec.Code, rec.Body.String())
	}
	body := html.UnescapeString(rec.Body.String())
	m := proofCmdRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no sign command on the page: %s", body)
	}
	nonce := nonceRE.FindString(m[1])
	if nonce == "" {
		t.Fatalf("the printed message carries no nonce: %q", m[1])
	}
	return challenge{nonce: nonce, message: m[1]}
}

// signMessage is what the student's `ssh-keygen -Y sign -n anygrade` produces.
func signMessage(t *testing.T, k testKey, message string) string {
	t.Helper()
	armored, err := sshsig.Sign(k.signer, keyProofNamespace, []byte(message))
	if err != nil {
		t.Fatal(err)
	}
	return string(armored)
}

// registerKey drives the whole two-step registration.
func registerKey(t *testing.T, h *Handler, c *http.Cookie, key testKey) *httptest.ResponseRecorder {
	t.Helper()
	ch := startProof(t, h, c, key)
	return doForm(h, "/settings/keys/verify", c, url.Values{
		"nonce": {ch.nonce}, "signature": {signMessage(t, key, ch.message)},
	})
}

// TestKeyRegistrationRequiresProof is the core of SPEC §8: pasting a public key
// registers nothing on its own. Public keys are public, so without a signature
// anyone could post a classmate's key, be handed the fingerprint, and lock that
// classmate out of their own SSH access.
func TestKeyRegistrationRequiresProof(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)
	u, session := newSession(t, h, "alice", "student")

	ch := startProof(t, h, session, key)
	keys, err := h.DB.ListSSHKeys(t.Context(), u.ID)
	if err != nil || len(keys) != 0 {
		t.Fatalf("step one registered a key without proof: %v (err %v)", keys, err)
	}
	// The signed line names the account and the key, so a student cannot be
	// talked into signing an opaque string that proves something else.
	if want := proofMessage(u.Login, key.fingerprint(t), ch.nonce); ch.message != want {
		t.Fatalf("printed message = %q, want %q", ch.message, want)
	}

	rec := doForm(h, "/settings/keys/verify", session, url.Values{
		"nonce": {ch.nonce}, "signature": {signMessage(t, key, ch.message)},
	})
	if rec.Header().Get("Location") != "/settings" {
		t.Fatalf("Location = %q, want /settings (body %s)", rec.Header().Get("Location"), rec.Body.String())
	}
	keys, err = h.DB.ListSSHKeys(t.Context(), u.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListSSHKeys = %v (err %v), want one key", keys, err)
	}
	if keys[0].Fingerprint != key.fingerprint(t) {
		t.Errorf("fingerprint = %q, want %q", keys[0].Fingerprint, key.fingerprint(t))
	}
	if keys[0].VerifiedAt == nil {
		t.Error("the key was stored without a proof timestamp")
	}
}

// TestKeyProofRejections: every way a proof can be wrong has to leave the key
// unregistered. Each of these is a distinct route back to first-come-first-
// served if it were accepted.
func TestKeyProofRejections(t *testing.T) {
	tests := []struct {
		name string
		// sign builds the posted signature; nonce is the live challenge.
		sign func(t *testing.T, own, other testKey, ch challenge) string
	}{
		{
			// Signed with a key the attacker really holds, submitted for the
			// victim's key: the SSHSIG carries its own public key.
			name: "wrong key",
			sign: func(t *testing.T, _, other testKey, ch challenge) string {
				return signMessage(t, other, ch.message)
			},
		},
		{
			// A signature over some other nonce, e.g. one captured earlier.
			name: "signature over a different nonce",
			sign: func(t *testing.T, own, _ testKey, ch challenge) string {
				return signMessage(t, own,
					strings.Replace(ch.message, ch.nonce, "agc_"+strings.Repeat("0", 64), 1))
			},
		},
		{
			// A real signature by the right key, made under another namespace -
			// git commit signing, say. Namespace is inside the signed bytes.
			name: "signature from another namespace",
			sign: func(t *testing.T, own, _ testKey, ch challenge) string {
				armored, err := sshsig.Sign(own.signer, "git", []byte(ch.message))
				if err != nil {
					t.Fatal(err)
				}
				return string(armored)
			},
		},
		{
			name: "not a signature at all",
			sign: func(t *testing.T, _, _ testKey, _ challenge) string { return "please let me in" },
		},
		{
			name: "no signature",
			sign: func(t *testing.T, _, _ testKey, _ challenge) string { return "" },
		},
		{
			// The phishing case the signed message exists to make visible: a
			// signature over the bare nonce, as a student would produce if they
			// were sent a random string to sign.
			name: "signature over the bare nonce",
			sign: func(t *testing.T, own, _ testKey, ch challenge) string {
				return signMessage(t, own, ch.nonce)
			},
		},
		{
			// And a proof made out for somebody else's account.
			name: "signature naming another account",
			sign: func(t *testing.T, own, _ testKey, ch challenge) string {
				return signMessage(t, own, proofMessage("mallory", own.fingerprint(t), ch.nonce))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestSite(t)
			own, other := newTestKey(t), newTestKey(t)
			u, session := newSession(t, h, "alice", "student")

			ch := startProof(t, h, session, own)
			rec := doForm(h, "/settings/keys/verify", session, url.Values{
				"nonce": {ch.nonce}, "signature": {tc.sign(t, own, other, ch)},
			})
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", rec.Code)
			}
			if keys, err := h.DB.ListSSHKeys(t.Context(), u.ID); err != nil || len(keys) != 0 {
				t.Fatalf("a bad proof registered a key: %v (err %v)", keys, err)
			}
			// The challenge survives a bad paste, so the student can retry.
			rec = doForm(h, "/settings/keys/verify", session, url.Values{
				"nonce": {ch.nonce}, "signature": {signMessage(t, own, ch.message)},
			})
			if rec.Header().Get("Location") != "/settings" {
				t.Fatalf("retry with a good signature failed: %q", rec.Header().Get("Location"))
			}
		})
	}
}

// TestKeyChallengeIsSingleUse: the nonce is a credential, so replaying a
// captured (nonce, signature) pair must not register the key a second time -
// nor register it on a second account.
func TestKeyChallengeIsSingleUse(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)
	u, session := newSession(t, h, "alice", "student")

	ch := startProof(t, h, session, key)
	form := url.Values{"nonce": {ch.nonce}, "signature": {signMessage(t, key, ch.message)}}

	if rec := doForm(h, "/settings/keys/verify", session, form); rec.Header().Get("Location") != "/settings" {
		t.Fatalf("first proof failed: %q", rec.Header().Get("Location"))
	}
	rec := doForm(h, "/settings/keys/verify", session, form)
	if want := "/settings?flash=key_challenge_expired"; rec.Header().Get("Location") != want {
		t.Fatalf("replay: Location = %q, want %q", rec.Header().Get("Location"), want)
	}
	if keys, err := h.DB.ListSSHKeys(t.Context(), u.ID); err != nil || len(keys) != 1 {
		t.Fatalf("replay changed the key list: %v (err %v)", keys, err)
	}
	if _, ok, err := h.DB.LookupKeyChallenge(t.Context(), ch.nonce, time.Now()); err != nil || ok {
		t.Fatalf("the nonce is still live after use: ok=%v err=%v", ok, err)
	}
}

// TestKeyChallengeExpires: a nonce read over somebody's shoulder is worthless
// once its window closes, even with a perfectly valid signature.
func TestKeyChallengeExpires(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)
	u, session := newSession(t, h, "alice", "student")

	// Write the challenge directly: the handler always issues a live one.
	nonce := "agc_" + strings.Repeat("a", 64)
	if err := h.DB.CreateKeyChallenge(t.Context(), u.ID, nonce, key.fingerprint(t),
		key.authorized, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	rec := doForm(h, "/settings/keys/verify", session, url.Values{
		"nonce":     {nonce},
		"signature": {signMessage(t, key, proofMessage(u.Login, key.fingerprint(t), nonce))},
	})
	if want := "/settings?flash=key_challenge_expired"; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
	if keys, err := h.DB.ListSSHKeys(t.Context(), u.ID); err != nil || len(keys) != 0 {
		t.Fatalf("an expired challenge registered a key: %v (err %v)", keys, err)
	}
}

// TestKeyChallengeBelongsToOneAccount: a leaked nonce plus its signature must
// not let a second account finish somebody else's registration. 404, not 403 -
// a student learns nothing about another account (SPEC §14).
func TestKeyChallengeBelongsToOneAccount(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)
	victim, victimSession := newSession(t, h, "alice", "student")
	mallory, mallorySession := newSession(t, h, "mallory", "student")

	ch := startProof(t, h, victimSession, key)
	rec := doForm(h, "/settings/keys/verify", mallorySession, url.Values{
		"nonce": {ch.nonce}, "signature": {signMessage(t, key, ch.message)},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	for _, id := range []int64{victim.ID, mallory.ID} {
		if keys, err := h.DB.ListSSHKeys(t.Context(), id); err != nil || len(keys) != 0 {
			t.Fatalf("user %d: keys = %v (err %v), want none", id, keys, err)
		}
	}
}

// TestDuplicateSSHKeyIsReported: with proof required, a refused fingerprint
// means the holder proved possession too, so the second claimant loses. The
// event still names both accounts, so a teacher can find the key and remove it
// from the holder's page.
func TestDuplicateSSHKeyIsReported(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)

	holder, holderSession := newSession(t, h, "mallory", "student")
	other, otherSession := newSession(t, h, "alice", "student")

	if rec := registerKey(t, h, holderSession, key); rec.Header().Get("Location") != "/settings" {
		t.Fatalf("first registration: %q", rec.Header().Get("Location"))
	}
	rec := registerKey(t, h, otherSession, key)
	if want := "/settings?flash=key_registered_elsewhere"; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
	if keys, err := h.DB.ListSSHKeys(t.Context(), other.ID); err != nil || len(keys) != 0 {
		t.Fatalf("the second claimant got the key: %v (err %v)", keys, err)
	}

	// The event is filed against the account that holds the key, which is the
	// page the teacher deletes it from.
	events, err := h.DB.ListEventsByTarget(t.Context(), holder.Login, 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "key.duplicate" {
			found = true
			if e.ActorLogin != other.Login || !strings.Contains(e.Detail, other.Login) {
				t.Errorf("event does not name the requester: actor=%q detail=%q", e.ActorLogin, e.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("no key.duplicate event against %q: %+v", holder.Login, events)
	}

	// The teacher sees the offending key on the holder's page and can remove it.
	_, teacher := newSession(t, h, "teacher", "teacher")
	page := do(h, http.MethodGet, "/students/"+holder.Login, teacher)
	keys, err := h.DB.ListSSHKeys(t.Context(), holder.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListSSHKeys: %v (%d keys)", err, len(keys))
	}
	// html/template escapes '+' and '=' in text content, so compare like for like.
	shown := strings.NewReplacer("+", "&#43;", "=", "&#61;").Replace(keys[0].Fingerprint)
	if !strings.Contains(page.Body.String(), shown) {
		t.Errorf("the teacher page does not show the key %q", keys[0].Fingerprint)
	}
	if want := "/students/" + holder.Login + "/keys/" + itoa(keys[0].ID) + "/delete"; !strings.Contains(page.Body.String(), want) {
		t.Errorf("the teacher page offers no way to remove the key (%s)", want)
	}
	del := doForm(h, "/students/"+holder.Login+"/keys/"+itoa(keys[0].ID)+"/delete",
		teacher, url.Values{"fingerprint": {keys[0].Fingerprint}})
	if del.Code != http.StatusSeeOther {
		t.Fatalf("delete: status %d", del.Code)
	}

	// With the key gone, the other account can register it.
	if rec := registerKey(t, h, otherSession, key); rec.Header().Get("Location") != "/settings" {
		t.Errorf("the key is still unregisterable: %q", rec.Header().Get("Location"))
	}
}

// TestOwnDuplicateSSHKeyIsNotAudited: re-proving your own key is a no-op, not
// an incident - no second row, no event.
func TestOwnDuplicateSSHKeyIsNotAudited(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)
	u, session := newSession(t, h, "alice", "student")

	registerKey(t, h, session, key)
	if rec := registerKey(t, h, session, key); rec.Header().Get("Location") != "/settings" {
		t.Fatalf("Location = %q, want /settings", rec.Header().Get("Location"))
	}
	keys, err := h.DB.ListSSHKeys(t.Context(), u.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListSSHKeys = %v (err %v), want exactly one key", keys, err)
	}
	events, err := h.DB.ListEventsByTarget(t.Context(), u.Login, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "key.duplicate" || e.Kind == "key.displaced" {
			t.Errorf("re-proving your own key was audited as an incident: %+v", e)
		}
	}
}

// TestProvenKeyDisplacesUnprovenSquatter is the deliberate change of who wins a
// contested fingerprint. Every unproven row is either a registration from
// before proof existed or one a teacher made out of band, and the only party
// who can sign is the one holding the private key - so the squat heals itself
// instead of waiting for a teacher to notice. The takeover is audited against
// the account that lost the key.
func TestProvenKeyDisplacesUnprovenSquatter(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)
	squatter, _ := newSession(t, h, "mallory", "student")
	victim, victimSession := newSession(t, h, "alice", "student")

	// The unproven registration path a teacher CLI leaves behind.
	if _, err := h.DB.AddSSHKey(t.Context(), squatter.ID, key.fingerprint(t), key.authorized); err != nil {
		t.Fatal(err)
	}

	if rec := registerKey(t, h, victimSession, key); rec.Header().Get("Location") != "/settings" {
		t.Fatalf("the rightful owner was refused: %q", rec.Header().Get("Location"))
	}
	if keys, err := h.DB.ListSSHKeys(t.Context(), squatter.ID); err != nil || len(keys) != 0 {
		t.Fatalf("the squatter kept the key: %v (err %v)", keys, err)
	}
	keys, err := h.DB.ListSSHKeys(t.Context(), victim.ID)
	if err != nil || len(keys) != 1 || keys[0].VerifiedAt == nil {
		t.Fatalf("the owner did not get a proven key: %v (err %v)", keys, err)
	}
	// SSH now resolves the fingerprint to the owner, which is the whole point.
	u, ok, err := h.DB.UserByFingerprint(t.Context(), key.fingerprint(t))
	if err != nil || !ok || u.ID != victim.ID {
		t.Fatalf("fingerprint resolves to %+v (ok=%v err=%v), want %q", u, ok, err, victim.Login)
	}

	events, err := h.DB.ListEventsByTarget(t.Context(), squatter.Login, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "key.displaced" && e.ActorLogin == victim.Login && strings.Contains(e.Detail, victim.Login) {
			return
		}
	}
	t.Fatalf("the takeover was not audited against %q: %+v", squatter.Login, events)
}

// TestProvenKeyDoesNotDisplaceProvenKey: a proof never beats another proof, or
// the flow would just be first-come-first-served with extra steps.
func TestProvenKeyDoesNotDisplaceProvenKey(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)
	holder, holderSession := newSession(t, h, "mallory", "student")
	other, otherSession := newSession(t, h, "alice", "student")

	registerKey(t, h, holderSession, key)
	registerKey(t, h, otherSession, key)

	if keys, err := h.DB.ListSSHKeys(t.Context(), holder.ID); err != nil || len(keys) != 1 {
		t.Fatalf("the proven holder lost the key: %v (err %v)", keys, err)
	}
	if keys, err := h.DB.ListSSHKeys(t.Context(), other.ID); err != nil || len(keys) != 0 {
		t.Fatalf("the second claimant got the key: %v (err %v)", keys, err)
	}
}

// TestProofUpgradesOwnUnprovenKey: a student whose key a teacher added, or who
// registered before proof existed, can mark it proven without deleting it -
// which is also what protects it from being taken over.
func TestProofUpgradesOwnUnprovenKey(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)
	u, session := newSession(t, h, "alice", "student")

	legacy, err := h.DB.AddSSHKey(t.Context(), u.ID, key.fingerprint(t), key.authorized)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.VerifiedAt != nil {
		t.Fatal("AddSSHKey must not claim a proof it never saw")
	}
	if rec := registerKey(t, h, session, key); rec.Header().Get("Location") != "/settings" {
		t.Fatalf("Location = %q, want /settings", rec.Header().Get("Location"))
	}
	keys, err := h.DB.ListSSHKeys(t.Context(), u.ID)
	if err != nil || len(keys) != 1 || keys[0].VerifiedAt == nil {
		t.Fatalf("ListSSHKeys = %v (err %v), want one proven key", keys, err)
	}
	if keys[0].ID != legacy.ID {
		t.Errorf("the key was replaced (id %d -> %d) instead of upgraded in place", legacy.ID, keys[0].ID)
	}
}

// TestLegacyKeysStillAuthenticate: keys registered before proof of possession
// existed are grandfathered, so an upgrade does not lock a running course out
// of SSH. They are flagged unproven in the UI instead.
func TestLegacyKeysStillAuthenticate(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)
	u, session := newSession(t, h, "alice", "student")
	if _, err := h.DB.AddSSHKey(t.Context(), u.ID, key.fingerprint(t), key.authorized); err != nil {
		t.Fatal(err)
	}

	got, ok, err := h.DB.UserByFingerprint(t.Context(), key.fingerprint(t))
	if err != nil || !ok || got.ID != u.ID {
		t.Fatalf("legacy key no longer authenticates: %+v ok=%v err=%v", got, ok, err)
	}
	page := do(h, http.MethodGet, "/settings", session)
	if !strings.Contains(page.Body.String(), "unproven") {
		t.Errorf("settings does not flag the unproven key:\n%s", page.Body.String())
	}
	_, teacher := newSession(t, h, "teacher", "teacher")
	student := do(h, http.MethodGet, "/students/"+u.Login, teacher)
	if !strings.Contains(student.Body.String(), "unproven") {
		t.Errorf("the teacher page does not flag the unproven key:\n%s", student.Body.String())
	}
}

// TestUnparseableKeyIssuesNoChallenge: step one still catches a typo before any
// state is written.
func TestUnparseableKeyIssuesNoChallenge(t *testing.T) {
	h, _ := newTestSite(t)
	_, session := newSession(t, h, "alice", "student")
	rec := doForm(h, "/settings/keys", session, url.Values{"key": {"not-a-key"}})
	if want := "/settings?flash=unparseable_ssh_key"; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
}

// TestDuplicateSSHKeyReportsDisabledHolder is the case a state-filtered lookup
// silently dropped: the holder's account is disabled, so the second claimant is
// still refused while nothing at all reaches the teacher.
func TestDuplicateSSHKeyReportsDisabledHolder(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)

	holder, holderSession := newSession(t, h, "mallory", "student")
	other, otherSession := newSession(t, h, "alice", "student")

	if rec := registerKey(t, h, holderSession, key); rec.Header().Get("Location") != "/settings" {
		t.Fatalf("first registration: %q", rec.Header().Get("Location"))
	}
	if err := h.DB.SetUserState(t.Context(), holder.Login, "disabled"); err != nil {
		t.Fatal(err)
	}

	rec := registerKey(t, h, otherSession, key)
	if want := "/settings?flash=key_registered_elsewhere"; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
	events, err := h.DB.ListEventsByTarget(t.Context(), holder.Login, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "key.duplicate" && strings.Contains(e.Detail, other.Login) {
			return
		}
	}
	t.Fatalf("no key.duplicate event against the disabled holder %q: %+v", holder.Login, events)
}

// TestLocalModeCanRegisterAKey: `serve --local` has one implicit user and no
// login, and the proof flow must still work on that path.
func TestLocalModeCanRegisterAKey(t *testing.T) {
	h, local := newTestSite(t)
	h.Local = &local
	key := newTestKey(t)

	req := httptest.NewRequest(http.MethodPost, "/settings/keys",
		strings.NewReader(url.Values{"key": {key.authorized}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)
	m := proofCmdRE.FindStringSubmatch(html.UnescapeString(rec.Body.String()))
	if m == nil {
		t.Fatalf("no challenge in local mode: %s", rec.Body.String())
	}
	nonce := nonceRE.FindString(m[1])

	req = httptest.NewRequest(http.MethodPost, "/settings/keys/verify",
		strings.NewReader(url.Values{"nonce": {nonce}, "signature": {signMessage(t, key, m[1])}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	New(h).ServeHTTP(rec, req)
	if rec.Header().Get("Location") != "/settings" {
		t.Fatalf("Location = %q, want /settings", rec.Header().Get("Location"))
	}
	if keys, err := h.DB.ListSSHKeys(t.Context(), local.ID); err != nil || len(keys) != 1 {
		t.Fatalf("ListSSHKeys = %v (err %v), want one key", keys, err)
	}
}

// TestPastedKeyIsCanonicalized: ParseAuthorizedKey reads the first line and
// ignores whatever follows, and it accepts authorized_keys options in front of
// the key - so the raw paste is unbounded attacker-controlled text that only
// looks like the key its fingerprint came from. Only the re-marshalled key is
// stored.
func TestPastedKeyIsCanonicalized(t *testing.T) {
	h, _ := newTestSite(t)
	key := newTestKey(t)
	u, session := newSession(t, h, "alice", "student")

	junk := `command="rm -rf /" ` + key.authorized + "\nnot a key at all\n" + strings.Repeat("x", 5000)
	rec := doForm(h, "/settings/keys", session, url.Values{"key": {junk}})
	if rec.Code != http.StatusOK {
		t.Fatalf("challenge page: status %d", rec.Code)
	}
	body := html.UnescapeString(rec.Body.String())
	m := proofCmdRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no sign command on the page: %s", body)
	}
	if rec := doForm(h, "/settings/keys/verify", session, url.Values{
		"nonce": {nonceRE.FindString(m[1])}, "signature": {signMessage(t, key, m[1])},
	}); rec.Header().Get("Location") != "/settings" {
		t.Fatalf("Location = %q, want /settings", rec.Header().Get("Location"))
	}

	keys, err := h.DB.ListSSHKeys(t.Context(), u.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListSSHKeys = %v (err %v), want one key", keys, err)
	}
	if keys[0].PublicKey != key.authorized {
		t.Errorf("stored public_key = %q, want the canonical line %q", keys[0].PublicKey, key.authorized)
	}
}
