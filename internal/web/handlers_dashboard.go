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
	// Local marks `serve --local`: there is no session, so the header hides
	// the log out button.
	Local bool
}

// userViewOf builds the header identity every page carries.
func (h *Handler) userViewOf(u store.User) userView {
	return userView{u.Login, u.DisplayName, u.Role, h.Local != nil}
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	subs, err := h.DB.ListByUser(r.Context(), u.ID)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	course := h.Course.Get()
	h.renderPage(w, r, "dashboard", dashboardData{
		CourseName: course.Resolved.Course.Name,
		User:       h.userViewOf(u),
		Tasks:      buildDashboard(course, subs),
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
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	events, cancel := h.Hub.SubscribeUser(u.ID)
	defer cancel()

	lang := h.lang(r)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			course := h.Course.Get()
			task, _, ok := course.Task(ev.TaskID)
			if !ok {
				continue
			}
			history, err := h.DB.ListByUserTask(r.Context(), u.ID, ev.TaskID)
			if err != nil {
				continue
			}
			view := buildTaskView(task, history, course.Resolved.Course.ScoringPolicy)
			html, err := renderPartial(lang, "task-row", view)
			if err != nil {
				continue
			}
			sse.send("task-"+ev.TaskID, html)
		}
	}
}
