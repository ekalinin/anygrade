package web

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strconv"

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
	// Subs is the submission list below the task table: every submission of
	// the student, or - when TaskFilter is set - the (student, task) history
	// the matrix drills down into. Newest first.
	Subs       []studentSubRow
	TaskFilter string
	Keys       []store.SSHKey
	Events     []store.EventRow
	Flash      string
}

type studentSubRow struct {
	Sub      store.Submission
	TaskName string
	Status   string // display status incl. canceled/retrying/error
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
	// ?task= narrows the list to one (student, task) pair; the URL is what a
	// matrix cell links to, so it must stay stable.
	taskFilter := r.URL.Query().Get("task")
	listed := subs
	if taskFilter != "" {
		if listed, err = h.DB.ListByUserTask(r.Context(), target.ID, taskFilter); err != nil {
			http.Error(w, "load failed", http.StatusInternalServerError)
			return
		}
	}
	course := h.Course.Get()
	keys, _ := h.DB.ListSSHKeys(r.Context(), target.ID)
	events, _ := h.DB.ListEventsByTarget(r.Context(), target.Login, 20)

	h.renderPage(w, r, "student", studentData{
		CourseName: course.Resolved.Course.Name,
		User:       h.userViewOf(u),
		Student:    target,
		Tasks:      buildDashboard(course, subs, overrides),
		Subs:       studentSubRows(course, listed),
		TaskFilter: taskFilter,
		Keys:       keys,
		Events:     events,
		Flash:      r.URL.Query().Get("flash"),
	})
}

// studentSubRows decorates submissions with the task name and the refined
// display status, newest first (both store listings come out oldest first,
// and ListByUser groups by task).
func studentSubRows(course *intake.Course, subs []store.Submission) []studentSubRow {
	rows := make([]studentSubRow, len(subs))
	for i, s := range subs {
		name := s.TaskID // task deleted: history stays visible (SPEC §13)
		if t, _, ok := course.Task(s.TaskID); ok {
			name = t.Name
		}
		rows[i] = studentSubRow{Sub: s, TaskName: name, Status: subDisplayStatus(s)}
	}
	slices.SortStableFunc(rows, func(a, b studentSubRow) int {
		if c := b.Sub.ReceivedAt.Compare(a.Sub.ReceivedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.Sub.ID, a.Sub.ID)
	})
	return rows
}

func (h *Handler) teacherRecheck(w http.ResponseWriter, r *http.Request) {
	actor := user(r)
	target, err := h.DB.GetUserByLogin(r.Context(), r.PathValue("login"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	taskID := r.PathValue("id")
	sub, warn, err := h.Recheck.TeacherRecheck(r.Context(), actor, target.ID, taskID)
	switch {
	case errors.Is(err, intake.ErrNothingToRecheck):
		http.Redirect(w, r, "/students/"+target.Login+"?flash=nothing_to_recheck", http.StatusSeeOther)
	case err != nil:
		http.Error(w, "recheck failed", http.StatusInternalServerError)
	default:
		http.Redirect(w, r, submissionURL(sub.ID, warn), http.StatusSeeOther)
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

// adminDeleteKey removes one of the student's SSH keys (SPEC §10). The delete
// is scoped to the target, not to the actor, so an id belonging to somebody
// else 404s instead of touching another account's key.
//
// The form carries the fingerprint the teacher was shown, and the delete
// matches on it: reading the key first and deleting it afterwards would leave
// a window in which the student replaces that key, and a reused rowid would
// then remove the new one while the audit named the old.
func (h *Handler) adminDeleteKey(w http.ResponseWriter, r *http.Request) {
	actor := user(r)
	target, err := h.DB.GetUserByLogin(r.Context(), r.PathValue("login"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	fingerprint := r.FormValue("fingerprint")
	ok, err := h.DB.DeleteSSHKey(r.Context(), target.ID, id, fingerprint)
	if err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	_ = h.DB.Log(r.Context(), store.Event{
		ActorID: &actor.ID, Kind: "key.delete", Target: target.Login,
		Detail: fingerprint,
	})
	http.Redirect(w, r, "/students/"+target.Login, http.StatusSeeOther)
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
