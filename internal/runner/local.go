package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
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

func (r *LocalRunner) execCheck(ctx context.Context, job Job, c config.Check, logPath string) (Outcome, error) {
	log, err := openCheckLog(logPath, r.Mirror, job.Spec.LogExcerpt)
	if err != nil {
		return Outcome{}, infraErr("workspace", err)
	}
	defer log.Close()

	cctx, cancel := context.WithTimeout(ctx, job.Spec.Timeout)
	defer cancel()

	cmd := exec.Command("sh", "-c", c.Run)
	cmd.Dir = filepath.Join(job.WorkspaceDir, filepath.FromSlash(job.TaskRelDir))
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
