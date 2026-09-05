package web

import (
	"net/http"
	"time"

	"github.com/ekalinin/anygrade/internal/store"
)

type dashboardData struct {
	CourseName string
	User       userView
	Tasks      []TaskView
	Now        time.Time
}

type userView struct {
	Login       string
	DisplayName string
	Role        string
	// Review and Admin mirror the role table (store/roles.go) for the
	// templates: the nav and the admin controls are drawn from the same two
	// rights the routes are gated by, so a button never leads to a 404.
	Review bool
	Admin  bool
	// Local marks `serve --local`: there is no session, so the header hides
	// the log out button.
	Local bool
}

// userViewOf builds the header identity every page carries.
func (h *Handler) userViewOf(u store.User) userView {
	return userView{
		Login:       u.Login,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		Review:      u.CanReview(),
		Admin:       u.CanAdminister(),
		Local:       h.Local != nil,
	}
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	course := h.Course.Get()
	tasks, err := h.loadDashboard(r.Context(), course, u.ID)
	if err != nil {
		h.httpError(w, r, "error.load_failed", http.StatusInternalServerError)
		return
	}
	h.renderPage(w, r, "dashboard", dashboardData{
		CourseName: course.Resolved.Course.Name,
		User:       h.userViewOf(u),
		Tasks:      tasks,
		Now:        time.Now(),
	})
}

// dashboardStream pushes re-rendered task rows over SSE when any of the
// user's submissions changes state. Event name = "task-<id>", the row itself
// carries the matching sse-swap attribute.
func (h *Handler) dashboardStream(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	sse, ok := newSSEWriter(w)
	if !ok {
		h.httpError(w, r, "error.streaming_unsupported", http.StatusInternalServerError)
		return
	}
	events, cancel := h.Hub.SubscribeUser(u.ID)
	defer cancel()

	lang := h.lang(r)
	// Re-render every row once after subscribing: the page rendered its
	// snapshot before this connection existed, and the Hub is allowed to drop
	// on overflow, so a state change landing in that gap is simply gone.
	h.sendDashboardRows(r, sse, lang, u.ID, "")
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			h.sendDashboardRows(r, sse, lang, u.ID, ev.TaskID)
		}
	}
}

// sendDashboardRows emits the task rows of one user: just taskID, or every
// task when it is empty (the post-subscribe reconciliation).
func (h *Handler) sendDashboardRows(r *http.Request, sse *sseWriter, lang string, userID int64, taskID string) {
	course := h.Course.Get()
	for _, task := range course.Resolved.Tasks {
		if taskID != "" && task.ID != taskID {
			continue
		}
		history, err := h.DB.ListByUserTask(r.Context(), userID, task.ID)
		if err != nil {
			continue
		}
		override, err := h.taskOverride(r.Context(), userID, task.ID)
		if err != nil {
			continue
		}
		view := buildTaskView(task, history, course.Resolved.Course.ScoringPolicy, override)
		html, err := renderPartial(lang, "task-row", view)
		if err != nil {
			continue
		}
		sse.send("task-"+task.ID, html)
	}
}
