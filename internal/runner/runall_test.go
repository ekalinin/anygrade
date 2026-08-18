package runner

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/config"
)

// fakeExecutor passes/fails checks by name.
type fakeExecutor struct {
	fail map[string]bool
	ran  []string
}

func (f *fakeExecutor) execCheck(_ context.Context, _ Job, c config.Check, logPath string) (Outcome, error) {
	f.ran = append(f.ran, c.Name)
	passed := !f.fail[c.Name]
	exit := 0
	if !passed {
		exit = 1
	}
	return Outcome{Name: c.Name, Passed: passed, ExitCode: exit, LogPath: logPath}, nil
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
		{Name: "build", Required: true, Run: "true"},
		{Name: "vet", Required: true, Run: "true"},
		{Name: "basic", Weight: 60, Run: "true"},
		{Name: "advanced", Weight: 40, Run: "true"},
	}
	ex := &fakeExecutor{fail: map[string]bool{"vet": true}}
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
		{Name: "basic", Weight: 60, Run: "true"},
		{Name: "advanced", Weight: 40, Run: "true"},
	}
	ex := &fakeExecutor{fail: map[string]bool{"basic": true}}
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
