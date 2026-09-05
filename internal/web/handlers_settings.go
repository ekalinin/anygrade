package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/sshsig"
	"github.com/ekalinin/anygrade/internal/store"
)

type settingsData struct {
	CourseName string
	User       userView
	Keys       []store.SSHKey
	Flash      string
	// HasToken decides whether the token section offers a rotation or a first
	// issue. An account that only ever signed in through the identity provider
	// has no token and cannot push over HTTP until it asks for one (SPEC §8),
	// so the page has to say something different to it than "regenerate".
	HasToken bool
}

func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	keys, _ := h.DB.ListSSHKeys(r.Context(), u.ID)
	// A read error shows the rotation wording, which is the safe default: it
	// works whether or not a token exists, where "create one" would be wrong.
	hasToken, err := h.DB.HasToken(r.Context(), u.ID)
	if err != nil {
		hasToken = true
	}
	h.renderPage(w, r, "settings", settingsData{
		CourseName: h.Course.Get().Resolved.Course.Name,
		User:       h.userViewOf(u),
		Keys:       keys,
		Flash:      r.URL.Query().Get("flash"),
		HasToken:   hasToken,
	})
}

// regenToken issues the personal token and re-binds THIS session to it: every
// other token-bound session dies (its token_hash no longer joins), the current
// tab stays logged in, and the new token is shown exactly once.
//
// It is one handler for two jobs, because they are the same write. For an
// account that already has a token this is a rotation, and the old token stops
// working the moment this runs - which is what the page warns about, since a
// git remote that saved it keeps sending the old one. For an account that came
// in through the identity provider and never had one, it is the first issue:
// the provider login gives a session, and only this gives something git can use
// as a password.
func (h *Handler) regenToken(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	token, err := h.DB.IssueToken(r.Context(), u.ID)
	if err != nil {
		h.httpError(w, r, "error.token_reset_failed", http.StatusInternalServerError)
		return
	}
	// Show the token even if the re-bind below fails: it is shown only once.
	if c, cerr := r.Cookie(sessionCookie); cerr == nil {
		_ = h.DB.DeleteSession(r.Context(), c.Value)
	}
	if sid, serr := h.DB.CreateSession(r.Context(), u.ID, token, sessionTTL); serr == nil {
		setSessionCookie(w, r, sid, sessionTTL)
	}
	h.renderTokenOnce(w, r, u.Login, token, false)
}

// keyProofNamespace is the SSHSIG namespace students sign the challenge under.
// It is part of the signed bytes, so a signature the student legitimately made
// for something else - git commit signing uses "git", signing a file uses
// "file" - can never be replayed here as a proof of possession.
const keyProofNamespace = "anygrade"

// keyChallengeTTL bounds how long a nonce stays usable. Long enough to switch
// to a terminal, find the key and paste the output back; short enough that a
// nonce read over someone's shoulder is worthless by the time it is used.
//
// The duration is quoted to the student by keyproof.expiry_hint in every
// locale catalog; change it here and there together.
const keyChallengeTTL = 10 * time.Minute

type keyChallengeData struct {
	CourseName  string
	User        userView
	Nonce       string
	Fingerprint string
	Namespace   string
	// Message is the exact line the student signs, printed on the page.
	Message string
	Error   string
}

// proofMessage is what the student's key actually signs. It is rebuilt from
// the session's login and the stored challenge at verification time, so it
// never has to be persisted.
//
// The nonce alone would be enough to make the proof fresh and single-use, and
// it would also make the proof transferable: anybody may open a challenge for
// anybody's public key, since public keys are public, so a signature over an
// opaque random string is a signature over "whatever the person who asked for
// it wanted". A student talked into running the command from a classmate's
// screen would hand that classmate a proven claim on their own key - a worse
// outcome than the squatting this flow exists to stop, because a proof is
// never displaced. Naming the account and the key inside the signed bytes
// makes that request self-incriminating: the line reads user=<someone else>.
//
// Logins are `[a-z0-9][a-z0-9._-]*` (internal/ident) and a fingerprint is
// base64, so neither can carry a quote out of the shell command on the page.
func proofMessage(login, fingerprint, nonce string) string {
	return "anygrade-key-proof/v1 user=" + login + " key=" + fingerprint + " nonce=" + nonce
}

// newChallengeNonce returns a fresh "agc_"-prefixed random nonce. The prefix
// is only a label: the nonce ends up in a shell command and in the student's
// history, and it should be recognizable as an anygrade challenge rather than
// mistaken for a token.
func newChallengeNonce() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "agc_" + hex.EncodeToString(buf), nil
}

