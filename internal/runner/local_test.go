package runner

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
)

func localJob(t *testing.T, timeout time.Duration, checks []config.Check) Job {
	t.Helper()
	ws := t.TempDir()
	return Job{
		WorkspaceDir: ws,
		TaskRelDir:   "",
		Spec:         config.ResolvedRunner{Type: "local", Timeout: timeout},
		Checks:       checks,
		LogDir:       filepath.Join(t.TempDir(), "logs"),
	}
}

func TestLocalRunnerPassFail(t *testing.T) {
	r := &LocalRunner{}
	job := localJob(t, time.Minute, []config.Check{
		{Name: "ok", Weight: 1, Run: "echo hello && exit 0"},
		{Name: "bad", Weight: 1, Run: "echo oops >&2; exit 3"},
	})
	outcomes, err := r.Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !outcomes[0].Passed || outcomes[0].ExitCode != 0 {
		t.Errorf("ok: %+v", outcomes[0])
	}
	if !strings.Contains(outcomes[0].LogExcerpt, "hello") {
		t.Errorf("stdout not captured: %q", outcomes[0].LogExcerpt)
	}
	if outcomes[1].Passed || outcomes[1].ExitCode != 3 {
		t.Errorf("bad: %+v", outcomes[1])
	}
	if !strings.Contains(outcomes[1].LogExcerpt, "oops") {
		t.Errorf("stderr not captured: %q", outcomes[1].LogExcerpt)
	}
	// Full log persisted on disk.
	if got := readFile(t, outcomes[0].LogPath); !strings.Contains(got, "hello") {
		t.Errorf("log file: %q", got)
	}
}

func TestLocalRunnerTimeoutKillsProcessGroup(t *testing.T) {
	r := &LocalRunner{}
	job := localJob(t, 300*time.Millisecond, []config.Check{
		// The sleep runs as a child of sh; the process-group kill must take
		// both down, otherwise Run would block for 30s.
		{Name: "slow", Weight: 1, Run: "sleep 30 & wait"},
		{Name: "after", Weight: 1, Run: "echo still runs"},
	})
	start := time.Now()
	outcomes, err := r.Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout did not kill the process tree, took %v", elapsed)
	}
	if !outcomes[0].TimedOut || outcomes[0].Passed {
		t.Errorf("slow: %+v", outcomes[0])
	}
	if !strings.Contains(outcomes[0].LogExcerpt, "timed out after") {
		t.Errorf("missing timeout note: %q", outcomes[0].LogExcerpt)
	}
	// A non-gate timeout does not stop the run (SPEC §13).
	if !outcomes[1].Passed || outcomes[1].Skipped {
		t.Errorf("after: %+v", outcomes[1])
	}
}

func TestLocalRunnerGateTimeoutSkipsRest(t *testing.T) {
	r := &LocalRunner{}
	job := localJob(t, 300*time.Millisecond, []config.Check{
		{Name: "gate", Required: true, Run: "sleep 30 & wait"},
		{Name: "rest", Weight: 1, Run: "echo unreachable"},
	})
	outcomes, err := r.Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !outcomes[0].TimedOut {
		t.Errorf("gate: %+v", outcomes[0])
	}
	if !outcomes[1].Skipped {
		t.Errorf("rest must be skipped after a timed-out gate: %+v", outcomes[1])
	}
}
