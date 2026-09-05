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
		if _, ok := errors.AsType[*exec.ExitError](err); err != nil && !ok {
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
