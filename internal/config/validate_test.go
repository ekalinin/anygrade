package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func diagStrings(diags []Diagnostic) []string {
	out := make([]string, len(diags))
	for i, d := range diags {
		out[i] = d.String()
	}
	return out
}
