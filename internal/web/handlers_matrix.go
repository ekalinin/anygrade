package web

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/store"
)

type matrixData struct {
	CourseName string
	User       userView
	Matrix     gradebook.Matrix
}

// buildMatrix loads the full gradebook (matrix page, CSV, leaderboard).
func (h *Handler) buildMatrix(r *http.Request) (gradebook.Matrix, error) {
	course := h.Course.Get()
	users, err := h.DB.ListUsers(r.Context())
	if err != nil {
		return gradebook.Matrix{}, err
	}
	subs, err := h.DB.ListAllSubmissions(r.Context())
	if err != nil {
		return gradebook.Matrix{}, err
	}
	overrides, err := h.DB.ListScoreOverrides(r.Context())
	if err != nil {
		return gradebook.Matrix{}, err
	}
	return gradebook.Build(users, taskCols(course), subs, overrides,
		course.Resolved.Course.ScoringPolicy), nil
}

func (h *Handler) matrixPage(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	m, err := h.buildMatrix(r)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	renderPage(w, "matrix", matrixData{
		CourseName: h.Course.Get().Resolved.Course.Name,
		User:       userView{u.Login, u.DisplayName, u.Role},
		Matrix:     m,
	})
}

// matrixStream re-renders one student's matrix row on any of their events.
func (h *Handler) matrixStream(w http.ResponseWriter, r *http.Request) {
	sse, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	events, cancel := h.Hub.SubscribeAll()
	defer cancel()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			row, login, err := h.buildMatrixRow(r, ev.UserID)
			if err != nil {
				continue
			}
			html, err := renderPartial("matrix-row", matrixRowData{Row: row, Tasks: taskCols(h.Course.Get())})
			if err != nil {
				continue
			}
			sse.send("user-"+login, html)
		}
	}
}

type matrixRowData struct {
	Row   gradebook.Row
	Tasks []gradebook.TaskCol
}

// buildMatrixRow rebuilds a single student's row (short indexed reads only).
func (h *Handler) buildMatrixRow(r *http.Request, userID int64) (gradebook.Row, string, error) {
	course := h.Course.Get()
	target, err := h.DB.GetUserByID(r.Context(), userID)
	if err != nil {
		return gradebook.Row{}, "", err
	}
	subs, err := h.DB.ListByUser(r.Context(), userID)
	if err != nil {
		return gradebook.Row{}, "", err
	}
	overrides, err := h.DB.ListScoreOverrides(r.Context())
	if err != nil {
		return gradebook.Row{}, "", err
	}
	m := gradebook.Build([]store.User{target}, taskCols(course), subs, overrides,
		course.Resolved.Course.ScoringPolicy)
	if len(m.Rows) == 0 {
		return gradebook.Row{}, "", fmt.Errorf("user %d is not a student", userID)
	}
	return m.Rows[0], target.Login, nil
}

func (h *Handler) exportCSV(w http.ResponseWriter, r *http.Request) {
	m, err := h.buildMatrix(r)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="scores.csv"`)
	_ = gradebook.WriteCSV(w, m)
}

// setOverride records a manual score for (student, task) (SPEC §9).
func (h *Handler) setOverride(w http.ResponseWriter, r *http.Request) {
	actor := user(r)
	target, err := h.DB.GetUserByLogin(r.Context(), r.PathValue("login"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	taskID := r.PathValue("id")
	if _, _, ok := h.Course.Get().Task(taskID); !ok {
		http.NotFound(w, r)
		return
	}
	score, err := strconv.ParseFloat(r.FormValue("score"), 64)
	if err != nil || score < 0 {
		http.Error(w, "score must be a non-negative number", http.StatusBadRequest)
		return
	}
	comment := r.FormValue("comment")
	err = h.DB.SetScoreOverride(r.Context(), store.ScoreOverride{
		UserID: target.ID, TaskID: taskID, Score: score,
		Comment: comment, TeacherID: actor.ID,
	})
	if err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	_ = h.DB.Log(r.Context(), store.Event{
		ActorID: &actor.ID, Kind: "override.set",
		Target: target.Login + "/" + taskID,
		Detail: fmt.Sprintf("score=%s comment=%q", gradebook.FmtScore(score), comment),
	})
	http.Redirect(w, r, "/students/"+target.Login, http.StatusSeeOther)
}

func (h *Handler) clearOverride(w http.ResponseWriter, r *http.Request) {
	actor := user(r)
	target, err := h.DB.GetUserByLogin(r.Context(), r.PathValue("login"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	taskID := r.PathValue("id")
	if err := h.DB.DeleteScoreOverride(r.Context(), target.ID, taskID); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	_ = h.DB.Log(r.Context(), store.Event{
		ActorID: &actor.ID, Kind: "override.clear",
		Target: target.Login + "/" + taskID,
	})
	http.Redirect(w, r, "/students/"+target.Login, http.StatusSeeOther)
}
