package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
)

// hiddenJob is testJob with something for the boundary to remove, which is what
// arms it (an empty HiddenPaths makes dropHiddenTests a no-op).
func hiddenJob(t *testing.T, checks []config.Check) Job {
	t.Helper()
	job := testJob(t, checks)
	job.HiddenPaths = []string{"tasks/x/hidden_test.go"}
	return job
}

// TestRunAllPhaseOrder pins the execution order of SPEC §6.1: every build
// phase, then the removal of the hidden tests, then every run phase. Not
// build/run per check - that would put the first check's run phase on disk
// beside the hidden tests the second check still has to compile against.
func TestRunAllPhaseOrder(t *testing.T) {
	checks := []config.Check{
		{Name: "a", Weight: 1, Build: "build-a", Run: "run-a"},
		{Name: "b", Weight: 1, Run: "run-b"}, // no build phase: run only
		{Name: "c", Weight: 1, Build: "build-c", Run: "run-c"},
	}
	ex := &fakeExecutor{}
	outcomes, err := runAll(t.Context(), hiddenJob(t, checks), ex)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a:build", "c:build", "<boundary>", "a", "b", "c"}
	if !slices.Equal(ex.ran, want) {
		t.Errorf("phase order: got %v, want %v", ex.ran, want)
	}
	for _, o := range outcomes {
		if !o.Passed || o.Skipped || o.BuildFailed {
			t.Errorf("%s: %+v", o.Name, o)
		}
	}
}

// TestRunAllNoBuildPhaseKeepsHiddenTests is the compatibility guarantee: a
// course whose checks are all run-only must behave exactly as before, hidden
// tests included. Removing them there would break every existing course, since
// a run-only check is how hidden tests have always been executed.
func TestRunAllNoBuildPhaseKeepsHiddenTests(t *testing.T) {
	ex := &fakeExecutor{}
	_, err := runAll(t.Context(), hiddenJob(t, []config.Check{{Name: "a", Weight: 1, Run: "run-a"}}), ex)
	if err != nil {
		t.Fatal(err)
	}
	if ex.dropped != 0 {
		t.Errorf("a task with no build phase must not lose its hidden tests, ran: %v", ex.ran)
	}
}

// TestRunAllBoundaryNeedsHiddenTests: with a build phase but no hidden-test
// overlay there is nothing to remove, and the docker runner's boundary costs a
// container round trip, so it must not fire.
func TestRunAllBoundaryNeedsHiddenTests(t *testing.T) {
	ex := &fakeExecutor{}
	job := testJob(t, []config.Check{{Name: "a", Weight: 1, Build: "build-a", Run: "run-a"}})
	if _, err := runAll(t.Context(), job, ex); err != nil {
		t.Fatal(err)
	}
	if ex.dropped != 0 {
		t.Errorf("nothing to remove, but the boundary ran: %v", ex.ran)
	}
}

// TestRunAllGateFailsAtBuild: a gate that fails while being built stops
// everything after it - the later builds are pointless and the later runs must
// not count - while the checks before it still get both of their phases and a
// real result, exactly as they did when a check was one command.
func TestRunAllGateFailsAtBuild(t *testing.T) {
	checks := []config.Check{
		{Name: "first", Weight: 1, Build: "build-first", Run: "run-first"},
		{Name: "gate", Required: true, Build: "build-gate", Run: "run-gate"},
		{Name: "last", Weight: 1, Build: "build-last", Run: "run-last"},
	}
	ex := &fakeExecutor{fail: map[string]bool{"build-gate": true}}
	outcomes, err := runAll(t.Context(), hiddenJob(t, checks), ex)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first:build", "gate:build", "<boundary>", "first"}
	if !slices.Equal(ex.ran, want) {
		t.Errorf("phases: got %v, want %v", ex.ran, want)
	}
	if !outcomes[0].Passed {
		t.Errorf("a check before the gate keeps its real result: %+v", outcomes[0])
	}
	gate := outcomes[1]
	switch {
	case gate.Passed || gate.Skipped:
		t.Errorf("gate must be a plain failure, not a skip: %+v", gate)
	case !gate.BuildFailed:
		t.Errorf("gate must be marked as failed in its build phase: %+v", gate)
	case gate.LogExcerpt != "" || gate.LogPath != "":
		t.Errorf("the build phase reads the hidden tests: nothing of it may reach the student: %+v", gate)
	case gate.BuildLogPath == "":
		t.Errorf("the teacher-only log path is missing: %+v", gate)
	}
	if !outcomes[2].Skipped {
		t.Errorf("everything after a failed gate is skipped: %+v", outcomes[2])
	}
}

