package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// dockerWorkspace returns a workspace dir that the docker VM can bind-mount.
// t.TempDir() lives under /var/folders on darwin, which colima does not
// mount, so we use a dir under the repo tree (inside /Users) instead.
func dockerWorkspace(t *testing.T) string {
	t.Helper()
	base, err := filepath.Abs(filepath.Join("..", "..", ".anygrade", "test-workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "ws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
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

// TestDockerUserArg pins the platform defaults: on Linux the container runs as
// the process that owns the workspace (an image-default root could not write
// into it with all capabilities dropped), on macOS as the image default.
func TestDockerUserArg(t *testing.T) {
	if got := (&DockerRunner{User: "1000:1000"}).userArg(); got != "1000:1000" {
		t.Errorf("explicit User: got %q, want %q", got, "1000:1000")
	}
	want := ""
	if runtime.GOOS == "linux" {
		want = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	if got := (&DockerRunner{}).userArg(); got != want {
		t.Errorf("default User on %s: got %q, want %q", runtime.GOOS, got, want)
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
			{Name: "read-mount", Required: true, Run: "cat input.txt"},
			{Name: "write-mount", Weight: 1, Run: "echo out > artifact.txt"},
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
		t.Fatalf("bind mount not visible in container: %+v excerpt=%q", outcomes[0], outcomes[0].LogExcerpt)
	}
	if !strings.Contains(outcomes[0].LogExcerpt, "data") {
		t.Errorf("log excerpt: %q", outcomes[0].LogExcerpt)
	}
	if !outcomes[1].Passed {
		t.Errorf("workspace must be writable: %+v", outcomes[1])
	}
	// Filesystem state persists across per-check containers via the mount.
	if !outcomes[2].Passed || !strings.Contains(outcomes[2].LogExcerpt, "out") {
		t.Errorf("artifact must persist between checks: %+v", outcomes[2])
	}
	if outcomes[3].Passed || outcomes[3].ExitCode != 7 {
		t.Errorf("exit code must propagate: %+v", outcomes[3])
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
		Spec:         dockerSpec(2 * time.Second),
		Checks: []config.Check{
			{Name: "slow", Weight: 1, Run: "sleep 60"},
			{Name: "after", Weight: 1, Run: "echo alive"},
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
	if !outcomes[0].TimedOut || outcomes[0].Passed {
		t.Errorf("slow: %+v", outcomes[0])
	}
	if !outcomes[1].Passed {
		t.Errorf("after must run in a fresh container: %+v", outcomes[1])
	}
}

func TestDockerRunnerRejectsUnmountablePath(t *testing.T) {
	// Pure logic, no docker needed; the guard is darwin-only.
	if runtime.GOOS != "darwin" {
		t.Skip("colima mount guard is darwin-only")
	}
	if err := checkMountablePath("/tmp/x"); err == nil {
		t.Fatal("expected error for a /tmp workspace on darwin (colima does not mount it)")
	}
	if err := checkMountablePath(filepath.Join(t.TempDir(), "ws")); err == nil {
		t.Fatal("expected error for a workspace under os.TempDir() on darwin")
	}
}
