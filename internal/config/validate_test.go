package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateFixtureOK(t *testing.T) {
	r, loadDiags, err := LoadAll("../../testdata/course-ok")
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	diags := append(loadDiags, Validate(r)...)
	for _, d := range diags {
		if d.Severity == SevError {
			t.Errorf("unexpected error: %s", d)
		}
	}
	if HasErrors(diags) {
		t.Fatal("course-ok should validate clean")
	}
}

func TestValidateFixtureBad(t *testing.T) {
	r, loadDiags, err := LoadAll("../../testdata/course-bad")
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	diags := append(loadDiags, Validate(r)...)
	if !HasErrors(diags) {
		t.Fatal("course-bad should have errors")
	}
	joined := strings.Join(diagStrings(diags), "\n")
	wantSubstrings := []string{
		"field foo not found",           // unknown field (rule 1)
		"does not exist",                // missing solution file (rule 16)
		"soft deadline must be <= hard", // deadline ordering (rule 17)
		"duplicate task id",             // rule 12
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(joined, want) {
			t.Errorf("missing expected diagnostic %q in:\n%s", want, joined)
		}
	}
}

// TestValidateWarnings covers the warning rules (gate weight ignored, dead
// weight) without failing validation.
func TestValidateWarnings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := &Task{
		Dir:           dir,
		ID:            "w",
		Name:          "W",
		Score:         100,
		SolutionFiles: []string{"main.go"},
		// local needs no image; memory set explicitly to trigger the
		// docker-only-limits warning.
		Runner: RunnerSpec{Type: new("local"), Memory: new(ByteSize(512 << 20))},
		Checks: []Check{
			{Name: "build", Required: true, Weight: 5, Run: "go build ./..."}, // rule 25
			{Name: "a", Weight: 10, Run: "go test -run A ./..."},
			{Name: "b", Weight: 0, Run: "go test -run B ./..."}, // rule 24
		},
	}
	rt := Resolve(&Course{}, task)
	rt.file = "task.yaml"
	r := &Resolved{
		Course:    ResolvedCourse{Name: "C", TasksDir: "tasks", Registration: Registration{Mode: "invite"}, ScoringPolicy: "best"},
		rawCourse: &Course{Registration: Registration{Mode: "invite"}, Scoring: Scoring{Policy: "best"}},
		Tasks:     []ResolvedTask{rt},
	}

	diags := Validate(r)
	if HasErrors(diags) {
		t.Fatalf("expected no errors, got: %v", diagStrings(diags))
	}
	joined := strings.Join(diagStrings(diags), "\n")
	if !strings.Contains(joined, "ignored for required") {
		t.Errorf("expected gate-weight warning, got:\n%s", joined)
	}
	if !strings.Contains(joined, "never contributes") {
		t.Errorf("expected dead-weight warning, got:\n%s", joined)
	}
	if !strings.Contains(joined, "not enforced by the local runner") {
		t.Errorf("expected local-runner limits warning, got:\n%s", joined)
	}
}

// TestValidateWorkspaceInclude covers the workspace.include rules: absolute
// path, escaping path, missing path, and a valid include.
func TestValidateWorkspaceInclude(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "tasks", "w")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	task := &Task{
		Dir:           dir,
		ID:            "w",
		Name:          "W",
		Score:         100,
		SolutionFiles: []string{"main.go"},
		Workspace: WorkspaceSpec{Include: []string{
			"/abs/path",
			"../escape",
			"missing.txt",
			"go.mod",
		}},
		Checks: []Check{
			{Name: "build", Required: true, Weight: 0, Run: "go build ./..."},
			{Name: "test", Weight: 100, Run: "go test ./..."},
		},
	}
	rt := Resolve(&Course{}, task)
	rt.file = "tasks/w/task.yaml"
	r := &Resolved{
		Course:    ResolvedCourse{Name: "C", TasksDir: "tasks", Registration: Registration{Mode: "invite"}, ScoringPolicy: "best"},
		rawCourse: &Course{Registration: Registration{Mode: "invite"}, Scoring: Scoring{Policy: "best"}},
		Tasks:     []ResolvedTask{rt},
	}

	diags := Validate(r)
	joined := strings.Join(diagStrings(diags), "\n")
	wantSubstrings := []string{
		"must be a relative path",
		"must not escape the course repo",
		"does not exist in the course repo",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(joined, want) {
			t.Errorf("missing expected diagnostic %q in:\n%s", want, joined)
		}
	}
	for _, d := range diags {
		if d.Field == "workspace.include[3]" {
			t.Errorf("unexpected diagnostic for valid include: %s", d)
		}
	}
}

// TestValidateLanguage covers the course-level language rule: an unsupported
// code is an error; empty and supported codes are accepted.
func TestValidateLanguage(t *testing.T) {
	base := func(lang string) *Resolved {
		return &Resolved{
			Course:    ResolvedCourse{Name: "C", TasksDir: "tasks", Registration: Registration{Mode: "invite"}, ScoringPolicy: "best"},
			rawCourse: &Course{Name: "C", Registration: Registration{Mode: "invite"}, Language: lang},
		}
	}
	if !hasFieldError(Validate(base("de")), "language") {
		t.Errorf("language: de should raise a language error")
	}
	for _, ok := range []string{"", "en", "ru"} {
		if hasFieldError(Validate(base(ok)), "language") {
			t.Errorf("language %q should not raise a language error", ok)
		}
	}
}

// TestValidateTimezone covers the course-level timezone rule: a name
// time.LoadLocation cannot resolve is an error; empty and IANA names are
// accepted.
func TestValidateTimezone(t *testing.T) {
	base := func(tz string) *Resolved {
		return &Resolved{
			Course:    ResolvedCourse{Name: "C", TasksDir: "tasks", Registration: Registration{Mode: "invite"}, ScoringPolicy: "best"},
			rawCourse: &Course{Name: "C", Registration: Registration{Mode: "invite"}, Timezone: tz},
		}
	}
	if !hasFieldError(Validate(base("Mars/Olympus")), "timezone") {
		t.Errorf("timezone: Mars/Olympus should raise a timezone error")
	}
	for _, ok := range []string{"", "UTC", "Europe/Berlin"} {
		if hasFieldError(Validate(base(ok)), "timezone") {
			t.Errorf("timezone %q should not raise a timezone error", ok)
		}
	}
}

// TestResolveCourseTimezone pins the resolution side: a valid name loads, and
// both "unset" and "unloadable" fall back to UTC so Resolved is always usable
// (the error surfaces through Validate, not here).
func TestResolveCourseTimezone(t *testing.T) {
	if got := resolveCourse(&Course{Timezone: "Europe/Berlin"}).Timezone; got.String() != "Europe/Berlin" {
		t.Errorf("timezone Europe/Berlin resolved to %q", got)
	}
	for _, tz := range []string{"", "Mars/Olympus"} {
		if got := resolveCourse(&Course{Timezone: tz}).Timezone; got != time.UTC {
			t.Errorf("timezone %q resolved to %q, want UTC", tz, got)
		}
	}
}

// hasFieldError reports whether diags carries a SevError for the given field.
func hasFieldError(diags []Diagnostic, field string) bool {
	for _, d := range diags {
		if d.Severity == SevError && d.Field == field {
			return true
		}
	}
	return false
}

func diagStrings(diags []Diagnostic) []string {
	out := make([]string, len(diags))
	for i, d := range diags {
		out[i] = d.String()
	}
	return out
}
