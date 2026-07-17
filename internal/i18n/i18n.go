// Package i18n is a tiny message catalog for the web UI (SPEC §10). Catalogs
// are flat key->string YAML files embedded at build time; en is the source of
// truth and every key must exist in it. It is a leaf package (stdlib + yaml)
// imported by both config (to validate course.yaml's language) and web.
package i18n

import (
	"embed"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed locales/*.yaml
var localeFS embed.FS

// Default is the fallback locale: its catalog is the source of truth and the
// lookup fallback for every other locale.
const Default = "en"

var (
	catalogs = map[string]map[string]string{}
	locales  []string
)

func init() {
	entries, err := localeFS.ReadDir("locales")
	if err != nil {
		panic("i18n: read locales dir: " + err.Error())
	}
	for _, e := range entries {
		lang, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok {
			continue
		}
		data, err := localeFS.ReadFile("locales/" + e.Name())
		if err != nil {
			panic("i18n: read " + e.Name() + ": " + err.Error())
		}
		var cat map[string]string
		if err := yaml.Unmarshal(data, &cat); err != nil {
			panic("i18n: parse " + e.Name() + ": " + err.Error())
		}
		catalogs[lang] = cat
	}
	if _, ok := catalogs[Default]; !ok {
		panic("i18n: default locale " + Default + " has no catalog")
	}
	// Default first, remaining locales sorted: a stable, predictable order for
	// the language switcher and validation messages.
	for lang := range catalogs {
		if lang != Default {
			locales = append(locales, lang)
		}
	}
	slices.Sort(locales)
	locales = append([]string{Default}, locales...)
}

// Locales returns the supported locale codes, Default first.
func Locales() []string {
	return slices.Clone(locales)
}

// Supported reports whether lang has a catalog.
func Supported(lang string) bool {
	_, ok := catalogs[lang]
	return ok
}

// Translator resolves keys for one locale.
type Translator struct {
	lang string
	cat  map[string]string
}

// For returns a Translator for lang, falling back to Default for unknown codes.
func For(lang string) Translator {
	cat, ok := catalogs[lang]
	if !ok {
		lang, cat = Default, catalogs[Default]
	}
	return Translator{lang: lang, cat: cat}
}

// Lang returns the translator's locale code.
func (t Translator) Lang() string { return t.lang }

// Lookup finds key in this locale, then in Default; ok is false if neither has
// it.
func (t Translator) Lookup(key string) (string, bool) {
	if v, ok := t.cat[key]; ok {
		return v, true
	}
	if v, ok := catalogs[Default][key]; ok {
		return v, true
	}
	return "", false
}

// T returns the translation for key, Sprintf-formatted when args are given.
// An unknown key renders as the key itself (visible but harmless).
func (t Translator) T(key string, args ...any) string {
	s, ok := t.Lookup(key)
	if !ok {
		s = key
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

// TStatus translates a status label (a raw enum value that may contain spaces,
// e.g. "not started"). Unknown values pass through unchanged so DB-stored
// statuses still render.
func (t Translator) TStatus(status string) string {
	if v, ok := t.Lookup("status." + strings.ReplaceAll(status, " ", "_")); ok {
		return v
	}
	return status
}

// TFlash translates a flash/error code (e.g. "unknown_login"). Empty stays
// empty; unknown codes (dynamic reject reasons stored at submission time) pass
// through unchanged.
func (t Translator) TFlash(code string) string {
	if code == "" {
		return ""
	}
	if v, ok := t.Lookup("flash." + code); ok {
		return v
	}
	return code
}
