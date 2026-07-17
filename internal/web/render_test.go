package web

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/i18n"
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
