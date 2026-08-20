package runner

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/config"
)

// fakeExecutor passes/fails phases by command; ran records the phases in the
// order the driver asked for them, which is what the two-phase order is about.
type fakeExecutor struct {
	fail    map[string]bool // keyed by the command the phase runs
	ran     []string        // "<check>" for a run phase, "<check>:build" for a build one
	dropped int
}

func (f *fakeExecutor) execCheck(_ context.Context, _ Job, c config.Check, cmd, logPath string) (Outcome, error) {
	label := c.Name
	if cmd == c.Build {
		label += ":build"
	}
	f.ran = append(f.ran, label)
	passed := !f.fail[cmd]
	exit := 0
	if !passed {
		exit = 1
	}
	return Outcome{Name: c.Name, Passed: passed, ExitCode: exit, LogPath: logPath}, nil
}

func (f *fakeExecutor) dropHiddenTests(context.Context, Job) error {
	f.ran = append(f.ran, "<boundary>")
	f.dropped++
	return nil
}

func testJob(t *testing.T, checks []config.Check) Job {
	t.Helper()
	return Job{
		WorkspaceDir: t.TempDir(),
		TaskRelDir:   "tasks/x",
		Checks:       checks,
		LogDir:       filepath.Join(t.TempDir(), "logs"),
	}
}

func TestRunAllGateShortCircuit(t *testing.T) {
	checks := []config.Check{
		{Name: "build", Required: true, Run: "run-build"},
		{Name: "vet", Required: true, Run: "run-vet"},
		{Name: "basic", Weight: 60, Run: "run-basic"},
		{Name: "advanced", Weight: 40, Run: "run-advanced"},
	}
	ex := &fakeExecutor{fail: map[string]bool{"run-vet": true}}
	outcomes, err := runAll(t.Context(), testJob(t, checks), ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 4 {
		t.Fatalf("want 4 outcomes, got %d", len(outcomes))
	}
	if !outcomes[0].Passed || outcomes[1].Passed {
		t.Errorf("unexpected gate outcomes: %+v", outcomes[:2])
	}
	for _, o := range outcomes[2:] {
		if !o.Skipped {
			t.Errorf("check %s should be skipped after failed gate", o.Name)
		}
	}
	if len(ex.ran) != 2 {
		t.Errorf("only build+vet should have run, ran: %v", ex.ran)
	}
}

func TestRunAllNonGateFailureContinues(t *testing.T) {
	checks := []config.Check{
		{Name: "basic", Weight: 60, Run: "run-basic"},
		{Name: "advanced", Weight: 40, Run: "run-advanced"},
	}
	ex := &fakeExecutor{fail: map[string]bool{"run-basic": true}}
	outcomes, err := runAll(t.Context(), testJob(t, checks), ex)
	if err != nil {
		t.Fatal(err)
	}
	if outcomes[0].Passed || outcomes[0].Skipped {
		t.Errorf("basic: %+v", outcomes[0])
	}
	if !outcomes[1].Passed || outcomes[1].Skipped {
		t.Errorf("advanced must still run after a non-gate failure: %+v", outcomes[1])
	}
}

// TestLogFileNameInjective: check names that sanitize to the same stem must
// still get their own log file, otherwise two checks silently overwrite each
// other's output (and the web layer downloads the wrong one).
func TestLogFileNameInjective(t *testing.T) {
	names := []string{
		"a/b", "a b", "a_b", "a\tb", "A_B", "a-b", "a~b", "..", ".hidden",
		"", "build", "go test ./...", strings.Repeat("x", 200), strings.Repeat("x", 201),
	}
	seen := map[string]string{}
	for _, n := range names {
		got := logFileName(n)
		// macOS is case-insensitive by default: two names that differ only in
		// case would still share one file there.
		key := strings.ToLower(got)
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q both map to %q", prev, n, got)
			continue
		}
		seen[key] = n
		if strings.ContainsAny(got, "/ \t") || len(got) > 255 {
			t.Errorf("%q maps to an unusable file name %q", n, got)
		}
		if got != logFileName(n) {
			t.Errorf("%q: mapping is not deterministic", n)
		}
	}
	// A name that needs no escaping keeps its spelling: log files stay readable.
	if got := logFileName("build"); got != "build.log" {
		t.Errorf("safe name got mangled: %q", got)
	}
}

func TestRunAllCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := runAll(ctx, testJob(t, []config.Check{{Name: "a", Run: "true"}}), &fakeExecutor{})
	if infra, ok := errors.AsType[*InfraError](err); !ok || infra.Op != "canceled" {
		t.Fatalf("want InfraError(canceled), got %v", err)
	}
}
