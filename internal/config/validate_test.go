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

// TestValidateLogExcerpt covers the runner.log_excerpt rule: 0 (explicitly
// disabling the excerpt) is an error, an oversized value is only a warning,
// and the inherited default passes clean.
func TestValidateLogExcerpt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := func(excerpt *ByteSize) []Diagnostic {
		task := &Task{
			Dir: dir, ID: "w", Name: "W", Score: 100,
			SolutionFiles: []string{"main.go"},
			Runner:        RunnerSpec{Type: new("local"), LogExcerpt: excerpt},
			Checks:        []Check{{Name: "test", Weight: 100, Run: "go test ./..."}},
		}
		rt := Resolve(&Course{}, task)
		rt.file = "task.yaml"
		return Validate(&Resolved{
			Course:    ResolvedCourse{Name: "C", TasksDir: "tasks", Registration: Registration{Mode: "invite"}, ScoringPolicy: "best"},
			rawCourse: &Course{Registration: Registration{Mode: "invite"}, Scoring: Scoring{Policy: "best"}},
			Tasks:     []ResolvedTask{rt},
		})
	}

	if !hasFieldError(build(new(ByteSize(0))), "runner.log_excerpt") {
		t.Error("log_excerpt 0 should be an error")
	}
	big := build(new(ByteSize(8 << 20)))
	if hasFieldError(big, "runner.log_excerpt") {
		t.Error("an oversized log_excerpt should warn, not fail")
	}
	if !strings.Contains(strings.Join(diagStrings(big), "\n"), "kept in memory per check") {
		t.Errorf("expected an oversized-excerpt warning, got:\n%s", strings.Join(diagStrings(big), "\n"))
	}
	for _, d := range build(nil) {
		if d.Field == "runner.log_excerpt" {
			t.Errorf("inherited default should be clean, got: %s", d)
		}
	}
}

// TestValidateCheckNameShape covers the check-name warning: a slash or
// whitespace stays legal (the log download resolves names against the
// submission's results) but is worth flagging, because it is rewritten in the
// log file name and percent-encoded in the URL.
func TestValidateCheckNameShape(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := &Task{
		Dir: dir, ID: "w", Name: "W", Score: 100,
		SolutionFiles: []string{"main.go"},
		Runner:        RunnerSpec{Type: new("local")},
		Checks: []Check{
			{Name: "build/all", Weight: 50, Run: "go build ./..."},
			{Name: "unit", Weight: 50, Run: "go test ./..."},
		},
	}
	rt := Resolve(&Course{}, task)
	rt.file = "task.yaml"
	diags := Validate(&Resolved{
		Course:    ResolvedCourse{Name: "C", TasksDir: "tasks", Registration: Registration{Mode: "invite"}, ScoringPolicy: "best"},
		rawCourse: &Course{Registration: Registration{Mode: "invite"}, Scoring: Scoring{Policy: "best"}},
		Tasks:     []ResolvedTask{rt},
	})

	if HasErrors(diags) {
		t.Fatalf("a slash in a check name must not fail validation: %v", diagStrings(diags))
	}
	joined := strings.Join(diagStrings(diags), "\n")
	if !strings.Contains(joined, "rewritten in the log file name") {
		t.Errorf("expected a check-name warning, got:\n%s", joined)
	}
	if strings.Count(joined, "rewritten in the log file name") != 1 {
		t.Errorf("only checks[0] should warn, got:\n%s", joined)
	}
}

// TestValidateCheckWeightNegative covers the check-weight rule: weights are
// normalized over the non-gate checks (SPEC §4.3), so a negative weight shrinks
// the divisor - 60 and -40 sum to 20, and passing the first check alone would
// score 300 out of 100. Weight 0 stays legal (dead-weight warning only).
func TestValidateCheckWeightNegative(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := func(checks ...Check) []Diagnostic {
		return validateOneTask(&Task{
			Dir: dir, ID: "w", Name: "W", Score: 100,
			SolutionFiles: []string{"main.go"},
			Runner:        RunnerSpec{Type: new("local")},
			Checks:        checks,
		})
	}

	neg := build(
		Check{Name: "basic", Weight: 60, Run: "go test -run Basic ./..."},
		Check{Name: "advanced", Weight: -40, Run: "go test -run Advanced ./..."},
	)
	if !hasFieldError(neg, "checks[1].weight") {
		t.Errorf("a negative weight should be an error, got:\n%s", strings.Join(diagStrings(neg), "\n"))
	}

	zero := build(
		Check{Name: "build", Required: true, Weight: 0, Run: "go build ./..."},
		Check{Name: "basic", Weight: 100, Run: "go test ./..."},
		Check{Name: "extra", Weight: 0, Run: "go vet ./..."},
	)
	if HasErrors(zero) {
		t.Errorf("weight 0 must stay legal, got:\n%s", strings.Join(diagStrings(zero), "\n"))
	}
}

