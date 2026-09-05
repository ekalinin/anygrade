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
	// CanDownloadLogs gates the raw full-log download to reviewers (SPEC §14:
	// students see the log as their tests produced it, staff see it whole).
	CanDownloadLogs bool
	// BuildChecks names the checks that currently declare a build phase, so
	// a reviewer gets its (staff-only) log even when the build succeeded.
	// A row's own BuildFailed covers the history the metadata has moved on
	// from.
	BuildChecks map[string]bool
	// Note is the worker note this viewer may read. A submission with no check
	// results has nothing else to explain itself, so the note is rendered on
	// its own - but only a reviewer gets it whole: the student reads the
	// student-safe projection the writer left behind, empty whenever the note
	// is operator detail (SPEC §14).
	Note     string
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

func (h *Handler) submissionData(sub store.Submission, checks []store.CheckRow, viewer store.User) submissionData {
	data := submissionData{
		Sub:             sub,
		Status:          subDisplayStatus(sub),
		Checks:          checks,
		CanDownloadLogs: viewer.CanReview(),
		Note:            sub.StudentNote,
		Running:         !terminalSubmission(sub),
		Rejected:        sub.Status == store.StatusRejectedDeadline || sub.Status == store.StatusRejectedLimit,
	}
	if viewer.CanReview() {
		data.Note = sub.WorkerNote
	}
	if task, _, ok := h.Course.Get().Task(sub.TaskID); ok {
		data.TaskName = task.Name
		data.TaskScore = task.Score
		data.BuildChecks = make(map[string]bool, len(task.Checks))
		for i, c := range task.Checks {
			data.Panes = append(data.Panes, logPane{Index: i, Name: c.Name})
			data.BuildChecks[c.Name] = c.Build != ""
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

// submissionLog serves one full check log from disk (SPEC §13: the DB holds
// only the excerpt). `?phase=build` asks for the build phase's log instead of
// the run phase's; `?view=1` hands the bytes to the browser to read instead of
// to save.
//
// Reviewers only (SPEC §14: "check logs are shown to students as produced by
// their tests; staff see full logs"). Student code runs under the same UID
// as the hidden tests copied into the workspace, so a solution can read them
// and print them; handing their own author the raw log would turn that into an
// exfiltration channel. The stored excerpt and the live stream stay with the
// student - both are bounded by the task's `runner.log_excerpt`.
//
// The build phase is the stricter case: it is the phase that compiles against
// the hidden tests, so a compiler quoting a hidden source line lands in its
// log. Nothing of it reaches the student anywhere - no excerpt is stored and
// the live stream does not tail it - and this route is the only way to read it.
// A TA reads it too: the person helping a student through a compile failure is
// exactly the one who needs the compiler's words, and a reviewer who may run
// the hidden tests and read their output has no smaller view of them than the
// build log offers.
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
	if !user(r).CanReview() {
		http.NotFound(w, r) // 404, not 403: never leak what exists (SPEC §14)
		return
	}
	check := r.PathValue("check")
	if !slices.ContainsFunc(checks, func(c store.CheckRow) bool { return c.Name == check }) {
		http.NotFound(w, r)
		return
	}
	// The file keeps the check's ordinary name inside the build subdirectory;
	// only the name offered for download is disambiguated, so a teacher who
	// saves both phases of one check ends up with two files.
	dir, file := intake.SubmissionLogDir(h.DataDir, sub.ID), runner.LogFileName(check)
	name := file
	if r.URL.Query().Get("phase") == "build" {
		dir, name = runner.BuildLogDir(dir), "build-"+file
	}
	f, err := os.Open(filepath.Join(dir, file))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// A log is student-controlled bytes served from the site's own origin. The
	// attachment disposition is what keeps a browser from rendering them today,
	// and the viewer below drops exactly that, so the declared type has to bind
	// on its own: without nosniff a log opening with "<html>" would be sniffed
	// into a page running inside the teacher's session.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The viewer is the download minus one header - same file, same role check,
	// deliberately the browser's own text viewer rather than a rendered page:
	// it holds a `runner.log_max`-sized log (10 MB by default) without an
	// inline cap, and find-in-page keeps working over the whole of it, which is
	// most of the reason to open a log at all.
	if r.URL.Query().Get("view") != "1" {
		// FormatMediaType quotes and RFC 2231-encodes the file name; building
		// the header by hand would let a check name carrying a quote break out
		// of it.
		if cd := mime.FormatMediaType("attachment", map[string]string{"filename": name}); cd != "" {
			w.Header().Set("Content-Disposition", cd)
		}
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
