// Package runner executes a submission's checks in an assembled workspace.
// Two implementations exist: LocalRunner (host processes, wall-clock timeout
// only) and DockerRunner (containers with cpu/memory/network limits). See
// SPEC §5, §13, §14.
package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
)

// Job describes one submission run: all checks of one task over one assembled
// workspace.
type Job struct {
	// WorkspaceDir is the assembled workspace root (bind-mounted at /work by
	// the docker runner). Must be built by Assemble.
	WorkspaceDir string
	// TaskRelDir is the task directory relative to WorkspaceDir (slash-
	// separated); it is the working directory of every check command.
	TaskRelDir string
	Spec       config.ResolvedRunner
	Checks     []config.Check // run in order
	LogDir     string         // per-check log files are written here
}

// Outcome is the result of one check.
type Outcome struct {
	Name       string
	Passed     bool
	ExitCode   int
	Duration   time.Duration
	TimedOut   bool
	Skipped    bool // an earlier gate failed; this check did not run
	LogPath    string
	LogExcerpt string // tail of the log, at most DefaultExcerptSize bytes
}

// Runner executes all checks of one submission in order. A non-nil error is
// always an *InfraError (environment problem: retryable, must not consume a
// submission attempt). Per-check pass/fail/timeout lives in the Outcome slice.
type Runner interface {
	Run(ctx context.Context, job Job) ([]Outcome, error)
}

// New returns the runner for spec.Type. dockerUser is the --user value for
// containers ("" = image default; the server passes its service uid:gid on
// Linux). mirror, when non-nil, receives a live copy of all check output.
func New(spec config.ResolvedRunner, dockerUser string, mirror io.Writer) (Runner, error) {
	switch spec.Type {
	case "local":
		return &LocalRunner{Mirror: mirror}, nil
	case "docker":
		return &DockerRunner{User: dockerUser, Mirror: mirror}, nil
	default:
		return nil, fmt.Errorf("unknown runner type %q", spec.Type)
	}
}

// InfraError marks an infrastructure failure (docker daemon down, image pull
// failed, workspace I/O error). Submissions hitting it are retried and do not
// consume an attempt (SPEC §13).
type InfraError struct {
	Op  string // "workspace" | "image_pull" | "container_create" | "runner_exec" | "canceled"
	Err error
}

func (e *InfraError) Error() string { return fmt.Sprintf("infra error (%s): %v", e.Op, e.Err) }
func (e *InfraError) Unwrap() error { return e.Err }

func infraErr(op string, err error) *InfraError { return &InfraError{Op: op, Err: err} }

// checkExecutor runs a single check; implemented by both runners. A non-nil
// error must be an *InfraError and aborts the whole run.
type checkExecutor interface {
	execCheck(ctx context.Context, job Job, c config.Check, logPath string) (Outcome, error)
}

// runAll is the shared driver: it iterates checks in order, applies the gate
// short-circuit (after a failed required check the remaining checks are
// reported Skipped), and honors parent-context cancellation.
func runAll(ctx context.Context, job Job, ex checkExecutor) ([]Outcome, error) {
	if err := os.MkdirAll(job.LogDir, 0o755); err != nil {
		return nil, infraErr("workspace", err)
	}
	outcomes := make([]Outcome, 0, len(job.Checks))
	gateFailed := false
	for _, c := range job.Checks {
		if gateFailed {
			outcomes = append(outcomes, Outcome{Name: c.Name, Skipped: true})
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, infraErr("canceled", err)
		}
		logPath := filepath.Join(job.LogDir, logFileName(c.Name))
		o, err := ex.execCheck(ctx, job, c, logPath)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, o)
		if c.Required && !o.Passed {
			gateFailed = true
		}
	}
	return outcomes, nil
}

// logFileName maps a check name to a safe file name.
func logFileName(name string) string {
	r := strings.NewReplacer("/", "_", " ", "_", "\t", "_")
	return r.Replace(name) + ".log"
}