// TestRunAllGateFailsAtRun: the builds of the later checks already happened by
// the time a gate fails at run time. That work is wasted, but their run phases
// must still be skipped - a gate failure scores the submission 0, and a check
// that never ran must not report a result.
func TestRunAllGateFailsAtRun(t *testing.T) {
	checks := []config.Check{
		{Name: "gate", Required: true, Build: "build-gate", Run: "run-gate"},
		{Name: "last", Weight: 1, Build: "build-last", Run: "run-last"},
	}
	ex := &fakeExecutor{fail: map[string]bool{"run-gate": true}}
	outcomes, err := runAll(t.Context(), hiddenJob(t, checks), ex)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gate:build", "last:build", "<boundary>", "gate"}
	if !slices.Equal(ex.ran, want) {
		t.Errorf("phases: got %v, want %v", ex.ran, want)
	}
	if outcomes[0].Passed || outcomes[0].BuildFailed {
		t.Errorf("gate failed in its run phase: %+v", outcomes[0])
	}
	if !outcomes[1].Skipped {
		t.Errorf("the run phase after a failed gate is skipped even though its build ran: %+v", outcomes[1])
	}
}

// TestRunAllNonGateBuildFailure: a build failure on an ordinary check is that
// check's failure and nothing more - the checks after it keep running, and the
// weights of the ones that pass still score.
func TestRunAllNonGateBuildFailure(t *testing.T) {
	checks := []config.Check{
		{Name: "a", Weight: 1, Build: "build-a", Run: "run-a"},
		{Name: "b", Weight: 1, Build: "build-b", Run: "run-b"},
	}
	ex := &fakeExecutor{fail: map[string]bool{"build-a": true}}
	outcomes, err := runAll(t.Context(), hiddenJob(t, checks), ex)
	if err != nil {
		t.Fatal(err)
	}
	if !outcomes[0].BuildFailed || outcomes[0].Passed || outcomes[0].Skipped {
		t.Errorf("a: %+v", outcomes[0])
	}
	if !outcomes[1].Passed {
		t.Errorf("b must still run after a non-gate build failure: %+v", outcomes[1])
	}
	if slices.Contains(ex.ran, "a") {
		t.Errorf("the run phase of a check whose build failed must not run: %v", ex.ran)
	}
}

// TestBuildLogsAreSeparateFiles: the build phase writes into its own
// subdirectory. That is what keeps the student's live stream - which tails
// LogDir by check name - from ever reading the phase that saw the hidden
// tests, and it keeps the names injective: a check "x" and a check "x.build"
// would collide under any suffix scheme.
func TestBuildLogsAreSeparateFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"x", "x.build", "build", "a/b"} {
		run := filepath.Join(dir, LogFileName(name))
		build := buildLogPath(dir, name)
		if run == build {
			t.Errorf("%q: the two phases share one log file %q", name, run)
		}
		if filepath.Dir(build) != BuildLogDir(dir) {
			t.Errorf("%q: build log %q is outside %q", name, build, BuildLogDir(dir))
		}
	}
	// Both phases of "x" against the run log of "x.build": three distinct files.
	seen := map[string]string{}
	for _, p := range []string{
		filepath.Join(dir, LogFileName("x")), buildLogPath(dir, "x"),
		filepath.Join(dir, LogFileName("x.build")), buildLogPath(dir, "x.build"),
	} {
		key := strings.ToLower(p) // macOS is case-insensitive by default
		if prev, dup := seen[key]; dup {
			t.Errorf("%s collides with %s", p, prev)
		}
		seen[key] = p
	}
}

