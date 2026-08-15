package web

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/runner"
	"github.com/ekalinin/anygrade/internal/store"
)

type submissionData struct {
	CourseName string
	User       userView
	Sub        store.Submission
	TaskName   string
	TaskScore  int
	Checks     []store.CheckRow
	// Panes pre-renders one log pane per metadata check, so the SSE stream
	// never has to create elements dynamically (design risk #4 avoidance).
	Panes    []logPane
	Running  bool
	Rejected bool
	// Flash carries a recheck warning from the redirect that landed here
	// (submissionURL); the fragment renderer leaves it empty.
	Flash string
}

type logPane struct {
	Index int
	Name  string
}

// loadSubmission is the shared fetch + ownership gate. A miss renders 404,
// never 403: object existence is not leaked (SPEC §14).
func (h *Handler) loadSubmission(w http.ResponseWriter, r *http.Request) (store.Submission, []store.CheckRow, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Submission{}, nil, false
	}
	sub, checks, err := h.DB.GetSubmission(r.Context(), id)
	if err != nil || !canSee(user(r), sub.UserID) {
		http.NotFound(w, r)
		return store.Submission{}, nil, false
	}
	return sub, checks, true
}

func (h *Handler) submissionData(sub store.Submission, checks []store.CheckRow) submissionData {
	data := submissionData{
		Sub:      sub,
		Checks:   checks,
		Running:  !terminalStatus(sub.Status),
		Rejected: sub.Status == store.StatusRejectedDeadline || sub.Status == store.StatusRejectedLimit,
	}
	if task, _, ok := h.Course.Get().Task(sub.TaskID); ok {
		data.TaskName = task.Name
		data.TaskScore = task.Score
		for i, c := range task.Checks {
			data.Panes = append(data.Panes, logPane{Index: i, Name: c.Name})
		}
	} else {
		data.TaskName = sub.TaskID // task deleted: history stays visible (SPEC §13)
	}
	return data
}

func (h *Handler) submissionPage(w http.ResponseWriter, r *http.Request) {
	sub, checks, ok := h.loadSubmission(w, r)
	if !ok {
		return
	}
	u := user(r)
	data := h.submissionData(sub, checks)
	data.CourseName = h.Course.Get().Resolved.Course.Name
	data.User = h.userViewOf(u)
	data.Flash = r.URL.Query().Get("flash")
	h.renderPage(w, r, "submission", data)
}

// submissionFragment re-renders the authoritative results block; the SSE
// `done` event triggers this fetch to reconcile any dropped live update.
func (h *Handler) submissionFragment(w http.ResponseWriter, r *http.Request) {
	sub, checks, ok := h.loadSubmission(w, r)
	if !ok {
		return
	}
	html, err := renderPartial(h.lang(r), "sub-results", h.submissionData(sub, checks))
	if err != nil {
		h.httpError(w, r, "error.render_failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

var checkNameRe = regexp.MustCompile(`^[A-Za-z0-9._ -]+$`)

// submissionLog downloads one full check log from disk (SPEC §13: the DB
// holds only the 64KB excerpt).
func (h *Handler) submissionLog(w http.ResponseWriter, r *http.Request) {
	sub, _, ok := h.loadSubmission(w, r)
	if !ok {
		return
	}
	check := r.PathValue("check")
	if !checkNameRe.MatchString(check) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(intake.SubmissionLogDir(h.DataDir, sub.ID), runner.LogFileName(check))
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+runner.LogFileName(check)+`"`)
	http.ServeContent(w, r, "", modTime(f), f)
}

func modTime(f *os.File) time.Time {
	if st, err := f.Stat(); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func terminalStatus(s string) bool {
	switch s {
	case store.StatusQueued, store.StatusRunning:
		return false
	case store.StatusInfraError:
		return false // retrying or awaiting teacher action; keep the stream open
	default:
		return true
	}
}
