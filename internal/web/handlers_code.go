package web

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ekalinin/anygrade/internal/store"
)

// codeMaxInline caps inlined file size; bigger files download instead.
const codeMaxInline = 256 << 10

// ErrFileTooLarge is what a file reader returns for a file that exists but is
// too large to hold in memory. web never imports gitserver, so the composition
// root maps the git-side error onto this one.
var ErrFileTooLarge = errors.New("file too large to read")

type codeData struct {
	CourseName string
	User       userView
	Student    string
	Sub        store.Submission
	Files      []string
	// Single-file view; empty Path = the listing.
	Path string
	// Content is the whole file, syntax-highlighted (see highlightLine on why
	// pre-escaped HTML is safe here).
	Content template.HTML
	Binary  bool
	TooBig  bool
	// Oversize is TooBig's harder cousin: the file is past what the server will
	// read at all, so unlike TooBig there is no download to offer.
	Oversize bool
	// HasDiff says the file has an authoritative counterpart, so the page shows
	// the diff/full-file toggle; Diff is the delta itself and is nil when the
	// file still matches the template. DiffView picks the rendered view.
	HasDiff  bool
	Diff     []diffRow
	DiffView bool
}

// loadCodeView resolves {login}+{id} and lists the submitted commit's files
// (task dir when the task still exists, whole tree otherwise).
func (h *Handler) loadCodeView(w http.ResponseWriter, r *http.Request) (codeData, bool) {
	target, err := h.DB.GetUserByLogin(r.Context(), r.PathValue("login"))
	if err != nil {
		http.NotFound(w, r)
		return codeData{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return codeData{}, false
	}
	sub, _, err := h.DB.GetSubmission(r.Context(), id)
	if err != nil || sub.UserID != target.ID {
		http.NotFound(w, r)
		return codeData{}, false
	}
	relDir := ""
	if _, rd, ok := h.Course.Get().Task(sub.TaskID); ok {
		relDir = rd
	}
	files, err := h.ListStudentFiles(r.Context(), target.Login, sub.CommitSHA, relDir)
	if err != nil {
		h.httpError(w, r, "error.commit_unreadable", http.StatusInternalServerError)
		return codeData{}, false
	}
	u := user(r)
	return codeData{
		CourseName: h.Course.Get().Resolved.Course.Name,
		User:       h.userViewOf(u),
		Student:    target.Login,
		Sub:        sub,
		Files:      files,
	}, true
}

func (h *Handler) codeList(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadCodeView(w, r)
	if !ok {
		return
	}
	h.renderPage(w, r, "code", data)
}

func (h *Handler) codeFile(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadCodeView(w, r)
	if !ok {
		return
	}
	path := r.PathValue("path")
	// The listing is the allowlist: never hand the raw URL path to git.
	if !slices.Contains(data.Files, path) {
		http.NotFound(w, r)
		return
	}
	content, found, err := h.ReadStudentFile(r.Context(), data.Student, data.Sub.CommitSHA, path)
	switch {
	case errors.Is(err, ErrFileTooLarge):
		// The file is there; saying "not found" would send the teacher looking
		// for a bug in the listing instead of at the file's size.
		data.Oversize = true
	case err != nil || !found:
		http.NotFound(w, r)
		return
	}
	data.Path = path
	// The download is the raw blob and needs none of the rendering below.
	if r.URL.Query().Get("download") == "1" {
		if data.Oversize {
			h.httpError(w, r, "code.too_large", http.StatusRequestEntityTooLarge)
			return
		}
		name := path[strings.LastIndexByte(path, '/')+1:]
		w.Header().Set("Content-Type", "application/octet-stream")
		// The name comes out of the student's commit, so it is arbitrary by
		// definition. FormatMediaType quotes and RFC 2231-encodes it; building
		// the header by hand would let a crafted file name carry a quote and
		// hand the teacher a download under some other name entirely.
		if cd := mime.FormatMediaType("attachment", map[string]string{"filename": name}); cd != "" {
			w.Header().Set("Content-Disposition", cd)
		}
		_, _ = w.Write(content)
		return
	}
	switch {
	case data.Oversize:
	case len(content) > codeMaxInline:
		data.TooBig = true
	case bytes.IndexByte(content[:min(len(content), 8<<10)], 0) >= 0:
		data.Binary = true
	default:
		text := string(content)
		l := langFor(path)
		data.Content = highlightFile(text, l)
		// What the student changed is the reason this page exists, so the diff
		// is the default view when there is one to show; ?view=full is one
		// click away and stays the only view when there is not.
		if base, ok := h.courseVersion(r.Context(), data.Sub.TaskID, path); ok {
			if ops, ok := lineDiff(splitLines(base), splitLines(text)); ok {
				data.HasDiff = true
				data.Diff = renderDiff(ops, l)
				data.DiffView = r.URL.Query().Get("view") != "full"
			}
		}
	}
	h.renderPage(w, r, "code", data)
}

// courseVersion reads the authoritative version of a submitted file: the same
// repo-relative path in the course mirror, which web reaches through the
// injected reader the task page already uses for READMEs - no new seam, and
// still no gitserver import.
//
// The comparison is against the course head as it stands now, not as it stood
// when the student pushed: nothing records the latter, and a teacher reading a
// submission is reading it against the template they can open today.
func (h *Handler) courseVersion(ctx context.Context, taskID, relPath string) (string, bool) {
	course := h.Course.Get()
	task, relDir, ok := course.Task(taskID)
	if !ok || !isSolutionFile(relDir, task.SolutionFiles, relPath) {
		return "", false
	}
	raw, ok, err := h.ReadCourseFile(ctx, course.Head, relPath)
	if err != nil || !ok {
		return "", false
	}
	return string(raw), true
}