// TestLocalRunnerHiddenTestBoundary is the guarantee itself, end to end on a
// real workspace: the build phase can read a hidden test, the run phase of the
// same submission cannot, and what the build phase left in $ANYGRADE_ARTIFACTS
// survives the removal - otherwise the compiled tests would have nothing to
// execute.
func TestLocalRunnerHiddenTestBoundary(t *testing.T) {
	const secret = "hidden-expectations-42"
	ws := assembleWithHidden(t, secret)

	job := Job{
		WorkspaceDir: ws.Root,
		TaskRelDir:   "tasks/01",
		Spec:         config.ResolvedRunner{Type: "local", Timeout: time.Minute},
		Checks: []config.Check{{
			Name:   "boundary",
			Weight: 1,
			// Stands in for `go test -c`: reads the hidden test, compiles an
			// artifact from it, never executes the solution. It also echoes
			// what it read, so the build log proves the phase really could see
			// the file the run phase cannot.
			Build: `cat hidden_test.txt > "$ANYGRADE_ARTIFACTS/compiled"; cat hidden_test.txt`,
			// Stands in for running that artifact: the expectations are still
			// available, the source they came from is not.
			Run: `cat "$ANYGRADE_ARTIFACTS/compiled"; ` +
				`if [ -e hidden_test.txt ]; then echo LEAKED; else echo GONE; fi`,
		}},
		LogDir:      filepath.Join(t.TempDir(), "logs"),
		HiddenPaths: ws.HiddenPaths,
		HiddenDirs:  ws.HiddenDirs,
	}
	outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	o := outcomes[0]
	if !o.Passed {
		t.Fatalf("boundary check failed: %+v excerpt=%q", o, o.LogExcerpt)
	}
	if strings.Contains(o.LogExcerpt, "LEAKED") {
		t.Errorf("the hidden test source was still on disk in the run phase: %q", o.LogExcerpt)
	}
	if !strings.Contains(o.LogExcerpt, "GONE") {
		t.Errorf("run phase did not report on the hidden test: %q", o.LogExcerpt)
	}
	if !strings.Contains(o.LogExcerpt, secret) {
		t.Errorf("the build artifact did not survive the boundary: %q", o.LogExcerpt)
	}
	// The build phase really could read it, so the run phase's failure to is
	// the boundary and not a broken fixture.
	if got := readFile(t, buildLogPath(job.LogDir, "boundary")); !strings.Contains(got, secret) {
		t.Errorf("the build phase should have read the hidden test: %q", got)
	}
	// And the file is gone from the workspace itself, not just from the view
	// the check had.
	if _, err := os.Stat(filepath.Join(ws.Root, "tasks/01/hidden_test.txt")); !os.IsNotExist(err) {
		t.Errorf("hidden test still in the workspace: %v", err)
	}
}

// TestLocalRunnerBuildFailureIsTeacherOnly: a compiler failing against the
// hidden tests quotes their source, so the failure must reach the student as
// the bare fact that the build failed. The output goes to the teacher-only
// file and nowhere else - no excerpt for the database, no run-phase log for
// the live stream to tail.
func TestLocalRunnerBuildFailureIsTeacherOnly(t *testing.T) {
	const secret = "hidden_test.txt:7: undefined: Solve"
	ws := assembleWithHidden(t, "unused")

	job := Job{
		WorkspaceDir: ws.Root,
		TaskRelDir:   "tasks/01",
		Spec:         config.ResolvedRunner{Type: "local", Timeout: time.Minute},
		Checks: []config.Check{
			{Name: "compiles", Weight: 1, Build: "echo '" + secret + "' >&2; exit 2", Run: "echo unreachable"},
			{Name: "after", Weight: 1, Run: "echo still runs"},
		},
		LogDir:      filepath.Join(t.TempDir(), "logs"),
		HiddenPaths: ws.HiddenPaths,
		HiddenDirs:  ws.HiddenDirs,
	}
	outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	o := outcomes[0]
	if o.Passed || o.Skipped || !o.BuildFailed || o.ExitCode != 2 {
		t.Fatalf("compiles: %+v", o)
	}
	if o.LogExcerpt != "" || o.LogPath != "" {
		t.Errorf("build output must not reach the student: excerpt=%q log=%q", o.LogExcerpt, o.LogPath)
	}
	if _, err := os.Stat(filepath.Join(job.LogDir, LogFileName("compiles"))); !os.IsNotExist(err) {
		t.Errorf("a run-phase log the live stream would tail must not exist: %v", err)
	}
	if got := readFile(t, o.BuildLogPath); !strings.Contains(got, secret) {
		t.Errorf("teacher-only build log missing the output: %q", got)
	}
	if !outcomes[1].Passed {
		t.Errorf("a non-gate build failure must not stop the run: %+v", outcomes[1])
	}
}

