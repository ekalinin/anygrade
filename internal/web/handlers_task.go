package web

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/queue"
)

// markdown renders task READMEs. Raw HTML stays escaped (goldmark default):
// free defense-in-depth, teacher content needs no scripts.
var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

type taskData struct {
	CourseName string
	User       userView
	View       TaskView
	Statement  template.HTML
	History    []historyRow
	Quota      quotaView
	Now        time.Time
	Flash      string
}

type historyRow struct {
	ID         int64
	AttemptNo  *int
	Status     string
	FinalScore *float64
	ReceivedAt time.Time
}

type quotaView struct {
	AttemptsLeft  int
	Unlimited     bool
	CooldownUntil *time.Time
	CanRecheck    bool
}

func (h *Handler) taskPage(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	course := h.Course.Get()
	task, relDir, ok := course.Task(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	history, err := h.DB.ListByUserTask(r.Context(), u.ID, task.ID)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}

	statement := template.HTML("<p>(no README.md in the task directory)</p>")
	if raw, ok, err := h.ReadCourseFile(r.Context(), course.Head, relDir+"/README.md"); err == nil && ok {
		var buf bytes.Buffer
		if err := markdown.Convert(raw, &buf); err == nil {
			statement = template.HTML(buf.String())
		}
	}

	now := time.Now()
	left, unlimited, cooldown := queue.Quota(task, history, now)
	rows := make([]historyRow, 0, len(history))
	canRecheck := false
	for i := len(history) - 1; i >= 0; i-- {
		s := history[i]
		rows = append(rows, historyRow{s.ID, s.AttemptNo, s.Status, s.FinalScore, s.ReceivedAt})
		if s.Counts {
			canRecheck = true
		}
	}

	renderPage(w, "task", taskData{
		CourseName: course.Resolved.Course.Name,
		User:       userView{u.Login, u.DisplayName, u.Role},
		View:       buildTaskView(task, history, course.Resolved.Course.ScoringPolicy),
		Statement:  statement,
		History:    rows,
		Quota: quotaView{
			AttemptsLeft: left, Unlimited: unlimited,
			CooldownUntil: cooldown,
			CanRecheck:    canRecheck && cooldown == nil && (unlimited || left > 0),
		},
		Now:   now,
		Flash: r.URL.Query().Get("flash"),
	})
}

func (h *Handler) taskRecheck(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	taskID := r.PathValue("id")
	sub, d, err := h.Recheck.Recheck(r.Context(), u.ID, taskID)
	switch {
	case errors.Is(err, intake.ErrNothingToRecheck):
		http.Redirect(w, r, "/tasks/"+taskID+"?flash=nothing+to+recheck", http.StatusSeeOther)
	case err != nil:
		http.Error(w, "recheck failed", http.StatusInternalServerError)
	case !d.Admit:
		http.Redirect(w, r, "/tasks/"+taskID+"?flash="+template.URLQueryEscaper(d.RejectReason), http.StatusSeeOther)
	default:
		http.Redirect(w, r, fmt.Sprintf("/submissions/%d", sub.ID), http.StatusSeeOther)
	}
}
