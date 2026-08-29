package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ekalinin/anygrade/internal/i18n"
	"github.com/ekalinin/anygrade/internal/version"
)

//go:embed templates static
var assets embed.FS

// .woff2 is not in Go's builtin MIME table. It resolves from a system table
// on a developer machine and not in a minimal container, so pin it here,
// once per process, to keep the served type identical everywhere.
func init() {
	if err := mime.AddExtensionType(".woff2", "font/woff2"); err != nil {
		panic(err)
	}
}

func staticHandler() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServerFS(sub)
}

// courseTZ holds the lookup for the course display timezone (SPEC §13: the UI
// renders in the course timezone). It is package state rather than page data
// for the same reason `version` is: timestamps live in partials shared by every
// page, so each page struct would otherwise have to carry the location. Storing
// the lookup instead of the location itself means a teacher metadata push that
// changes `timezone:` is picked up without a restart.
var courseTZ atomic.Pointer[func() *time.Location]

// SetTimezoneSource installs the course display-timezone lookup; the
// composition root is the only caller.
func SetTimezoneSource(fn func() *time.Location) { courseTZ.Store(&fn) }

// displayTZ is the location fmtTime renders in. Until a source is installed
// (tests, non-server commands) it is UTC, so output never depends on the
// machine's local zone.
func displayTZ() *time.Location {
	if fn := courseTZ.Load(); fn != nil {
		if loc := (*fn)(); loc != nil {
			return loc
		}
	}
	return time.UTC
}

// localeFuncs builds the template FuncMap bound to one locale. The locale-free
// helpers (fmtTime, score, statusClass, dict) are the same for every locale;
// the translation helpers (t, tFlash, tStatus, countdown) close over the
// locale's translator.
func localeFuncs(lang string) template.FuncMap {
	tr := i18n.For(lang)
	return template.FuncMap{
		"fmtTime": func(t any) string {
			return withTime(t, func(v time.Time) string { return v.In(displayTZ()).Format("2006-01-02 15:04") })
		},
		"countdown": func(t any) string {
			return withTime(t, func(v time.Time) string { return countdown(v, time.Now(), tr) })
		},
		"score":       fmtScore,
		"statusClass": statusClass,
		"dict":        dict,
		"t":           tr.T,
		"tFlash":      tr.TFlash,
		"tStatus":     tr.TStatus,
		"lang":        tr.Lang,
		"locales":     i18n.Locales,
		"upper":       strings.ToUpper,
		// Functions, not page data: the footer lives in base.html and every
		// page would otherwise have to carry these in its own struct.
		"version": version.Short,
		"repoURL": func() string { return version.URL },
	}
}

// fmtScore trims a trailing ".0" so whole scores render as integers.
func fmtScore(f any) string {
	v, ok := f.(float64)
	if p, isPtr := f.(*float64); isPtr {
		if p == nil {
			return ""
		}
		v, ok = *p, true
	}
	if !ok {
		return ""
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
}

// statusClass maps a status value to its CSS badge class (raw value, not the
// translated label - the stylesheet keys off the English enum).
func statusClass(s string) string {
	return "st-" + strings.NewReplacer(" ", "-", "_", "-").Replace(s)
}

// dict builds a payload for partials that need several roots (a struct on the
// Go side, a map here: field and key access read the same).
func dict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("dict: odd argument count")
	}
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		k, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", pairs[i])
		}
		m[k] = pairs[i+1]
	}
	return m, nil
}

// withTime accepts time.Time and *time.Time (templates mix both).
func withTime(t any, f func(time.Time) string) string {
	switch v := t.(type) {
	case time.Time:
		return f(v)
	case *time.Time:
		if v != nil {
			return f(*v)
		}
	}
	return ""
}

var pageNames = []string{
	"login", "dashboard", "task", "submission",
	"matrix", "queue", "students", "student", "code",
	"leaderboard", "settings", "key_challenge", "invite", "register", "token_once", "audit",
}

// pages maps locale -> page name -> parsed template set (base + partials + the
// page). The set is parsed once per locale at init with a locale-bound FuncMap,
// so request-time rendering only picks the map entry (templates stay immutable
// and race-free).
var pages = func() map[string]map[string]*template.Template {
	m := map[string]map[string]*template.Template{}
	for _, lang := range i18n.Locales() {
		base := template.Must(template.New("base").Funcs(localeFuncs(lang)).ParseFS(assets,
			"templates/base.html", "templates/partials/*.html"))
		set := map[string]*template.Template{}
		for _, page := range pageNames {
			clone := template.Must(base.Clone())
			set[page] = template.Must(clone.ParseFS(assets, "templates/"+page+".html"))
		}
		m[lang] = set
	}
	return m
}()

// httpError is http.Error with a localized body. SPEC §10.1 keeps push output,
// the CLI and server logs English, but everything the browser renders belongs
// to the UI - including the failure paths, which is exactly when a reader is
// least able to work around a language they do not read.
func (h *Handler) httpError(w http.ResponseWriter, r *http.Request, key string, code int) {
	http.Error(w, i18n.For(h.lang(r)).T(key), code)
}

// renderPage executes a full page in the request's locale; render errors after
// headers are logged in-band (template output is already partially written -
// keep it simple).
func (h *Handler) renderPage(w http.ResponseWriter, r *http.Request, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages[h.lang(r)][page].ExecuteTemplate(w, "base", data); err != nil {
		fmt.Fprintf(w, "<!-- render error: %v -->", err)
	}
}

// renderPartial executes one named partial into a buffer (htmx fragments and
// SSE payloads) in the given locale.
func renderPartial(lang, name string, data any) (string, error) {
	var buf bytes.Buffer
	// Partials are parsed into every page set; any set for the locale can
	// execute them.
	if err := pages[lang]["dashboard"].ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// sseWriter frames SSE events over one streaming response.
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	return &sseWriter{w: w, f: f}, true
}

// sseLineBreaks normalizes every SSE line terminator to "\n". The event-stream
// format ends a line on CRLF, CR, or LF alike, so a lone CR inside a payload
// would end the `data:` line early and let the rest of it be parsed as stream
// syntax - check output containing a carriage return could inject events into
// the stream a teacher is watching. Splitting on "\n" only is not enough.
var sseLineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// sseEventName drops line terminators outright: an event name has no multi-line
// form, so a break in one (a task id is a directory name) could only be framing.
var sseEventName = strings.NewReplacer("\r", "", "\n", "")

// send frames one event; multi-line payloads become multiple data: lines
// (the SSE format reassembles them with \n).
func (s *sseWriter) send(event, payload string) {
	fmt.Fprintf(s.w, "event: %s\n", sseEventName.Replace(event))
	for line := range strings.SplitSeq(sseLineBreaks.Replace(payload), "\n") {
		fmt.Fprintf(s.w, "data: %s\n", line)
	}
	io.WriteString(s.w, "\n")
	s.f.Flush()
}
