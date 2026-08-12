package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
)

const testImage = "alpine:3"

// dockerAvailable reports whether a usable docker daemon is reachable.
func dockerAvailable() bool {
	return exec.Command("docker", "version", "--format", "{{.Server.Version}}").Run() == nil
}

// dockerWorkspace returns a workspace dir for a docker run. Any location will
// do: the workspace is copied into the container, never mounted, so the colima
// restriction on which host dirs reach the VM no longer applies.
func dockerWorkspace(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func dockerSpec(timeout time.Duration) config.ResolvedRunner {
	return config.ResolvedRunner{
		Type:    "docker",
		Image:   testImage,
		Timeout: timeout,
		Memory:  256 << 20,
		CPUs:    1,
		Network: "none",
	}
}

// TestDockerUserArg pins the user policy: student code never runs as root
// (SPEC §14), on every platform. The workspace is copied into the container
// with `docker cp`, which keeps the uid of the host files, so the container
// user is the process that assembled the workspace.
func TestDockerUserArg(t *testing.T) {
	if got := (&DockerRunner{User: "1000:1000"}).userArg(); got != "1000:1000" {
		t.Errorf("explicit User: got %q, want %q", got, "1000:1000")
	}
	want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if got := (&DockerRunner{}).userArg(); got != want {
		t.Errorf("default User on %s: got %q, want %q", runtime.GOOS, got, want)
	}
}

// TestDockerRunArgsWorkspaceIsTmpfs pins the container shape: /work is a
// tmpfs inside the container, the host workspace is never mounted, and the
// hardening flags stay on the container that the checks are exec'd into.
func TestDockerRunArgsWorkspaceIsTmpfs(t *testing.T) {
	job := Job{
		WorkspaceDir: "/host/anygrade/workspaces/ws-1",
		TaskRelDir:   "tasks/01",
		Spec:         dockerSpec(time.Minute),
		Checks:       []config.Check{{Name: "a", Run: "true"}},
	}
	s := &dockerSession{r: &DockerRunner{}, job: job}
	args := s.runArgs("ag-test")
	line := strings.Join(args, " ")

	if strings.Contains(line, job.WorkspaceDir) || slices.Contains(args, "-v") {
		t.Errorf("the host workspace must not be mounted into the container: %s", line)
	}
	if !strings.Contains(line, "dst="+containerWorkdir) || !strings.Contains(line, "volume-opt=type=tmpfs") {
		t.Errorf("%s must be a tmpfs mount: %s", containerWorkdir, line)
	}
	if !strings.Contains(line, fmt.Sprintf("size=%d", job.Spec.Memory)) {
		t.Errorf("the tmpfs must be sized by the memory limit: %s", line)
	}
	for _, want := range []string{"--read-only", "--detach", "--rm"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s: %s", want, line)
		}
	}
	if i := slices.Index(args, "--user"); i < 0 || args[i+1] != fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) {
		t.Errorf("container must run non-root as the workspace owner: %s", line)
	}
	// The container itself only waits; checks are exec'd into it. The wait is
	// bounded so a crashed anygrade cannot leak a container.
	last := args[len(args)-1]
	if !strings.HasPrefix(last, "sleep ") || last == "sleep 0" {
		t.Errorf("container command must be a bounded sleep, got %q", last)
	}
}

func TestDockerRunnerEndToEnd(t *testing.T) {
	if testing.Short() || !dockerAvailable() {
		t.Skip("docker not available")
	}
	ws := dockerWorkspace(t)
	writeFiles(t, ws, map[string]string{
		"tasks/01/input.txt": "data\n",
	})

	r := &DockerRunner{}
	job := Job{
		WorkspaceDir: ws,
		TaskRelDir:   "tasks/01",
		Spec:         dockerSpec(time.Minute),
		Checks: []config.Check{
			{Name: "read-workspace", Required: true, Run: "cat input.txt"},
			{Name: "write-workspace", Weight: 1, Run: "echo out > artifact.txt"},
			{Name: "sees-artifact", Weight: 1, Run: "cat artifact.txt"},
			{Name: "fails", Weight: 1, Run: "exit 7"},
		},
		LogDir: filepath.Join(t.TempDir(), "logs"),
	}
	outcomes, err := r.Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !outcomes[0].Passed {
		t.Fatalf("workspace not visible in container: %+v excerpt=%q", outcomes[0], outcomes[0].LogExcerpt)
	}
	if !strings.Contains(outcomes[0].LogExcerpt, "data") {
		t.Errorf("log excerpt: %q", outcomes[0].LogExcerpt)
	}
	if !outcomes[1].Passed {
		t.Errorf("workspace must be writable: %+v", outcomes[1])
	}
	// Filesystem state persists across checks: they share one container.
	if !outcomes[2].Passed || !strings.Contains(outcomes[2].LogExcerpt, "out") {
		t.Errorf("artifact must persist between checks: %+v", outcomes[2])
	}
	if outcomes[3].Passed || outcomes[3].ExitCode != 7 {
		t.Errorf("exit code must propagate: %+v", outcomes[3])
	}
}

