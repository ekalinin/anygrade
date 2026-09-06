package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/testreport"
)

// pipeWaitDelay bounds how long execCheck waits, once a check's own exit is
// observed - whether it exited on its own or was killed after a timeout -
// for its stdout/stderr pipes to close (cmd.WaitDelay, SPEC §13). A
// descendant outside the process group is not reached by a group kill and
// can hold a pipe open indefinitely; ten seconds is generous for a well
// behaved tree to finish flushing and exit on its own.
const pipeWaitDelay = 10 * time.Second

// LocalRunner executes check commands as host processes. It enforces only the
// wall-clock timeout (process-group kill); memory/cpu limits are docker-only
// (SPEC §14). Suitable for `anygrade check` and trusted setups only.
type LocalRunner struct {
	Mirror io.Writer // optional live copy of check output (verbose mode)
}

// Run implements Runner.
func (r *LocalRunner) Run(ctx context.Context, job Job) ([]Outcome, error) {
	return runAll(ctx, job, r)
}

// dropHiddenTests implements checkExecutor: the local runner executes in the
// host workspace itself, so removing the files there is the whole boundary.
func (r *LocalRunner) dropHiddenTests(_ context.Context, job Job) error {
	return dropHiddenTests(job)
}

// readReport implements checkExecutor: the workspace is the host tree itself,
// read through an os.Root so a path that leaves it - a symlink the check
// planted, a `parser_file:` that walks up - is refused rather than followed.
func (r *LocalRunner) readReport(_ context.Context, job Job, rel string) ([]byte, error) {
	root, err := os.OpenRoot(job.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, testreport.MaxInput+1))
}

func (r *LocalRunner) execCheck(ctx context.Context, job Job, c config.Check, command, logPath string) (Outcome, error) {
	log, err := openCheckLog(logPath, c.Name, r.Mirror, job.Spec.LogExcerpt, job.Spec.LogMax)
	if err != nil {
		return Outcome{}, infraErr("workspace", err)
	}
	defer log.Close()

	cctx, cancel := context.WithTimeout(ctx, job.Spec.Timeout)
	defer cancel()

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = filepath.Join(job.WorkspaceDir, filepath.FromSlash(job.TaskRelDir))
	// Both phases agree on where a build may leave what a run executes.
	cmd.Env = append(os.Environ(), artifactsEnv+"="+filepath.Join(job.WorkspaceDir, artifactsDir))
	cmd.Stdout = log
	cmd.Stderr = log
	// Own process group so a timeout kills the whole tree, not just sh.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A descendant that escapes the process group (e.g. by calling setsid)
	// can keep the pipes backing cmd.Stdout/Stderr open long after the
	// command itself exits - normally or via the group kill below - which
	// would otherwise wedge cmd.Wait forever (issue #123). Give up on the
	// pipes after this grace period instead of blocking the worker; the
	// ordinary-exit branch below turns the resulting exec.ErrWaitDelay back
	// into a verdict rather than an infra failure.
	cmd.WaitDelay = pipeWaitDelay

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Outcome{}, infraErr("runner_exec", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timedOut := false
	select {
	case <-cctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		if ctx.Err() != nil {
			// The parent was canceled (worker shutdown), not a per-check timeout.
			return Outcome{}, infraErr("canceled", ctx.Err())
		}
		timedOut = true
		fmt.Fprintf(log, "\nanygrade: timed out after %s\n", job.Spec.Timeout)
	case err := <-done:
		if errors.Is(err, exec.ErrWaitDelay) {
			// The command exited on its own; pipeWaitDelay fired only
			// because something it left running kept the pipes open.
			// cmd.ProcessState already has the real exit status, so judge
			// the check by that instead of an infra failure.
			fmt.Fprintf(log, "\nanygrade: output still held open %s after the command exited\n", pipeWaitDelay)
		} else if _, ok := errors.AsType[*exec.ExitError](err); err != nil && !ok {
			return Outcome{}, infraErr("runner_exec", err)
		}
	}

	exit := cmd.ProcessState.ExitCode()
	return Outcome{
		Name:       c.Name,
		Passed:     !timedOut && exit == 0,
		ExitCode:   exit,
		Duration:   time.Since(start),
		TimedOut:   timedOut,
		LogPath:    logPath,
		LogExcerpt: log.Excerpt(),
	}, nil
}
