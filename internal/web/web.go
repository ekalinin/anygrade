// Package web is the SSR UI (SPEC §10): html/template + htmx + SSE,
// everything embedded. It talks to the rest of the system only through the
// injected seams below; it never imports gitserver.
package web

import (
	"context"
	"net/http"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/ratelimit"
	"github.com/ekalinin/anygrade/internal/store"
)

// Rechecker is the intake surface web needs (satisfied by *intake.Server).
type Rechecker interface {
	Recheck(ctx context.Context, userID int64, taskID string) (store.Submission, queue.Decision, intake.RecheckWarning, error)
	TeacherRecheck(ctx context.Context, actor store.User, targetUserID int64, taskID string) (store.Submission, intake.RecheckWarning, error)
}

// Canceler aborts queued/running submissions (satisfied by *queue.Queue).
type Canceler interface {
	Cancel(ctx context.Context, id int64) (bool, error)
}

// Handler bundles the web layer's dependencies; New turns it into the site.
type Handler struct {
	DB      store.Store
	Course  *intake.Holder
	Hub     *Hub
	Recheck Rechecker
	Cancel  Canceler
	// ReadCourseFile reads one repo-relative file at a course commit
	// (app injects a GitSource-backed impl; ok=false = file absent).
	ReadCourseFile func(ctx context.Context, commit, relPath string) ([]byte, bool, error)
	// ListStudentFiles / ReadStudentFile expose a student's submitted commit
	// for the teacher code view (GitSource-backed, injected by app).
	ListStudentFiles func(ctx context.Context, login, commit, relDir string) ([]string, error)
	ReadStudentFile  func(ctx context.Context, login, commit, relPath string) ([]byte, bool, error)
	// EnsureRepo provisions the student's personal repo at account activation
	// (SPEC §7; RepoManager-backed, injected by app). Nil is safe and so is a
	// returned error: the git transports still create the repo on first
	// access, so this only decides whether the clone URL works right away.
	EnsureRepo func(ctx context.Context, login string) error
	DataDir    string
	BaseURL    string // git clone/upstream links on activation pages
	SSHAddr    string // git SSH listen address; "" hides SSH clone hints
	// Limit, when non-nil, throttles failed logins (shared with git basic
	// auth by the composition root).
	Limit *ratelimit.Limiter
	// Local, when non-nil, disables authentication and serves every request
	// as this user (serve --local; the caller guarantees a loopback bind).
	// The zero value is the secure one: only the composition root sets it.
	Local *store.User
	// BehindProxy makes the site trust X-Forwarded-Proto when deciding whether
	// the browser's connection is encrypted (session cookie Secure flag). Off
	// by default: the header is forgeable by anyone who reaches the port.
	BehindProxy bool
	// Alias anonymizes leaderboard names for students (SPEC §10). The secret
	// behind it is per instance and comes from the composition root; the zero
	// value derives guessable aliases and is test-only.
	Alias gradebook.Aliaser
}

