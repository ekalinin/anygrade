package runner

import (
	"context"
	"errors"
	"path/filepath"
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

func TestRunAllCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := runAll(ctx, testJob(t, []config.Check{{Name: "a", Run: "true"}}), &fakeExecutor{})
	if infra, ok := errors.AsType[*InfraError](err); !ok || infra.Op != "canceled" {
		t.Fatalf("want InfraError(canceled), got %v", err)
	}
}
