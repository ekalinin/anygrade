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
	"github.com/ekalinin/anygrade/internal/testreport"
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
	// HiddenPaths lists the workspace-relative (slash-separated) files the
	// hidden-tests overlay wrote, as reported by Assemble. They are removed
	// between the build and the run phases when the task has any build phase
	// at all - the execution boundary of SPEC §6.1. Empty means there is
	// nothing to remove and the boundary is a no-op.
	HiddenPaths []string
	// HiddenDirs are the directories of that same overlay, removed after the
	// files when they are left empty, so a directory name cannot outlive the
	// sources it held.
	HiddenDirs []string
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
	// BuildFailed says the check never reached its run phase because its build
	// phase failed. LogPath and LogExcerpt are then empty on purpose: the only
	// output this check produced came from the phase that read the hidden
	// tests, and that one is teacher-only (SPEC §14). BuildLogPath points at
	// it for the callers that are allowed to look.
	BuildFailed  bool
	BuildLogPath string
	// Cases are the per-test-case results of a check that declared a
	// `parser:`, parsed out of its run phase (SPEC §4.3). Empty for every
	// other check - and for one whose report could not be read, which is what
	// ParseFailed says.
	Cases []testreport.Case
	// ParseFailed marks a check whose parser produced nothing usable. The
	// check keeps the verdict of its exit code: an unreadable report is the
	// parser's fault and never the student's.
	ParseFailed bool
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

// checkExecutor runs the phases of a run; implemented by both runners. A
// non-nil error must be an *InfraError and aborts the whole run.
type checkExecutor interface {
	// execCheck runs one phase of check c (cmd is c.Build or c.Run) and writes
	// its output to logPath.
	execCheck(ctx context.Context, job Job, c config.Check, cmd, logPath string) (Outcome, error)
	// dropHiddenTests takes the hidden-test sources out of the workspace the
	// run phases will execute in. It is called once, between the two sweeps of
	// runAll, and only when there is something to remove.
	dropHiddenTests(ctx context.Context, job Job) error
	// readReport returns the contents of a workspace file a check wrote
	// (`parser_file:`), given a workspace-relative slash path. Only the
	// executor can reach it: the workspace is a host directory for the local
	// runner and a tmpfs inside the container for the docker one.
	readReport(ctx context.Context, job Job, rel string) ([]byte, error)
}

// runAll is the shared driver. A check is up to two phases (SPEC §4.3): every
// build phase runs first, in check order; then the hidden-test sources leave
// the workspace; then every run phase, in check order. A task whose checks
// declare no build phase skips the first two steps entirely and behaves
// exactly as it did before the boundary existed.
//
// Both sweeps share one stop index, which is what makes the gates coherent
// across the split. A gate that fails takes everything after it out of the
// run, whichever phase it failed in: at build time the builds that follow are
// pointless, at run time their builds already happened and the wasted work is
// simply accepted - re-ordering the sweeps to avoid it would put student code
// on disk next to the hidden tests again, which is the whole thing being
// bought here. Checks *before* the failed gate keep both of their phases and
// report a real result, exactly as they did when a check was one command.
//
// Each phase gets the task's full `runner.timeout`: a phase is one command and
// the timeout has always been a per-command wall clock. A check with both
// phases can therefore occupy 2×timeout (see dockerSession.lifetime).
func runAll(ctx context.Context, job Job, ex checkExecutor) ([]Outcome, error) {
	// Owner-only: check logs hold the full output of a student's run, and the
	// data dir around them is 0700 as well.
	if err := os.MkdirAll(job.LogDir, 0o700); err != nil {
		return nil, infraErr("workspace", err)
	}
	// Pre-seed every check as skipped: whatever neither sweep reaches keeps
	// that verdict, so a gate needs no bookkeeping beyond the stop index.
	outcomes := make([]Outcome, len(job.Checks))
	for i, c := range job.Checks {
		outcomes[i] = Outcome{Name: c.Name, Skipped: true}
	}
	stop := len(job.Checks)
	// What each check spent in its build phase. A check costs both of its
	// phases, so the duration it reports is their sum - a check that takes 40s
	// to compile and 2s to run is not a 2s check.
	buildDur := make([]time.Duration, len(job.Checks))

	if config.HasBuildPhase(job.Checks) {
		if err := os.MkdirAll(BuildLogDir(job.LogDir), 0o700); err != nil {
			return nil, infraErr("workspace", err)
		}
		for i, c := range job.Checks {
			if i >= stop {
				break
			}
			if c.Build == "" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, infraErr("canceled", err)
			}
			o, err := ex.execCheck(ctx, job, c, c.Build, buildLogPath(job.LogDir, c.Name))
			if err != nil {
				return nil, err
			}
			if o.Passed {
				buildDur[i] = o.Duration
				continue // the run phase decides the check
			}
			// The build phase is the one that reads the hidden tests, so none
			// of its output may reach the student: no excerpt, no log for the
			// live stream to tail, only the teacher-only file (SPEC §14).
			o.BuildFailed, o.BuildLogPath = true, o.LogPath
			o.LogPath, o.LogExcerpt = "", ""
			outcomes[i] = o
			if c.Required {
				stop = i + 1
			}
		}
		if len(job.HiddenPaths) > 0 {
			if err := ex.dropHiddenTests(ctx, job); err != nil {
				return nil, err
			}
		}
	}

	for i, c := range job.Checks {
		if i >= stop || outcomes[i].BuildFailed {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, infraErr("canceled", err)
		}
		o, err := ex.execCheck(ctx, job, c, c.Run, filepath.Join(job.LogDir, logFileName(c.Name)))
		if err != nil {
			return nil, err
		}
		o.Duration += buildDur[i]
		// The run phase is the only one a parser ever reads: the build phase's
		// output is teacher-only, so it cannot feed a student-visible list of
		// cases (SPEC §14).
		attachCases(ctx, job, c, &o, ex)
		outcomes[i] = o
		if c.Required && !o.Passed {
			stop = i + 1
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

// buildLogSubdir holds the build-phase logs. A subdirectory rather than a
// suffixed file name for two reasons: it stays injective for free (a check
// named "x" and a check named "x.build" would otherwise fight over
// x.build.log), and the teacher-only rule becomes structural - the student's
// live stream tails LogDir and never descends into it (SPEC §14).
const buildLogSubdir = "build"

// BuildLogDir is where the build-phase logs of a submission live. The file
// inside it keeps the check's ordinary LogFileName.
func BuildLogDir(logDir string) string { return filepath.Join(logDir, buildLogSubdir) }

func buildLogPath(logDir, check string) string {
	return filepath.Join(BuildLogDir(logDir), logFileName(check))
}
