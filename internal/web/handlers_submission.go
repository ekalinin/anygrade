package web

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	// Status is the refined display status (retrying / error / canceled for an
	// infra_error), the same value the queue view shows.
	Status    string
	TaskName  string
	TaskScore int
	Checks    []store.CheckRow
	// Panes pre-renders one log pane per metadata check, so the SSE stream
	// never has to create elements dynamically (design risk #4 avoidance).
	Panes []logPane
	// CanDownloadLogs gates the raw full-log download to teachers (SPEC §14:
	// students see the log as their tests produced it, teachers see it whole).
	CanDownloadLogs bool
	Running         bool
	Rejected        bool
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

func (h *Handler) submissionData(sub store.Submission, checks []store.CheckRow, viewer store.User) submissionData {
	data := submissionData{
		Sub:             sub,
		Status:          subDisplayStatus(sub),
		Checks:          checks,
		CanDownloadLogs: viewer.Role == "teacher",
		Running:         !terminalSubmission(sub),
		Rejected:        sub.Status == store.StatusRejectedDeadline || sub.Status == store.StatusRejectedLimit,
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
	data := h.submissionData(sub, checks, u)
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
	html, err := renderPartial(h.lang(r), "sub-results", h.submissionData(sub, checks, user(r)))
	if err != nil {
		h.httpError(w, r, "error.render_failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

// submissionLog downloads one full check log from disk (SPEC §13: the DB holds
// only the excerpt).
//
// Teachers only (SPEC §14: "check logs are shown to students as produced by
// their tests; teachers see full logs"). Student code runs under the same UID
// as the hidden tests copied into the workspace, so a solution can read them
// and print them; handing their own author the raw log would turn that into an
// exfiltration channel. The stored excerpt and the live stream stay with the
// student - both are bounded by the task's `runner.log_excerpt`.
//
// The check name is validated by membership in this submission's results, not
// by shape: metadata only requires a name to be non-empty and unique within the
// task, so any pattern-based allowlist rejects logs the run legitimately wrote.
// Traversal is not a concern either way - the name never reaches the path, only
// runner.LogFileName's deterministic rendering of it does.
func (h *Handler) submissionLog(w http.ResponseWriter, r *http.Request) {
	sub, checks, ok := h.loadSubmission(w, r)
	if !ok {
		return
	}
	if user(r).Role != "teacher" {
		http.NotFound(w, r) // 404, not 403: never leak what exists (SPEC §14)
		return
	}
	check := r.PathValue("check")
	if !slices.ContainsFunc(checks, func(c store.CheckRow) bool { return c.Name == check }) {
		http.NotFound(w, r)
		return
	}
	name := runner.LogFileName(check)
	f, err := os.Open(filepath.Join(intake.SubmissionLogDir(h.DataDir, sub.ID), name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// FormatMediaType quotes and RFC 2231-encodes the file name; building the
	// header by hand would let a check name carrying a quote break out of it.
	if cd := mime.FormatMediaType("attachment", map[string]string{"filename": name}); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
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
		return false // ambiguous by status alone; see terminalSubmission
	default:
		return true
	}
}

// terminalSubmission applies the queue's retry model to the one status that
// terminalStatus cannot decide on its own. An infra_error is only on its way
// back to a worker while a retry is actually scheduled: retries exhausted
// (retry_at nil) and a teacher cancel (canceled_at set) are both final, and
// treating them as running left the page promising a "retrying" that never
// came and the SSE stream open until the client gave up.
func terminalSubmission(sub store.Submission) bool {
	if sub.Status == store.StatusInfraError {
		return sub.RetryAt == nil || sub.CanceledAt != nil
	}
	return terminalStatus(sub.Status)
}