// TestPhaseBudgetIsPerPhase pins the timeout as per-phase (SPEC §4.3): a phase
// is one command, and the timeout has always been the wall clock of one
// command. Both phases here fit the budget on their own and would blow it
// together, so a shared clock fails the check. The reported duration is their
// sum for the same reason: a check that takes a long time to compile and no
// time to run is not a fast check, and the teacher setting `runner.timeout`
// has to see it.
func TestPhaseBudgetIsPerPhase(t *testing.T) {
	// Whole seconds: fractional sleeps are not POSIX. 2+2 over a 3s budget
	// leaves a second of slack per phase and still fails a shared clock.
	job := localJob(t, 3*time.Second, []config.Check{
		{Name: "two-phase", Weight: 1, Build: "sleep 2", Run: "sleep 2"},
	})
	outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	o := outcomes[0]
	if !o.Passed || o.TimedOut {
		t.Fatalf("each phase gets the full timeout: %+v", o)
	}
	if o.Duration < 3500*time.Millisecond {
		t.Errorf("duration %v does not cover both phases", o.Duration)
	}
}

// TestLocalRunnerBuildTimeout: a build phase that times out is that check's
// failure, in its build phase - so no run phase, and the checks after it are
// untouched because it is not a gate.
func TestLocalRunnerBuildTimeout(t *testing.T) {
	job := localJob(t, 300*time.Millisecond, []config.Check{
		{Name: "slow-build", Weight: 1, Build: "sleep 30 & wait", Run: "echo unreachable"},
		{Name: "after", Weight: 1, Run: "echo still runs"},
	})
	outcomes, err := (&LocalRunner{}).Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !outcomes[0].TimedOut || !outcomes[0].BuildFailed || outcomes[0].Passed {
		t.Errorf("slow-build: %+v", outcomes[0])
	}
	// The timeout note belongs to the build phase, so it goes to the
	// teacher-only log and not to the student's excerpt.
	if outcomes[0].LogExcerpt != "" {
		t.Errorf("a timed-out build must not leave a student-visible excerpt: %q", outcomes[0].LogExcerpt)
	}
	if got := readFile(t, outcomes[0].BuildLogPath); !strings.Contains(got, "timed out after") {
		t.Errorf("build log missing the timeout note: %q", got)
	}
	// The clock the previous check burned is not this one's.
	if !outcomes[1].Passed {
		t.Errorf("after: %+v", outcomes[1])
	}
}

// assembleWithHidden builds a workspace whose task dir carries one hidden test
// file with the given content.
func assembleWithHidden(t *testing.T, secret string) *Workspace {
	t.Helper()
	course := t.TempDir()
	writeFiles(t, course, map[string]string{"tasks/01/main.sh": "echo main\n"})
	hidden := t.TempDir()
	writeFiles(t, hidden, map[string]string{"hidden_test.txt": secret + "\n"})

	ws, err := Assemble(t.Context(), Assembly{
		Dest:          filepath.Join(t.TempDir(), "ws"),
		Task:          config.ResolvedTask{SolutionFiles: []string{"main.sh"}},
		TaskRelDir:    "tasks/01",
		Authoritative: WorkingCopySource{Root: course},
		Hidden:        WorkingCopySource{Root: hidden},
		RunAsUID:      -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}
