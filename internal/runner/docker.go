package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
)

// DockerRunner executes each check in its own ephemeral container
// (`docker run --rm`) over a shared workspace bind mount. One container per
// check (not per submission) so a timed-out check can be killed cleanly with
// `docker kill` while the remaining checks continue in fresh containers;
// filesystem state persists across checks via the shared mount.
type DockerRunner struct {
	// User is the container --user value ("uid:gid"). Empty = image default;
	// the server passes its non-root service uid on Linux (SPEC §14). On
	// macOS/colima it must stay empty: virtiofs remaps bind mounts to
	// root:root, so only root can write to the workspace.
	User   string
	Mirror io.Writer // optional live copy of check output
}

const containerWorkdir = "/work"

// Run implements Runner.
func (r *DockerRunner) Run(ctx context.Context, job Job) ([]Outcome, error) {
	if err := checkMountablePath(job.WorkspaceDir); err != nil {
		return nil, err
	}
	if err := r.ensureImage(ctx, job.Spec.Image); err != nil {
		return nil, err
	}
	return runAll(ctx, job, r)
}

// checkMountablePath rejects workspace locations that colima does not mount
// into the docker VM on macOS: a bind mount from /tmp or /var/folders is
// silently EMPTY inside the container.
func checkMountablePath(dir string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	for _, p := range []string{os.TempDir(), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"} {
		if p != "" && strings.HasPrefix(dir, p) {
			return infraErr("workspace", fmt.Errorf(
				"workspace %s is under %s, which colima does not mount into the docker VM; place the data dir under your home directory", dir, p))
		}
	}
	return nil
}

// ensureImage checks the image is present, pulling it if not. Front-loading
// the pull keeps per-check runs fast and turns pull/daemon failures into a
// single InfraError before any check runs.
func (r *DockerRunner) ensureImage(ctx context.Context, image string) error {
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", image).Run(); err == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput()
	if err != nil {
		return infraErr("image_pull", fmt.Errorf("docker pull %s: %v: %s", image, err, lastLine(out)))
	}
	return nil
}

func (r *DockerRunner) execCheck(ctx context.Context, job Job, c config.Check, logPath string) (Outcome, error) {
	log, err := openCheckLog(logPath, r.Mirror)
	if err != nil {
		return Outcome{}, infraErr("workspace", err)
	}
	defer log.Close()

	name := "ag-" + randHex(10)
	args := []string{
		"run", "--rm", "--name", name,
		"--network", job.Spec.Network,
		"--memory", strconv.FormatInt(job.Spec.Memory, 10),
		"--memory-swap", strconv.FormatInt(job.Spec.Memory, 10), // swap == memory: no swap
		"--cpus", strconv.FormatFloat(job.Spec.CPUs, 'f', -1, 64),
		"--pids-limit", "512",
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,mode=1777,size=512m",
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"-v", job.WorkspaceDir + ":" + containerWorkdir,
		"-w", path.Join(containerWorkdir, job.TaskRelDir),
		// Redirect HOME and Go caches to the writable tmpfs so builds work
		// read-only and non-root regardless of the image's default user.
		"-e", "HOME=/tmp",
		"-e", "GOCACHE=/tmp/.cache/go-build",
		"-e", "GOMODCACHE=/tmp/go/pkg/mod",
		"-e", "GOPATH=/tmp/go",
	}
	if r.User != "" {
		args = append(args, "--user", r.User)
	}
	args = append(args, "--entrypoint", "sh", job.Spec.Image, "-c", c.Run)

	cctx, cancel := context.WithTimeout(ctx, job.Spec.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "docker", args...)
	cmd.Stdout = log
	cmd.Stderr = log

	// Killing the docker CLI does NOT stop the container; an explicit
	// `docker kill` is required. Fire it when cctx ends, unless stopped first.
	stopKill := context.AfterFunc(cctx, func() {
		kctx, kcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer kcancel()
		_ = exec.CommandContext(kctx, "docker", "kill", name).Run()
	})
	// Belt and braces: --rm removes the container on the happy path, this
	// covers kill/cancel races. Runs on a fresh context so it survives cancel.
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rcancel()
		_ = exec.CommandContext(rctx, "docker", "rm", "-f", name).Run()
	}()

	start := time.Now()
	err = cmd.Run()
	stopKill() // no-op if the kill hook already fired
	dur := time.Since(start)

	timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
	if ctx.Err() != nil {
		return Outcome{}, infraErr("canceled", ctx.Err())
	}
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); !ok && !timedOut {
			return Outcome{}, infraErr("runner_exec", err)
		}
	}
	exit := cmd.ProcessState.ExitCode()
	// docker's own exit-code contract: 125 = daemon/create error (infra);
	// 126/127 and everything else = the command ran and failed (a real check
	// failure, charged to the submission).
	if exit == 125 && !timedOut {
		return Outcome{}, infraErr("container_create", fmt.Errorf("docker run failed: %s", lastLine([]byte(log.Excerpt()))))
	}
	if timedOut {
		fmt.Fprintf(log, "\nanygrade: timed out after %s\n", job.Spec.Timeout)
	}

	return Outcome{
		Name:       c.Name,
		Passed:     !timedOut && exit == 0,
		ExitCode:   exit,
		Duration:   dur,
		TimedOut:   timedOut,
		LogPath:    logPath,
		LogExcerpt: log.Excerpt(),
	}, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func lastLine(out []byte) string {
	s := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
