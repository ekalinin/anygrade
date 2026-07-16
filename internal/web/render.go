package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed templates static
var assets embed.FS

func staticHandler() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServerFS(sub)
}

var funcs = template.FuncMap{
	"fmtTime": func(t any) string {
		return withTime(t, func(v time.Time) string { return v.Local().Format("2006-01-02 15:04") })
	},
	"countdown": func(t any) string { return withTime(t, func(v time.Time) string { return countdown(v, time.Now()) }) },
	"score": func(f any) string {
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
	},
	"statusClass": func(s string) string {
		return "st-" + strings.NewReplacer(" ", "-", "_", "-").Replace(s)
	},
	// dict builds a payload for partials that need several roots (a struct
	// on the Go side, a map here: field and key access read the same).
	"dict": func(pairs ...any) (map[string]any, error) {
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
	},
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

// pages maps page name -> parsed template set (base + partials + the page).
var pages = func() map[string]*template.Template {
	base := template.Must(template.New("base").Funcs(funcs).ParseFS(assets,
		"templates/base.html", "templates/partials/*.html"))
	m := map[string]*template.Template{}
	for _, page := range []string{
		"login", "dashboard", "task", "submission",
		"matrix", "queue", "students", "student", "code",
		"leaderboard", "settings", "invite", "register", "token_once", "audit",
	} {
		clone := template.Must(base.Clone())
		m[page] = template.Must(clone.ParseFS(assets, "templates/"+page+".html"))
	}
	return m
}()

// renderPage executes a full page; render errors after headers are logged
// in-band (template output is already partially written - keep it simple).
func renderPage(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages[page].ExecuteTemplate(w, "base", data); err != nil {
		fmt.Fprintf(w, "<!-- render error: %v -->", err)
	}
}

// renderPartial executes one named partial into a buffer (htmx fragments and
// SSE payloads).
func renderPartial(name string, data any) (string, error) {
	var buf bytes.Buffer
	// Partials are parsed into every page set; any set can execute them.
	if err := pages["dashboard"].ExecuteTemplate(&buf, name, data); err != nil {
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

// send frames one event; multi-line payloads become multiple data: lines
// (the SSE format reassembles them with \n).
func (s *sseWriter) send(event, payload string) {
	fmt.Fprintf(s.w, "event: %s\n", event)
	for line := range strings.SplitSeq(payload, "\n") {
		fmt.Fprintf(s.w, "data: %s\n", line)
	}
	io.WriteString(s.w, "\n")
	s.f.Flush()
}
