// Package web is the SSR student UI (SPEC §10): html/template + htmx + SSE,
// everything embedded. It talks to the rest of the system only through the
// injected seams below; it never imports gitserver.
package web

import (
	"context"
	"net/http"

	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/store"
)

// Rechecker is the one intake method web needs (satisfied by *intake.Server).
type Rechecker interface {
	Recheck(ctx context.Context, userID int64, taskID string) (store.Submission, queue.Decision, error)
}

// Handler bundles the web layer's dependencies; New turns it into the site.
type Handler struct {
	DB      store.Store
	Course  *intake.Holder
	Hub     *Hub
	Recheck Rechecker
	// ReadCourseFile reads one repo-relative file at a course commit
	// (app injects a GitSource-backed impl; ok=false = file absent).
	ReadCourseFile func(ctx context.Context, commit, relPath string) ([]byte, bool, error)
	DataDir        string
}

// New builds the site mux. Everything except /login and /static/ requires a
// session; role checks are per-handler (SPEC §14).
func New(h *Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", h.loginForm)
	mux.HandleFunc("POST /login", h.loginSubmit)
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))

	mux.Handle("POST /logout", h.requireAuth(h.logout))
	mux.Handle("GET /{$}", h.requireAuth(h.dashboard))
	mux.Handle("GET /dashboard/stream", h.requireAuth(h.dashboardStream))
	mux.Handle("GET /tasks/{id}", h.requireAuth(h.taskPage))
	mux.Handle("POST /tasks/{id}/recheck", h.requireAuth(h.taskRecheck))
	mux.Handle("GET /submissions/{id}", h.requireAuth(h.submissionPage))
	mux.Handle("GET /submissions/{id}/fragment", h.requireAuth(h.submissionFragment))
	mux.Handle("GET /submissions/{id}/stream", h.requireAuth(h.submissionStream))
	mux.Handle("GET /submissions/{id}/logs/{check}", h.requireAuth(h.submissionLog))
	return mux
}

// canSee is the per-object access rule: own data, or teacher (SPEC §14).
func canSee(u store.User, ownerID int64) bool {
	return u.ID == ownerID || u.Role == "teacher"
}