// TestValidateHiddenLocalPath covers the hidden_tests.path rules for
// source: local. The path is resolved on the machine that runs the checks, so a
// missing or relative path only warns; an existing absolute directory is clean.
func TestValidateHiddenLocalPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hiddenFile := filepath.Join(dir, "main.go")
	build := func(path string) []Diagnostic {
		return validateOneTask(&Task{
			Dir: dir, ID: "w", Name: "W", Score: 100,
			SolutionFiles: []string{"main.go"},
			Runner:        RunnerSpec{Type: new("local")},
			HiddenTests:   &HiddenTests{Source: "local", Path: path},
			Checks:        []Check{{Name: "test", Weight: 100, Run: "go test ./..."}},
		})
	}

	cases := []struct {
		path string
		want string // expected substring; "" = no hidden_tests.path diagnostic
	}{
		{dir, ""},
		{filepath.Join(dir, "nope"), "does not exist here"},
		{"hidden/01-intro", "should be an absolute path"},
		{hiddenFile, "is not a directory"},
	}
	for _, tc := range cases {
		diags := build(tc.path)
		if HasErrors(diags) {
			t.Errorf("path %q must not fail validation, got:\n%s", tc.path, strings.Join(diagStrings(diags), "\n"))
		}
		var got []string
		for _, d := range diags {
			if d.Field == "hidden_tests.path" {
				got = append(got, d.String())
			}
		}
		joined := strings.Join(got, "\n")
		if tc.want == "" {
			if len(got) > 0 {
				t.Errorf("path %q should be clean, got:\n%s", tc.path, joined)
			}
			continue
		}
		if !strings.Contains(joined, tc.want) {
			t.Errorf("path %q: missing warning %q, got:\n%s", tc.path, tc.want, joined)
		}
	}
}

// TestValidateHiddenGitURLCredentials covers the hidden_tests.url rule: the URL
// is passed to git and logged, so credentials must come from the environment
// (SPEC §11, §14). A bare ssh username is a normal remote address, not a secret.
func TestValidateHiddenGitURLCredentials(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := func(url string) []Diagnostic {
		return validateOneTask(&Task{
			Dir: dir, ID: "w", Name: "W", Score: 100,
			SolutionFiles: []string{"main.go"},
			Runner:        RunnerSpec{Type: new("local")},
			HiddenTests:   &HiddenTests{Source: "git", URL: url, Ref: "main"},
			Checks:        []Check{{Name: "test", Weight: 100, Run: "go test ./..."}},
		})
	}

	cases := []struct {
		url     string
		wantErr bool
	}{
		{"https://user:s3cret@example.com/org/hidden.git", true},
		{"https://s3cret@example.com/org/hidden.git", true},
		{"ssh://git:s3cret@example.com/org/hidden.git", true},
		{"https://example.com/org/hidden.git", false},
		{"ssh://git@example.com/org/hidden.git", false},
		{"git@github.com:org/hidden.git", false},
		{"file:///srv/hidden.git", false},
	}
	for _, tc := range cases {
		diags := build(tc.url)
		got := hasFieldError(diags, "hidden_tests.url")
		if got != tc.wantErr {
			t.Errorf("url %q: error=%v, want %v; diagnostics:\n%s", tc.url, got, tc.wantErr, strings.Join(diagStrings(diags), "\n"))
		}
		// The diagnostic travels back in the teacher's push output; it must
		// not repeat the secret it complains about.
		if joined := strings.Join(diagStrings(diags), "\n"); strings.Contains(joined, "s3cret") {
			t.Errorf("url %q: diagnostic leaks the credential:\n%s", tc.url, joined)
		}
	}
}

