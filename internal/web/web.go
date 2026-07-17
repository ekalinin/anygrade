// Package web is the SSR UI (SPEC §10): html/template + htmx + SSE,
// everything embedded. It talks to the rest of the system only through the
// injected seams below; it never imports gitserver.
package web

import (
	"context"
	"net/http"

	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/ratelimit"
	"github.com/ekalinin/anygrade/internal/store"
)

// Rechecker is the intake surface web needs (satisfied by *intake.Server).
type Rechecker interface {
	Recheck(ctx context.Context, userID int64, taskID string) (store.Submission, queue.Decision, error)
	TeacherRecheck(ctx context.Context, actor store.User, targetUserID int64, taskID string) (store.Submission, error)
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
	DataDir          string
	BaseURL          string // git clone/upstream links on activation pages
	SSHAddr          string // git SSH listen address; "" hides SSH clone hints
	// Limit, when non-nil, throttles failed logins (shared with git basic
	// auth by the composition root).
	Limit *ratelimit.Limiter
}

// New builds the site mux. Everything except /login, /invite, /register, and
// /static/ requires a session; teacher routes add a role check (SPEC §14).
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
	mux.Handle("GET /submissions/{id}/logs/{check}", h.requireAuth(h.submissionLog))
	mux.Handle("GET /leaderboard", h.requireAuth(h.leaderboardPage))
	mux.Handle("GET /settings", h.requireAuth(h.settingsPage))
	mux.Handle("POST /settings/token", h.requireAuth(h.regenToken))
	mux.Handle("POST /settings/keys", h.requireAuth(h.addOwnKey))
	mux.Handle("POST /settings/keys/{id}/delete", h.requireAuth(h.delOwnKey))

	// Teacher only.
	mux.Handle("GET /matrix", h.requireTeacher(h.matrixPage))
	mux.Handle("GET /matrix/stream", h.requireTeacher(h.matrixStream))
	mux.Handle("GET /export/scores.csv", h.requireTeacher(h.exportCSV))
	mux.Handle("GET /queue", h.requireTeacher(h.queuePage))
	mux.Handle("GET /queue/stream", h.requireTeacher(h.queueStream))
	mux.Handle("POST /queue/{id}/cancel", h.requireTeacher(h.cancelSubmission))
	mux.Handle("GET /students", h.requireTeacher(h.studentsPage))
	mux.Handle("GET /students/{login}", h.requireTeacher(h.studentPage))
	mux.Handle("POST /students/{login}/tasks/{id}/recheck", h.requireTeacher(h.teacherRecheck))
	mux.Handle("POST /students/{login}/tasks/{id}/override", h.requireTeacher(h.setOverride))
	mux.Handle("POST /students/{login}/tasks/{id}/override/delete", h.requireTeacher(h.clearOverride))
	mux.Handle("POST /students/{login}/token/reset", h.requireTeacher(h.adminResetToken))
	mux.Handle("POST /students/{login}/state", h.requireTeacher(h.adminSetState))
	mux.Handle("GET /students/{login}/submissions/{id}/code", h.requireTeacher(h.codeList))
	mux.Handle("GET /students/{login}/submissions/{id}/code/{path...}", h.requireTeacher(h.codeFile))
	mux.Handle("GET /audit", h.requireTeacher(h.auditPage))
	return mux
}

// requireTeacher layers the role check over requireAuth (SPEC §14).
func (h *Handler) requireTeacher(next http.HandlerFunc) http.Handler {
	return h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if user(r).Role != "teacher" {
			http.NotFound(w, r) // do not leak the route's existence
			return
		}
		next(w, r)
	})
}

// canSee is the per-object access rule: own data, or teacher (SPEC §14).
func canSee(u store.User, ownerID int64) bool {
	return u.ID == ownerID || u.Role == "teacher"
}
