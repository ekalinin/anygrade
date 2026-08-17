// Package runner executes a submission's checks in an assembled workspace.
// Two implementations exist: LocalRunner (host processes, wall-clock timeout
// only) and DockerRunner (containers with cpu/memory/network limits). See
// SPEC §5, §13, §14.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
)

// Job describes one submission run: all checks of one task over one assembled
// workspace.
type Job struct {
	// WorkspaceDir is the assembled workspace root (copied into the docker
	// runner's tmpfs /work). Must be built by Assemble.
	WorkspaceDir string
	// TaskRelDir is the task directory relative to WorkspaceDir (slash-
	// separated); it is the working directory of every check command.
	TaskRelDir string
	Spec       config.ResolvedRunner
	Checks     []config.Check // run in order
	LogDir     string         // per-check log files are written here
	// ExportWorkspace copies the container's /work back onto WorkspaceDir when
	// the run finishes (docker runner only). The workspace lives in a tmpfs
	// inside the container, so this is the only way files a check produced
	// reach the host: `anygrade check --keep` asks for it, the server does not
	// (it deletes the workspace right after the run).
	ExportWorkspace bool
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
	LogExcerpt string // tail of the log, at most Job.Spec.LogExcerpt bytes
}

// Runner executes all checks of one submission in order. A non-nil error is
// always an *InfraError (environment problem: retryable, must not consume a
// submission attempt). Per-check pass/fail/timeout lives in the Outcome slice.
type Runner interface {
	Run(ctx context.Context, job Job) ([]Outcome, error)
}

// New returns the runner for spec.Type. dockerUser is the --user value for
// containers ("" = the uid:gid of this process, or a fixed unprivileged id
// when that is root; see containerUser).
// mirror, when non-nil, receives a live copy of all check output.
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
	// Owner-only: check logs hold the full output of a student's run, and the
	// data dir around them is 0700 as well.
	if err := os.MkdirAll(job.LogDir, 0o700); err != nil {
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

const (
	// logStemSep separates a sanitized stem from its hash. It is never produced
	// by sanitizeLogStem and disqualifies a name from safeLogStem, so it cannot
	// occur in a file name built from a name that was kept verbatim.
	logStemSep = "~"
	// maxLogStem keeps the file name well inside the 255-byte limit even after
	// the separator, the hash and ".log".
	maxLogStem = 64
)

// logFileName maps a check name to a safe file name, injectively: check names
// that are already file-name safe keep their spelling, everything else is
// sanitized and tagged with a hash of the original name. Without the tag
// "a/b", "a b" and "a_b" would all write to a_b.log and silently overwrite
// each other's logs.
func logFileName(name string) string {
	if safeLogStem(name) {
		return name + ".log"
	}
	return sanitizeLogStem(name) + logStemSep + shortHash(name) + ".log"
}

// safeLogStem reports whether name can be used as a file name as is. The
// alphabet is deliberately narrow: uppercase is excluded because macOS is
// case-insensitive by default, where "Build" and "build" would be one file.
func safeLogStem(name string) bool {
	if name == "" || len(name) > maxLogStem {
		return false
	}
	if name[0] == '.' || name[0] == '-' {
		return false // hidden file / leading dash
	}
	for i := range len(name) {
		if !safeStemByte(name[i]) {
			return false
		}
	}
	return true
}

func safeStemByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '.' || b == '-' || b == '_':
		return true
	}
	return false
}

// readableStemByte is safeStemByte plus uppercase: the hash already separates
// names that differ only in case, so the stem may keep the original spelling.
func readableStemByte(b byte) bool { return safeStemByte(b) || (b >= 'A' && b <= 'Z') }

// sanitizeLogStem keeps the name readable: everything else (the separator and
// any non-ASCII byte included) becomes "_".
func sanitizeLogStem(name string) string {
	if len(name) > maxLogStem {
		name = name[:maxLogStem]
	}
	b := []byte(name)
	for i := range b {
		if !readableStemByte(b[i]) {
			b[i] = '_'
		}
	}
	return string(b)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// LogFileName is logFileName for other packages: the web layer must tail
// exactly the files the runner writes.
func LogFileName(name string) string { return logFileName(name) }
