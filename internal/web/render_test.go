package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/i18n"
	"github.com/ekalinin/anygrade/internal/store"
	"github.com/ekalinin/anygrade/internal/version"
)

// TestPagesParsePerLocale fails loudly if any template references a function or
// syntax that breaks in one locale's parsed set (the per-locale init would
// panic before this, but keep an explicit guard).
func TestPagesParsePerLocale(t *testing.T) {
	for _, lang := range i18n.Locales() {
		set, ok := pages[lang]
		if !ok {
			t.Fatalf("no parsed page set for locale %q", lang)
		}
		for _, page := range pageNames {
			if set[page] == nil {
				t.Errorf("locale %q: page %q not parsed", lang, page)
			}
		}
	}
}

// TestRenderLoginLocalized renders the login page in each locale and checks the
// title reflects the locale (English default stays "Sign in").
func TestRenderLoginLocalized(t *testing.T) {
	cases := map[string]string{"en": "Sign in", "ru": "Вход"}
	for lang, want := range cases {
		var buf bytes.Buffer
		data := loginData{Next: "/"}
		if err := pages[lang]["login"].ExecuteTemplate(&buf, "base", data); err != nil {
			t.Fatalf("render login [%s]: %v", lang, err)
		}
		out := buf.String()
		if !strings.Contains(out, want) {
			t.Errorf("login [%s]: missing %q", lang, want)
		}
		if !strings.Contains(out, `lang="`+lang+`"`) {
			t.Errorf("login [%s]: missing html lang attribute", lang)
		}
	}
}

// TestRenderFooter checks the footer carries the project link and the running
// version. The footer lives in base.html and reads the version through a
// template func, so one page per locale covers every page.
func TestRenderFooter(t *testing.T) {
	for _, lang := range i18n.Locales() {
		var buf bytes.Buffer
		if err := pages[lang]["login"].ExecuteTemplate(&buf, "base", loginData{Next: "/"}); err != nil {
			t.Fatalf("render login [%s]: %v", lang, err)
		}
		out := buf.String()
		if !strings.Contains(out, "https://github.com/ekalinin/anygrade") {
			t.Errorf("footer [%s]: missing the project link", lang)
		}
		if !strings.Contains(out, version.Short()) {
			t.Errorf("footer [%s]: missing version %q", lang, version.Short())
		}
	}
}

// TestFmtTimeCourseTimezone checks the template helper renders in the course
// timezone (SPEC §13) and falls back to UTC - never the machine's local zone -
// when no source is installed.
func TestFmtTimeCourseTimezone(t *testing.T) {
	fmtTime, ok := localeFuncs("en")["fmtTime"].(func(any) string)
	if !ok {
		t.Fatal("fmtTime is not func(any) string")
	}
	// 12:30 UTC; Europe/Berlin is UTC+2 on that date.
	at := time.Date(2026, 7, 1, 12, 30, 0, 0, time.UTC)

	t.Cleanup(func() { courseTZ.Store(nil) })
	courseTZ.Store(nil)
	if got, want := fmtTime(at), "2026-07-01 12:30"; got != want {
		t.Errorf("no source installed: fmtTime = %q, want %q (UTC)", got, want)
	}

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	SetTimezoneSource(func() *time.Location { return berlin })
	if got, want := fmtTime(&at), "2026-07-01 14:30"; got != want {
		t.Errorf("Europe/Berlin: fmtTime = %q, want %q", got, want)
	}
}

// TestRenderTaskRowStatusLocalized checks the status badge label is translated
// while the CSS class keeps the raw English enum.
func TestRenderTaskRowStatusLocalized(t *testing.T) {
	view := TaskView{Status: "passed"}
	ru, err := renderPartial("ru", "task-row", view)
	if err != nil {
		t.Fatalf("render task-row [ru]: %v", err)
	}
	if !strings.Contains(ru, "зачтено") {
		t.Errorf("task-row [ru]: status label not translated: %s", ru)
	}
	if !strings.Contains(ru, "st-passed") {
		t.Errorf("task-row [ru]: CSS class should stay English-derived: %s", ru)
	}
}

// TestTaskRowShowsTheCorrection: when a teacher has overridden a score, the
// row shows both numbers - the machine's struck through and the teacher's in
// pen - plus the comment as a margin note. The pair, not the color alone, is
// what makes the override legible (SPEC.ui.md 3.1).
func TestTaskRowShowsTheCorrection(t *testing.T) {
	computed := 72.0
	view := TaskView{
		Task:     config.ResolvedTask{ID: "03-interfaces", Name: "Interfaces", Score: 100},
		Status:   "overridden",
		Score:    &computed,
		Override: &store.ScoreOverride{Score: 85, Comment: "partial credit"},
	}
	html, err := renderPartial("en", "task-row", view)
	if err != nil {
		t.Fatalf("render task-row: %v", err)
	}
	for _, want := range []string{
		`<span class="machine">72</span>`,
		`<span class="pen">85</span>`,
		`class="note"`,
		"partial credit",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("task-row: missing %s:\n%s", want, html)
		}
	}
}

// TestTaskRowWithoutOverrideHasNoPen: red means a human intervened, so a row
// the machine graded on its own must contain no pen markup at all.
func TestTaskRowWithoutOverrideHasNoPen(t *testing.T) {
	computed := 72.0
	view := TaskView{
		Task:   config.ResolvedTask{ID: "03-interfaces", Name: "Interfaces", Score: 100},
		Status: "passed",
		Score:  &computed,
	}
	html, err := renderPartial("en", "task-row", view)
	if err != nil {
		t.Fatalf("render task-row: %v", err)
	}
	for _, unwanted := range []string{`class="pen"`, `class="machine"`, `class="note"`} {
		if strings.Contains(html, unwanted) {
			t.Errorf("task-row: %s must not appear without an override:\n%s", unwanted, html)
		}
	}
}

// TestTaskRowGutterCarriesTheTaskID: the gutter shows the record's own key,
// not a loop ordinal - this partial is re-rendered alone over SSE, where no
// index exists. The task id is also what a student types in a
// [recheck <task-id>] commit marker.
func TestTaskRowGutterCarriesTheTaskID(t *testing.T) {
	view := TaskView{
		Task:   config.ResolvedTask{ID: "03-interfaces", Name: "Interfaces", Score: 100},
		Status: "not started",
	}
	html, err := renderPartial("en", "task-row", view)
	if err != nil {
		t.Fatalf("render task-row: %v", err)
	}
	if !strings.Contains(html, `<td class="key">03-interfaces</td>`) {
		t.Errorf("task-row: gutter should carry the task id:\n%s", html)
	}
}

// TestFontsAreServedWithTheRightType: the display face is embedded and served
// as font/woff2. The type matters because .woff2 is absent from Go's builtin
// MIME table - it resolves from a system table on a developer machine, but not
// in a minimal container, so staticHandler registers it explicitly.
func TestFontsAreServedWithTheRightType(t *testing.T) {
	for _, name := range []string{"geologica-latin.woff2", "geologica-cyrillic.woff2"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/fonts/"+name, nil)
		staticHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", name, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != "font/woff2" {
			t.Errorf("%s: Content-Type = %q, want %q", name, got, "font/woff2")
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: served an empty body", name)
		}
	}
}