// TestDockerRunnerWorkspaceIsolation pins SPEC §14: /work is a tmpfs, the
// check does not run as root, and nothing a check writes reaches the host.
func TestDockerRunnerWorkspaceIsolation(t *testing.T) {
	if testing.Short() || !dockerAvailable() {
		t.Skip("docker not available")
	}
	ws := dockerWorkspace(t)
	writeFiles(t, ws, map[string]string{"tasks/01/input.txt": "data\n"})

	r := &DockerRunner{}
	job := Job{
		WorkspaceDir: ws,
		TaskRelDir:   "tasks/01",
		Spec:         dockerSpec(time.Minute),
		Checks: []config.Check{
			{Name: "tmpfs", Required: true, Run: `grep -qE "^[^ ]+ /work tmpfs " /proc/mounts && echo WORK_IS_TMPFS`},
			{Name: "non-root", Required: true, Run: `test "$(id -u)" != 0 && echo "uid=$(id -u)"`},
			{Name: "writes", Required: true, Run: "echo leaked > escapee.txt && cat input.txt"},
		},
		LogDir: filepath.Join(t.TempDir(), "logs"),
	}
	outcomes, err := r.Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outcomes {
		if !o.Passed {
			t.Errorf("%s must pass: %+v excerpt=%q", o.Name, o, o.LogExcerpt)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "tasks", "01", "escapee.txt")); !os.IsNotExist(err) {
		t.Errorf("a file written by a check must not reach the host workspace (err=%v)", err)
	}
}

// TestDockerRunnerExportWorkspace covers `anygrade check --keep`: the caller
// asks for the ephemeral workspace to be copied back so the artifacts a check
// produced can be inspected on the host.
func TestDockerRunnerExportWorkspace(t *testing.T) {
	if testing.Short() || !dockerAvailable() {
		t.Skip("docker not available")
	}
	ws := dockerWorkspace(t)
	writeFiles(t, ws, map[string]string{"tasks/01/input.txt": "data\n"})

	r := &DockerRunner{}
	job := Job{
		WorkspaceDir:    ws,
		TaskRelDir:      "tasks/01",
		Spec:            dockerSpec(time.Minute),
		Checks:          []config.Check{{Name: "produce", Required: true, Run: "echo built > artifact.bin"}},
		LogDir:          filepath.Join(t.TempDir(), "logs"),
		ExportWorkspace: true,
	}
	if _, err := r.Run(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(ws, "tasks", "01", "artifact.bin")); got != "built\n" {
		t.Errorf("artifact not exported to the host: %q", got)
	}
}

// TestDockerRunnerContainerGone pins how a check that could not run at all is
// classified: the container died under it (daemon restart, expired lifetime),
// so this is an infrastructure error - retried, no attempt consumed (SPEC §13)
// - and not a check the student failed.
func TestDockerRunnerContainerGone(t *testing.T) {
	if testing.Short() || !dockerAvailable() {
		t.Skip("docker not available")
	}
	job := Job{
		WorkspaceDir: dockerWorkspace(t),
		Spec:         dockerSpec(time.Minute),
		Checks:       []config.Check{{Name: "x", Run: "true"}},
	}
	// A session that believes it owns a container which does not exist.
	s := &dockerSession{r: &DockerRunner{}, job: job, name: "ag-gone-" + randHex(6)}
	_, err := s.execCheck(t.Context(), job, job.Checks[0], filepath.Join(t.TempDir(), "x.log"))
	if infra, ok := errors.AsType[*InfraError](err); !ok || infra.Op != "runner_exec" {
		t.Fatalf("want InfraError(runner_exec), got %v", err)
	}
}

func TestDockerRunnerTimeout(t *testing.T) {
	if testing.Short() || !dockerAvailable() {
		t.Skip("docker not available")
	}
	ws := dockerWorkspace(t)
	r := &DockerRunner{}
	job := Job{
		WorkspaceDir: ws,
		TaskRelDir:   "",
		Spec:         dockerSpec(5 * time.Second),
		Checks: []config.Check{
			{Name: "before", Weight: 1, Run: "echo kept > artifact.txt"},
			{Name: "slow", Weight: 1, Run: "sleep 120"},
			{Name: "after", Weight: 1, Run: "cat artifact.txt"},
		},
		LogDir: filepath.Join(t.TempDir(), "logs"),
	}
	start := time.Now()
	outcomes, err := r.Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("timed-out container was not killed promptly: %v", elapsed)
	}
	if !outcomes[0].Passed {
		t.Errorf("before: %+v", outcomes[0])
	}
	if !outcomes[1].TimedOut || outcomes[1].Passed {
		t.Errorf("slow: %+v", outcomes[1])
	}
	// Killing a timed-out check tears the container down, so the checks that
	// follow get a fresh one - with the workspace they had built up so far.
	if !outcomes[2].Passed || !strings.Contains(outcomes[2].LogExcerpt, "kept") {
		t.Errorf("after must run in a fresh container over the same workspace: %+v", outcomes[2])
	}
}