// addOwnKey is step one of registering a key: the student pastes the public
// half and gets a one-shot nonce to sign with the private half (SPEC §8).
//
// Nothing is written to ssh_keys here, and the duplicate check deliberately
// does not happen here either: who wins a contested fingerprint depends on
// whether the current holder ever proved possession, and that is decided in
// one transaction at the end, in proveOwnKey.
func (h *Handler) addOwnKey(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	keyText := strings.TrimSpace(r.FormValue("key"))
	pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(keyText))
	if err != nil {
		http.Redirect(w, r, "/settings?flash=unparseable_ssh_key", http.StatusSeeOther)
		return
	}
	nonce, err := newChallengeNonce()
	if err != nil {
		h.httpError(w, r, "error.save_failed", http.StatusInternalServerError)
		return
	}
	fingerprint := gossh.FingerprintSHA256(pk)
	// Store the parsed key re-marshalled, not the pasted text.
	// ParseAuthorizedKey reads the first line and ignores everything after it,
	// and it accepts authorized_keys options in front of the key - so the raw
	// paste is unbounded attacker-controlled text that only looks like the key
	// its fingerprint was taken from.
	keyText = strings.TrimSpace(string(gossh.MarshalAuthorizedKey(pk)))
	// The key is stored with the challenge rather than round-tripped through a
	// hidden form field: step two then verifies the signature against the key
	// the nonce was issued for, and a student who edits the form on the way
	// back cannot register a key nobody signed for.
	if err := h.DB.CreateKeyChallenge(r.Context(), u.ID, nonce, fingerprint, keyText,
		time.Now().Add(keyChallengeTTL)); err != nil {
		h.httpError(w, r, "error.save_failed", http.StatusInternalServerError)
		return
	}
	h.renderPage(w, r, "key_challenge", keyChallengeData{
		CourseName:  h.Course.Get().Resolved.Course.Name,
		User:        h.userViewOf(u),
		Nonce:       nonce,
		Fingerprint: fingerprint,
		Namespace:   keyProofNamespace,
		Message:     proofMessage(u.Login, fingerprint, nonce),
	})
}

