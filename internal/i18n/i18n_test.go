package i18n

import (
	"regexp"
	"slices"
	"testing"
)

var verbRe = regexp.MustCompile(`%[#+\-0-9.]*[a-zA-Z%]`)

// verbs returns the sorted multiset of fmt verbs in s (literal %% dropped).
func verbs(s string) []string {
	var out []string
	for _, v := range verbRe.FindAllString(s, -1) {
		if v == "%%" {
			continue
		}
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// TestCatalogParity is the contract that keeps translations honest: every
// locale defines exactly the Default key set, and each value uses the same fmt
// verbs as the Default (a mismatch would panic at render time on Sprintf).
func TestCatalogParity(t *testing.T) {
	en := catalogs[Default]
	for lang, cat := range catalogs {
		if lang == Default {
			continue
		}
		for key := range en {
			if _, ok := cat[key]; !ok {
				t.Errorf("locale %q missing key %q", lang, key)
			}
		}
		for key, val := range cat {
			enVal, ok := en[key]
			if !ok {
				t.Errorf("locale %q has unknown key %q (not in %q)", lang, key, Default)
				continue
			}
			if ev, lv := verbs(enVal), verbs(val); !slices.Equal(ev, lv) {
				t.Errorf("locale %q key %q: fmt verbs %v != %v", lang, key, lv, ev)
			}
		}
	}
}

func TestLocalesDefaultFirst(t *testing.T) {
	got := Locales()
	if len(got) == 0 || got[0] != Default {
		t.Fatalf("Locales()[0] = %v, want %q first", got, Default)
	}
	if !slices.Contains(got, "ru") {
		t.Errorf("Locales() = %v, want it to contain \"ru\"", got)
	}
}

func TestForUnknownFallsBackToDefault(t *testing.T) {
	if For("xx").Lang() != Default {
		t.Errorf("For(\"xx\").Lang() = %q, want %q", For("xx").Lang(), Default)
	}
	if !Supported("ru") || Supported("xx") {
		t.Errorf("Supported: ru=%v xx=%v, want true/false", Supported("ru"), Supported("xx"))
	}
}

func TestLookupFallback(t *testing.T) {
	// A key present in en but (hypothetically) absent in ru resolves to en.
	ru := For("ru")
	if _, ok := ru.Lookup("login.title"); !ok {
		t.Fatalf("login.title should resolve for ru")
	}
	// Unknown key: T echoes it, TFlash/TStatus pass through.
	if got := ru.T("no.such.key"); got != "no.such.key" {
		t.Errorf("T(unknown) = %q, want the key echoed", got)
	}
	if got := ru.TFlash("hard deadline passed (x)"); got != "hard deadline passed (x)" {
		t.Errorf("TFlash(dynamic) = %q, want passthrough", got)
	}
	if got := ru.TStatus("mystery"); got != "mystery" {
		t.Errorf("TStatus(unknown) = %q, want passthrough", got)
	}
}

func TestTStatusHandlesSpaces(t *testing.T) {
	if got := For("en").TStatus("not started"); got != "not started" {
		t.Errorf("en TStatus(\"not started\") = %q", got)
	}
	if got := For("ru").TStatus("not started"); got != "не начато" {
		t.Errorf("ru TStatus(\"not started\") = %q, want translated", got)
	}
}
