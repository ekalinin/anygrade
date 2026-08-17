package web

import (
	"net/http"
	"strconv"
	"strings"

	gossh "golang.org/x/crypto/ssh"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/store"
)

type settingsData struct {
	CourseName string
	User       userView
	Keys       []store.SSHKey
	Flash      string
}

func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	keys, _ := h.DB.ListSSHKeys(r.Context(), u.ID)
	h.renderPage(w, r, "settings", settingsData{
		CourseName: h.Course.Get().Resolved.Course.Name,
		User:       h.userViewOf(u),
		Keys:       keys,
		Flash:      r.URL.Query().Get("flash"),
	})
}

// regenToken rotates the personal token and re-binds THIS session to it:
// every other session dies (its token_hash no longer joins), the current
// tab stays logged in, and the new token is shown exactly once.
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

func (h *Handler) addOwnKey(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	keyText := strings.TrimSpace(r.FormValue("key"))
	pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(keyText))
	if err != nil {
		http.Redirect(w, r, "/settings?flash=unparseable_ssh_key", http.StatusSeeOther)
		return
	}
	fingerprint := gossh.FingerprintSHA256(pk)
	if _, err := h.DB.AddSSHKey(r.Context(), u.ID, fingerprint, keyText); err != nil {
		http.Redirect(w, r, "/settings?flash="+h.reportDuplicateKey(r, u, fingerprint), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// reportDuplicateKey turns a rejected duplicate fingerprint into a flash code,
// and records an audit event when the key belongs to somebody else.
//
// Public keys are public and ssh_keys.fingerprint is globally unique, so
// registering a key is first-come-first-served: an attacker who posts a
// classmate's public key locks that classmate out of their own key and makes
// their SSH connections resolve to the attacker's identity - denial of service,
// not stolen access, since the attacker still cannot use the private key.
// Proof of possession (sign a server challenge) is the real fix and is left as
// follow-up work; until then the event names both accounts, so a teacher can
// see who holds the key and remove it from that student's page.
func (h *Handler) reportDuplicateKey(r *http.Request, actor store.User, fingerprint string) string {
	holder, ok, err := h.DB.UserByFingerprint(r.Context(), fingerprint)
	if err != nil || !ok || holder.ID == actor.ID {
		// Own key re-added, or the holder is deactivated and no longer
		// resolvable: nothing to report beyond the plain duplicate.
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
	anonymize := lb.Anonymize && u.Role != "teacher"
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
