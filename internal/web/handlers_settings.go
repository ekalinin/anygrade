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
		http.Error(w, "token reset failed", http.StatusInternalServerError)
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
	if _, err := h.DB.AddSSHKey(r.Context(), u.ID, gossh.FingerprintSHA256(pk), keyText); err != nil {
		http.Redirect(w, r, "/settings?flash=key_already_registered", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (h *Handler) delOwnKey(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Scoped delete: a forged id cannot touch another user's key.
	if err := h.DB.DeleteSSHKey(r.Context(), u.ID, id); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
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
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	anonymize := lb.Anonymize && u.Role != "teacher"
	rows := make([]leaderboardRow, 0)
	for _, lr := range gradebook.Leaderboard(m) {
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