// proveOwnKey is step two: the signature over the nonce is what registers the
// key (SPEC §8).
func (h *Handler) proveOwnKey(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	nonce := strings.TrimSpace(r.FormValue("nonce"))
	c, ok, err := h.DB.LookupKeyChallenge(r.Context(), nonce, time.Now())
	if err != nil {
		h.httpError(w, r, "error.save_failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		// Unknown, already used, or expired - all the same to the student, and
		// all recoverable by pasting the key again.
		http.Redirect(w, r, "/settings?flash=key_challenge_expired", http.StatusSeeOther)
		return
	}
	if c.UserID != u.ID {
		// A nonce belongs to one account. 404, not 403: a student learns
		// nothing about somebody else's pending registration (SPEC §14).
		http.NotFound(w, r)
		return
	}
	pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(c.PublicKey))
	if err != nil {
		h.httpError(w, r, "error.save_failed", http.StatusInternalServerError)
		return
	}
	message := proofMessage(u.Login, c.Fingerprint, nonce)
	// Verify before consuming: a mistyped paste should not cost the student the
	// challenge, and the nonce is single-use by the delete below anyway.
	if err := sshsig.Verify(pk, keyProofNamespace, []byte(message), []byte(r.FormValue("signature"))); err != nil {
		// The student is told only that the proof failed; the reason is the
		// teacher's, and it is the difference between a fumbled paste and
		// somebody trying to register a key they do not hold.
		slog.Warn("ssh key proof rejected", "login", u.Login, "fingerprint", c.Fingerprint, "err", err)
		w.WriteHeader(http.StatusUnprocessableEntity)
		h.renderPage(w, r, "key_challenge", keyChallengeData{
			CourseName:  h.Course.Get().Resolved.Course.Name,
			User:        h.userViewOf(u),
			Nonce:       nonce,
			Fingerprint: c.Fingerprint,
			Namespace:   keyProofNamespace,
			Message:     message,
			Error:       "key_proof_failed",
		})
		return
	}
	// Burn the nonce before the key is registered. Lookup only proves it was
	// live when it was read, so two replays of one captured signature would
	// otherwise both go through. Losing the process between here and the insert
	// costs the student nothing but pasting the key again.
	if used, err := h.DB.ConsumeKeyChallenge(r.Context(), nonce); err != nil || !used {
		http.Redirect(w, r, "/settings?flash=key_challenge_expired", http.StatusSeeOther)
		return
	}
	_, displaced, err := h.DB.AddProvenSSHKey(r.Context(), u.ID, c.Fingerprint, c.PublicKey)
	switch {
	case errors.Is(err, store.ErrKeyHeld):
		http.Redirect(w, r, "/settings?flash="+h.reportDuplicateKey(r, u, c.Fingerprint), http.StatusSeeOther)
		return
	case err != nil:
		h.httpError(w, r, "error.save_failed", http.StatusInternalServerError)
		return
	}
	if displaced != nil {
		// The takeover is audited, never silent: the losing account may be a
		// squatter or may be a student whose legacy key this genuinely was, and
		// only a teacher can tell the two apart afterwards.
		_ = h.DB.Log(r.Context(), store.Event{
			ActorID: &u.ID, Kind: "key.displaced", Target: displaced.Login,
			Detail: "unproven key taken over by " + u.Login +
				" after proof of possession, fingerprint " + c.Fingerprint,
		})
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// reportDuplicateKey turns a rejected duplicate fingerprint into a flash code,
// and records an audit event when the key belongs to somebody else.
//
// Since keys are registered only against a signed challenge, a refusal here
// means the holder proved possession too - so either the student is trying to
// register a classmate's key, or two people share one private key. Either way
// the event names both accounts, so a teacher can see who holds the key and
// remove it from that student's page.
func (h *Handler) reportDuplicateKey(r *http.Request, actor store.User, fingerprint string) string {
	// KeyHolder, not UserByFingerprint: a squatter whose account was disabled
	// since still holds the fingerprint, and that is the case a teacher most
	// needs told - the victim is refused their own key with nothing on record.
	holder, ok, err := h.DB.KeyHolder(r.Context(), fingerprint)
	if err != nil || !ok || holder.ID == actor.ID {
		// Not a squat, or the holder could not be read: say the least alarming
		// true thing rather than accuse somebody the lookup never identified.
		// Re-proving your own key does not reach here at all - it upgrades the
		// existing row - so this branch is the fallback, not the common case.
		return "key_already_registered"
	}
	// Target is the holder: the teacher UI lists a student's keys with a delete
	// button on their own page, which is where this has to lead.
	_ = h.DB.Log(r.Context(), store.Event{
		ActorID: &actor.ID, Kind: "key.duplicate", Target: holder.Login,
		Detail: "requested by " + actor.Login + ", fingerprint " + fingerprint,
	})
	return "key_registered_elsewhere"
}

func (h *Handler) delOwnKey(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Scoped delete: a forged id cannot touch another user's key. The
	// fingerprint pins it to the key the page actually showed, so a rowid
	// reused by a key added meanwhile is not removed by a stale form.
	ok, err := h.DB.DeleteSSHKey(r.Context(), u.ID, id, r.FormValue("fingerprint"))
	if err != nil {
		h.httpError(w, r, "error.delete_failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

type leaderboardData struct {
	CourseName string
	User       userView
	Rows       []leaderboardRow
	Anonymize  bool
}

type leaderboardRow struct {
	Rank  int
	Name  string
	Total string
	Self  bool
}

func (h *Handler) leaderboardPage(w http.ResponseWriter, r *http.Request) {
	course := h.Course.Get()
	lb := course.Resolved.Course.Leaderboard
	if !lb.Enabled {
		http.NotFound(w, r)
		return
	}
	u := user(r)
	m, err := h.buildMatrix(r)
	if err != nil {
		h.httpError(w, r, "error.load_failed", http.StatusInternalServerError)
		return
	}
	// Anonymization keeps students from ranking each other; staff who may open
	// any submission lose nothing by being sent to the matrix for the same
	// names, so it is off for every reviewer (SPEC §10).
	anonymize := lb.Anonymize && !u.CanReview()
	rows := make([]leaderboardRow, 0)
	for _, lr := range gradebook.Leaderboard(m, h.Alias) {
		name := lr.Login
		if anonymize {
			name = lr.Alias
		}
		rows = append(rows, leaderboardRow{
			Rank: lr.Rank, Name: name,
			Total: gradebook.FmtScore(lr.Total),
			Self:  lr.Login == u.Login,
		})
	}
	h.renderPage(w, r, "leaderboard", leaderboardData{
		CourseName: course.Resolved.Course.Name,
		User:       h.userViewOf(u),
		Rows:       rows,
		Anonymize:  anonymize,
	})
}
