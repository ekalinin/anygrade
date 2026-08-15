package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/store"
)

type studentsData struct {
	CourseName string
	User       userView
	Students   []store.User
}

func (h *Handler) studentsPage(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	users, err := h.DB.ListUsers(r.Context())
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	h.renderPage(w, r, "students", studentsData{
		CourseName: h.Course.Get().Resolved.Course.Name,
		User:       h.userViewOf(u),
		Students:   users,
	})
}

type studentData struct {
	CourseName string
	User       userView
	Student    store.User
	Tasks      []TaskView
	Keys       []store.SSHKey
	Events     []store.EventRow
	Flash      string
}

func (h *Handler) studentPage(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	target, err := h.DB.GetUserByLogin(r.Context(), r.PathValue("login"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	subs, err := h.DB.ListByUser(r.Context(), target.ID)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	overrides, err := h.userOverrides(r.Context(), target.ID)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	course := h.Course.Get()
	keys, _ := h.DB.ListSSHKeys(r.Context(), target.ID)
	events, _ := h.DB.ListEventsByTarget(r.Context(), target.Login, 20)

	h.renderPage(w, r, "student", studentData{
		CourseName: course.Resolved.Course.Name,
		User:       h.userViewOf(u),
		Student:    target,
		Tasks:      buildDashboard(course, subs, overrides),
		Keys:       keys,
		Events:     events,
		Flash:      r.URL.Query().Get("flash"),
	})
}

func (h *Handler) teacherRecheck(w http.ResponseWriter, r *http.Request) {
	actor := user(r)
	target, err := h.DB.GetUserByLogin(r.Context(), r.PathValue("login"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	taskID := r.PathValue("id")
	sub, err := h.Recheck.TeacherRecheck(r.Context(), actor, target.ID, taskID)
	switch {
	case errors.Is(err, intake.ErrNothingToRecheck):
		http.Redirect(w, r, "/students/"+target.Login+"?flash=nothing_to_recheck", http.StatusSeeOther)
	case err != nil:
		http.Error(w, "recheck failed", http.StatusInternalServerError)
	default:
		http.Redirect(w, r, fmt.Sprintf("/submissions/%d", sub.ID), http.StatusSeeOther)
	}
}

// adminResetToken issues a fresh token for a student and shows it once.
func (h *Handler) adminResetToken(w http.ResponseWriter, r *http.Request) {
	actor := user(r)
	target, err := h.DB.GetUserByLogin(r.Context(), r.PathValue("login"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	token, err := h.DB.IssueToken(r.Context(), target.ID)
	if err != nil {
		http.Error(w, "reset failed", http.StatusInternalServerError)
		return
	}
	_ = h.DB.Log(r.Context(), store.Event{
		ActorID: &actor.ID, Kind: "token.reset", Target: target.Login, Detail: "by teacher",
	})
	h.renderTokenOnce(w, r, target.Login, token, false)
}

func (h *Handler) adminSetState(w http.ResponseWriter, r *http.Request) {
	actor := user(r)
	login := r.PathValue("login")
	state := r.FormValue("state")
	if state != "active" && state != "disabled" {
		http.Error(w, "state must be active or disabled", http.StatusBadRequest)
		return
	}
	if login == actor.Login {
		http.Error(w, "refusing to change your own state", http.StatusBadRequest)
		return
	}
	if err := h.DB.SetUserState(r.Context(), login, state); err != nil {
		http.NotFound(w, r)
		return
	}
	_ = h.DB.Log(r.Context(), store.Event{
		ActorID: &actor.ID, Kind: "user.state", Target: login, Detail: state,
	})
	http.Redirect(w, r, "/students/"+login, http.StatusSeeOther)
}