// New builds the site mux. Everything except /login, /invite, /register, and
// /static/ requires a session; staff routes add a rights check (SPEC §14).
func New(h *Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", h.loginForm)
	mux.HandleFunc("POST /login", h.loginSubmit)
	mux.HandleFunc("GET /invite/{token}", h.invitePage)
	mux.HandleFunc("POST /invite/{token}", h.inviteSubmit)
	mux.HandleFunc("GET /register", h.registerPage)
	mux.HandleFunc("POST /register", h.registerSubmit)
	mux.HandleFunc("POST /lang", h.setLang)
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))

	// Authenticated (any role).
	mux.Handle("POST /logout", h.requireAuth(h.logout))
	mux.Handle("GET /{$}", h.requireAuth(h.dashboard))
	mux.Handle("GET /dashboard/stream", h.requireAuth(h.dashboardStream))
	mux.Handle("GET /tasks/{id}", h.requireAuth(h.taskPage))
	mux.Handle("POST /tasks/{id}/recheck", h.requireAuth(h.taskRecheck))
	mux.Handle("GET /submissions/{id}", h.requireAuth(h.submissionPage))
	mux.Handle("GET /submissions/{id}/fragment", h.requireAuth(h.submissionFragment))
	mux.Handle("GET /submissions/{id}/stream", h.requireAuth(h.submissionStream))
	// {check...}: a check name may contain '/' (metadata only forbids empty and
	// duplicate names), which a single-segment wildcard could never match.
	mux.Handle("GET /submissions/{id}/logs/{check...}", h.requireAuth(h.submissionLog))
	mux.Handle("GET /leaderboard", h.requireAuth(h.leaderboardPage))
	mux.Handle("GET /settings", h.requireAuth(h.settingsPage))
	mux.Handle("POST /settings/token", h.requireAuth(h.regenToken))
	mux.Handle("POST /settings/keys", h.requireAuth(h.addOwnKey))
	mux.Handle("POST /settings/keys/verify", h.requireAuth(h.proveOwnKey))
	mux.Handle("POST /settings/keys/{id}/delete", h.requireAuth(h.delOwnKey))

	// Reviewing other people's work: teachers and TAs (store.User.CanReview).
	mux.Handle("GET /matrix", h.requireReview(h.matrixPage))
	mux.Handle("GET /matrix/stream", h.requireReview(h.matrixStream))
	mux.Handle("GET /export/scores.csv", h.requireReview(h.exportCSV))
	mux.Handle("GET /queue", h.requireReview(h.queuePage))
	mux.Handle("GET /queue/stream", h.requireReview(h.queueStream))
	mux.Handle("POST /queue/{id}/cancel", h.requireReview(h.cancelSubmission))
	mux.Handle("POST /queue/{id}/recheck", h.requireReview(h.recheckSubmission))
	mux.Handle("GET /students", h.requireReview(h.studentsPage))
	mux.Handle("GET /students/{login}", h.requireReview(h.studentPage))
	mux.Handle("POST /students/{login}/tasks/{id}/recheck", h.requireReview(h.teacherRecheck))
	mux.Handle("GET /students/{login}/submissions/{id}/code", h.requireReview(h.codeList))
	mux.Handle("GET /students/{login}/submissions/{id}/code/{path...}", h.requireReview(h.codeFile))

	// Changing the record: teachers only (store.User.CanAdminister).
	mux.Handle("POST /students/{login}/tasks/{id}/override", h.requireTeacher(h.setOverride))
	mux.Handle("POST /students/{login}/tasks/{id}/override/delete", h.requireTeacher(h.clearOverride))
	mux.Handle("POST /students/{login}/token/reset", h.requireTeacher(h.adminResetToken))
	mux.Handle("POST /students/{login}/state", h.requireTeacher(h.adminSetState))
	mux.Handle("POST /students/{login}/keys/{id}/delete", h.requireTeacher(h.adminDeleteKey))
	mux.Handle("GET /audit", h.requireTeacher(h.auditPage))
	return h.secureContext(mux)
}

// requireReview and requireTeacher are the two rights of the role table
// (store/roles.go) as middleware; a route is gated by the right it needs, not
// by the roles that happen to hold it today.
func (h *Handler) requireReview(next http.HandlerFunc) http.Handler {
	return h.requireRight(store.User.CanReview, next)
}

func (h *Handler) requireTeacher(next http.HandlerFunc) http.Handler {
	return h.requireRight(store.User.CanAdminister, next)
}

// requireRight layers one right over requireAuth (SPEC §14). A refusal is a
// 404 for a TA exactly as it is for a student: the answer must not tell the
// caller that the route exists and they are merely not allowed on it.
func (h *Handler) requireRight(has func(store.User) bool, next http.HandlerFunc) http.Handler {
	return h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if !has(user(r)) {
			http.NotFound(w, r) // do not leak the route's existence
			return
		}
		next(w, r)
	})
}

// canSee is the per-object access rule: own data, or a reviewer (SPEC §14).
func canSee(u store.User, ownerID int64) bool {
	return u.ID == ownerID || u.CanReview()
}