// validateOneTask validates a single task inside an otherwise valid course.
func validateOneTask(task *Task) []Diagnostic {
	rt := Resolve(&Course{}, task)
	rt.file = "task.yaml"
	return Validate(&Resolved{
		Course:    ResolvedCourse{Name: "C", TasksDir: "tasks", Registration: Registration{Mode: "invite"}, ScoringPolicy: "best"},
		rawCourse: &Course{Registration: Registration{Mode: "invite"}, Scoring: Scoring{Policy: "best"}},
		Tasks:     []ResolvedTask{rt},
	})
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

// hasFieldWarning reports whether diags carries a SevWarning for the field.
func hasFieldWarning(diags []Diagnostic, field string) bool {
	for _, d := range diags {
		if d.Severity == SevWarning && d.Field == field {
			return true
		}
	}
	return false
}

// taskWithDir builds a minimal valid task rooted at dir.
func taskWithDir(dir string) *Task {
	return &Task{
		Dir: dir, ID: "w", Name: "W", Score: 100,
		SolutionFiles: []string{"main.go"},
		Runner:        RunnerSpec{Type: new("local")},
		Checks:        []Check{{Name: "test", Weight: 100, Run: "true"}},
	}
}

func mainGoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestValidateSizeLimits covers the keys the hardening work added. None of
// them is load-bearing for safety - the runner falls back to its built-in
// default for anything non-positive - so this is about telling the teacher
// that what they wrote was ignored.
func TestValidateSizeLimits(t *testing.T) {
	dir := mainGoDir(t)

	task := taskWithDir(dir)
	task.Runner.LogMax = new(ByteSize(0))
	if !hasFieldError(validateOneTask(task), "runner.log_max") {
		t.Error("runner.log_max = 0 is not an error")
	}

	// An excerpt bigger than the file it is taken from is a size the UI can
	// never show.
	task = taskWithDir(dir)
	task.Runner.LogExcerpt = new(ByteSize(64 << 10))
	task.Runner.LogMax = new(ByteSize(1 << 10))
	if !hasFieldWarning(validateOneTask(task), "runner.log_max") {
		t.Error("log_max below log_excerpt earns no warning")
	}

	task = taskWithDir(dir)
	task.Workspace.MaxFileSize = new(ByteSize(0))
	if !hasFieldError(validateOneTask(task), "workspace.max_file_size") {
		t.Error("workspace.max_file_size = 0 is not an error")
	}

	task = taskWithDir(dir)
	task.Workspace.MaxTotalSize = new(ByteSize(0))
	if !hasFieldError(validateOneTask(task), "workspace.max_total_size") {
		t.Error("workspace.max_total_size = 0 is not an error")
	}

	// A total below the per-file cap means no single file can ever be overlaid.
	task = taskWithDir(dir)
	task.Workspace.MaxFileSize = new(ByteSize(10 << 20))
	task.Workspace.MaxTotalSize = new(ByteSize(1 << 20))
	if !hasFieldError(validateOneTask(task), "workspace.max_total_size") {
		t.Error("max_total_size below max_file_size is not an error")
	}

	// The inherited defaults must stay clean.
	if diags := validateOneTask(taskWithDir(dir)); len(diags) > 0 {
		t.Errorf("defaults are not clean: %v", diagStrings(diags))
	}
}

// TestValidateMaxPushSize: resolution silently falls back to the default for a
// non-positive value, so validation is the only place a teacher finds out.
func TestValidateMaxPushSize(t *testing.T) {
	dir := mainGoDir(t)
	build := func(size *ByteSize) []Diagnostic {
		rt := Resolve(&Course{}, taskWithDir(dir))
		rt.file = "task.yaml"
		return Validate(&Resolved{
			Course:    ResolvedCourse{Name: "C", TasksDir: "tasks", Registration: Registration{Mode: "invite"}, ScoringPolicy: "best"},
			rawCourse: &Course{Registration: Registration{Mode: "invite"}, Scoring: Scoring{Policy: "best"}, Limits: CourseLimits{MaxPushSize: size}},
			Tasks:     []ResolvedTask{rt},
		})
	}
	if !hasFieldError(build(new(ByteSize(0))), "limits.max_push_size") {
		t.Error("max_push_size = 0 is not an error")
	}
	if !hasFieldWarning(build(new(ByteSize(4<<10))), "limits.max_push_size") {
		t.Error("a cap far below a normal push earns no warning")
	}
	if diags := build(new(ByteSize(50 << 20))); len(diags) > 0 {
		t.Errorf("a sane cap is not clean: %v", diagStrings(diags))
	}
}

// TestValidateRefusesSymlinkedPaths: the workspace is materialized as a plain
// tree, so a symlink is skipped there and the file is simply absent when the
// checks run. os.Stat followed the link and reported it present.
func TestValidateRefusesSymlinkedPaths(t *testing.T) {
	dir := mainGoDir(t)
	if err := os.Symlink(filepath.Join(dir, "main.go"), filepath.Join(dir, "link.go")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	task := taskWithDir(dir)
	task.SolutionFiles = []string{"link.go"}
	if !hasFieldError(validateOneTask(task), "solution_files[0]") {
		t.Error("a symlinked solution file is accepted")
	}

	// workspace.include is resolved against the repo root, which for a task at
	// the repo root is the task dir itself.
	task = taskWithDir(dir)
	task.Workspace.Include = []string{"link.go"}
	if !hasFieldError(validateOneTask(task), "workspace.include[0]") {
		t.Error("a symlinked workspace.include path is accepted")
	}
}

// TestValidateHiddenLocalRoots warns only when the validating process has the
// allowlist, since validate usually runs somewhere that is not the server.
func TestValidateHiddenLocalRoots(t *testing.T) {
	dir := mainGoDir(t)
	hidden := t.TempDir()
	task := taskWithDir(dir)
	task.HiddenTests = &HiddenTests{Source: "local", Path: hidden}

	t.Setenv(hiddenLocalRootsEnv, "")
	if hasFieldWarning(validateOneTask(task), "hidden_tests.path") {
		t.Error("warned about the allowlist with the variable unset")
	}
	t.Setenv(hiddenLocalRootsEnv, t.TempDir())
	if !hasFieldWarning(validateOneTask(task), "hidden_tests.path") {
		t.Error("a path outside every root earns no warning")
	}
	t.Setenv(hiddenLocalRootsEnv, hidden)
	if hasFieldWarning(validateOneTask(task), "hidden_tests.path") {
		t.Error("warned about a path inside an allowed root")
	}
}
