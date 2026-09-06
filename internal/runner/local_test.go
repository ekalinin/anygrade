package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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

// TestLocalRunnerLogExcerptSize checks the excerpt honors the task's
// runner.log_excerpt while the log file on disk stays complete (SPEC §13),
// and that an unset size still falls back to the 64 KB default.
func TestLocalRunnerLogExcerptSize(t *testing.T) {
	// 200 lines of 11 bytes = 2200 bytes: well over the 128-byte excerpt
	// below and well under the default.
	const loud = "for i in $(seq 1 200); do echo 0123456789; done"
	const fullSize = 200 * 11

	run := func(excerpt int64) Outcome {
		t.Helper()
		job := localJob(t, time.Minute, []config.Check{{Name: "loud", Weight: 1, Run: loud}})
		job.Spec.LogExcerpt = excerpt
		outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(readFile(t, outcomes[0].LogPath)); got != fullSize {
			t.Errorf("log file: got %d bytes, want the full %d", got, fullSize)
		}
		return outcomes[0]
	}

	const marker = "[...truncated...]\n"
	capped := run(128)
	if !strings.HasPrefix(capped.LogExcerpt, marker) {
		t.Fatalf("excerpt should be marked truncated: %q", capped.LogExcerpt)
	}
	if got := len(strings.TrimPrefix(capped.LogExcerpt, marker)); got != 128 {
		t.Errorf("excerpt: got %d bytes, want 128", got)
	}

	if full := run(0); len(full.LogExcerpt) != fullSize {
		t.Errorf("unset log_excerpt: got %d bytes, want the full %d (default is %d)",
			len(full.LogExcerpt), fullSize, DefaultExcerptSize)
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

// TestLocalRunnerPassesDespitePipeHeldOpen guards against a regression in the
// cmd.WaitDelay fix for issue #123: a command that exits successfully while
// something it left running keeps the output pipe open must still be graded
// by its own exit code once pipeWaitDelay forces the pipe closed, not
// reported as an infra failure.
func TestLocalRunnerPassesDespitePipeHeldOpen(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "leftover.pid")
	var pid int
	t.Cleanup(func() {
		if pid != 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	r := &LocalRunner{}
	job := localJob(t, time.Minute, []config.Check{
		// No setsid: the backgrounded sleep stays in the same process
		// group, it just outlives the shell that spawned it and keeps
		// stdout open well past the command's own successful exit.
		{Name: "leftover", Weight: 1, Run: fmt.Sprintf(
			"sleep 30 & echo $! > %s\necho started\nexit 0\n", shQuote(pidFile))},
	})

	start := time.Now()
	outcomes, err := r.Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > pipeWaitDelay+5*time.Second {
		t.Fatalf("took %v, want under pipeWaitDelay + margin", elapsed)
	}
	if !outcomes[0].Passed || outcomes[0].ExitCode != 0 {
		t.Errorf("leftover: %+v", outcomes[0])
	}
	if !strings.Contains(outcomes[0].LogExcerpt, "still held open") {
		t.Errorf("missing pipe-held-open note: %q", outcomes[0].LogExcerpt)
	}

	if b, err := os.ReadFile(pidFile); err == nil {
		if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			pid = p
		}
	}
}

// leakerHelperEnv, set to "1", turns a re-exec of this test binary into the
// escaped descendant used by TestLocalRunnerSurvivesEscapedDescendant, rather
// than a no-op test run. leakerPidFileEnv carries the path the helper writes
// its own pid to. Both are set on the shell command line itself, not
// inherited from the outer test's environment, so the mechanism does not
// depend on whatever cmd.Env passes through to the check.
const (
	leakerHelperEnv  = "ANYGRADE_TEST_LEAKER"
	leakerPidFileEnv = "ANYGRADE_TEST_LEAKER_PIDFILE"
)

// TestLeakerHelperProcess is not a real test: it does nothing unless it is
// the re-exec target of TestLocalRunnerSurvivesEscapedDescendant. macOS has
// no setsid(1), so the test binary re-execs itself (os.Executable()) to play
// the part of a descendant that detaches into its own session and outlives
// the check's process-group kill, still holding the inherited stdout open.
// It writes its own pid only after Setsid succeeds, which is the handshake
// the outer test waits on before proving the escape actually happened.
func TestLeakerHelperProcess(t *testing.T) {
	if os.Getenv(leakerHelperEnv) != "1" {
		t.Skip("only runs as the re-exec helper for TestLocalRunnerSurvivesEscapedDescendant")
	}
	if _, err := syscall.Setsid(); err != nil {
		t.Fatalf("setsid: %v", err)
	}
	pidFile := os.Getenv(leakerPidFileEnv)
	if pidFile == "" {
		t.Fatalf("missing %s", leakerPidFileEnv)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	time.Sleep(30 * time.Second)
}

// shQuote single-quotes s for embedding in an sh -c command line.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestLocalRunnerSurvivesEscapedDescendant reproduces issue #123: a
// descendant that calls setsid() escapes the check's process group before
// the group kill, keeping the inherited stdout pipe open. Without
// cmd.WaitDelay that wedges cmd.Wait forever, so this test bounds the
// runner's return with a deadline of its own - it must fail (not hang) on
// the current code.
func TestLocalRunnerSurvivesEscapedDescendant(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups and setsid are POSIX-only; the escaped-descendant scenario does not apply on windows")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "leaker.pid")

	var leakerPID int
	t.Cleanup(func() {
		// Safety net: the main body already kills the descendant once it
		// has confirmed the escape, but a Fatal above that point would skip
		// straight here, and it is still asleep in its own session.
		if leakerPID != 0 {
			_ = syscall.Kill(leakerPID, syscall.SIGKILL)
		}
	})

	r := &LocalRunner{}
	// 2s, not the 300ms used elsewhere in this file: nothing here depends on
	// a short timeout (the shell only sleeps past it after the handshake
	// below), and the helper is a re-exec'd go test binary that has to
	// initialize testing, parse -test.run and reach Setsid before it can
	// write its pid file - too tight a budget flakes under parallel package
	// builds instead of proving anything about the escape.
	job := localJob(t, 2*time.Second, []config.Check{
		{
			Name:   "leaker",
			Weight: 1,
			// Backgrounds a re-exec of the test binary that detaches into
			// its own session and writes its own pid only once Setsid has
			// succeeded (TestLeakerHelperProcess); this shell waits for that
			// handshake before its own sleep runs past the check timeout, so
			// the group kill is guaranteed to race a genuinely escaped
			// descendant, not one that has not detached yet.
			Run: fmt.Sprintf(
				"%s=1 %s=%s %s -test.run=^TestLeakerHelperProcess$ &\nwhile [ ! -f %s ]; do sleep 0.05; done\nsleep 30\n",
				leakerHelperEnv, leakerPidFileEnv, shQuote(pidFile), shQuote(exe), shQuote(pidFile),
			),
		},
	})

	done := make(chan struct{})
	var outcomes []Outcome
	var runErr error
	go func() {
		outcomes, runErr = r.Run(t.Context(), job)
		close(done)
	}()

	// The runner must give up on the escaped descendant's pipe within
	// job.Spec.Timeout + pipeWaitDelay plus a margin, never block forever.
	wantWithin := job.Spec.Timeout + pipeWaitDelay + 5*time.Second
	select {
	case <-done:
	case <-time.After(wantWithin):
		t.Fatalf("runner did not return within %v; blocked on the escaped descendant", wantWithin)
	}

	if runErr != nil {
		t.Fatal(runErr)
	}

	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("leaked descendant never completed its post-setsid handshake: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("pid file: %v", err)
	}
	leakerPID = pid
	// Proves the descendant genuinely survived the process-group kill,
	// rather than the assertions below passing vacuously the same way they
	// would for TestLocalRunnerTimeoutKillsProcessGroup.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("escaped descendant (pid %d) did not survive the group kill: %v", pid, err)
	}

	if !outcomes[0].TimedOut || outcomes[0].Passed {
		t.Errorf("leaker: %+v", outcomes[0])
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
