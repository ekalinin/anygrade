package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/store"
)

// matrixStatuses lists the filterable statuses in display order (SPEC §10).
var matrixStatuses = []string{
	gradebook.StatusNotStarted, gradebook.StatusRetrying, gradebook.StatusError,
	gradebook.StatusCanceled, gradebook.StatusRejected, gradebook.StatusPassed,
	gradebook.StatusPartial, gradebook.StatusFailed,
}

type matrixData struct {
	CourseName string
	User       userView
	Matrix     gradebook.Matrix
	Q          string
	Task       string
	Status     string
	Statuses   []string
	Filtered   bool
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
	q := r.URL.Query().Get("q")
	task := r.URL.Query().Get("task")
	status := r.URL.Query().Get("status")
	m.Rows = filterRows(m.Rows, m.Tasks, q, task, status)
	renderPage(w, "matrix", matrixData{
		CourseName: h.Course.Get().Resolved.Course.Name,
		User:       userView{u.Login, u.DisplayName, u.Role},
		Matrix:     m,
		Q:          q,
		Task:       task,
		Status:     status,
		Statuses:   matrixStatuses,
		Filtered:   q != "" || task != "" || status != "",
	})
}

// filterRows applies the matrix page's q/task/status filters (SPEC §10).
// q matches login or display name (case-insensitive substring); status
// alone keeps rows with any matching cell; task+status keeps rows where
// that task's cell matches; task alone is a no-op.
func filterRows(rows []gradebook.Row, tasks []gradebook.TaskCol, q, taskID, status string) []gradebook.Row {
	if q == "" && status == "" {
		return rows
	}
	validTask := false
	for _, t := range tasks {
		if t.ID == taskID {
			validTask = true
			break
		}
	}
	if !validTask {
		taskID = ""
	}

	q = strings.ToLower(q)
	out := make([]gradebook.Row, 0, len(rows))
	for _, row := range rows {
		if q != "" &&
			!strings.Contains(strings.ToLower(row.User.Login), q) &&
			!strings.Contains(strings.ToLower(row.User.DisplayName), q) {
			continue
		}
		if status != "" {
			switch {
			case taskID != "":
				if cellStatus(row, taskID) != status {
					continue
				}
			default:
				match := false
				for _, t := range tasks {
					if cellStatus(row, t.ID) == status {
						match = true
						break
					}
				}
				if !match {
					continue
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// cellStatus normalizes a cell's status for filtering: Build clears
// StatusNotStarted to "" for display, so restore it here.
func cellStatus(row gradebook.Row, taskID string) string {
	s := row.Cells[taskID].Status
	if s == "" {
		return gradebook.StatusNotStarted
	}
	return s
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
