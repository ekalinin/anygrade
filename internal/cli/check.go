package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/runner"
	"github.com/ekalinin/anygrade/internal/scoring"
	"github.com/ekalinin/anygrade/internal/testreport"
)

// cmdCheck implements `anygrade check [TASK ...]` (SPEC §11): run checks
// locally in the current working copy, open tests only.
// Exit codes: 0 = all checks passed, 1 = a check failed or timed out,
// 2 = usage/metadata error, 3 = infrastructure error (e.g. docker down).
func cmdCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	repoFlag := fs.String("repo", "", "course repo root (default: git toplevel, else \".\")")
	dataFlag := fs.String("data-dir", "", "workspaces and logs dir (default: <repo>/.anygrade)")
	runnerFlag := fs.String("runner", "", "override the runner for all tasks: local|docker")
	keep := fs.Bool("keep", false, "keep workspaces after the run")
	timeoutFlag := fs.Duration("timeout", 0, "override the per-check timeout")
	verbose := fs.Bool("v", false, "stream check output to stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o := *runnerFlag; o != "" && o != "local" && o != "docker" {
		fmt.Fprintf(os.Stderr, "check: --runner must be local or docker, got %q\n", o)
		return 2
	}

	repo := *repoFlag
	if repo == "" {
		if top, err := gitOut(".", "rev-parse", "--show-toplevel"); err == nil {
			repo = top
		} else {
			repo = "."
		}
	}
	repo, err := filepath.Abs(repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check: %v\n", err)
		return 2
	}

	resolved, diags, err := config.LoadAll(repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check: %v\n", err)
		return 2
	}
	diags = append(diags, config.Validate(resolved)...)
	if config.HasErrors(diags) {
		for _, d := range diags {
			if d.Severity == config.SevError {
				fmt.Fprintln(os.Stderr, d)
			}
		}
		fmt.Fprintln(os.Stderr, "check: course metadata is invalid; see `anygrade validate`")
		return 2
	}

	tasks, code := selectTasks(repo, resolved.Tasks, fs.Args())
	if code != 0 {
		return code
	}
	if len(tasks) == 0 {
		fmt.Println("anygrade: no tasks changed")
		return 0
	}

	dataDir := *dataFlag
	if dataDir == "" {
		dataDir = filepath.Join(repo, ".anygrade")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	exitCode := 0
	for _, t := range tasks {
		ok, err := runTask(ctx, repo, dataDir, t, *runnerFlag, *timeoutFlag, *keep, *verbose)
		if err != nil {
			fmt.Fprintf(os.Stderr, "check: %s: %v\n", t.ID, err)
			if infra, isInfra := errors.AsType[*runner.InfraError](err); isInfra {
				if infra.Op == "image_pull" && *runnerFlag == "" {
					fmt.Fprintln(os.Stderr, "hint: docker unavailable? re-run with --runner local to run checks on this host (executes task code unsandboxed)")
				}
				return 3
			}
			return 2
		}
		if !ok {
			exitCode = 1
		}
	}
	return exitCode
}

// selectTasks resolves explicit task arguments, or detects changed tasks from
// git when no arguments are given.
func selectTasks(repo string, all []config.ResolvedTask, args []string) ([]config.ResolvedTask, int) {
	if len(args) == 0 {
		tasks, err := detectChangedTasks(repo, all)
		if err != nil {
			fmt.Fprintf(os.Stderr, "check: %v\n", err)
			return nil, 2
		}
		return tasks, 0
	}
	var out []config.ResolvedTask
	for _, arg := range args {
		idx := slices.IndexFunc(all, func(t config.ResolvedTask) bool {
			return t.ID == arg || filepath.Base(t.Dir) == arg
		})
		if idx < 0 {
			ids := make([]string, len(all))
			for i, t := range all {
				ids[i] = t.ID
			}
			fmt.Fprintf(os.Stderr, "check: unknown task %q; known tasks: %s\n", arg, strings.Join(ids, ", "))
			return nil, 2
		}
		out = append(out, all[idx])
	}
	return out, 0
}

// detectChangedTasks maps the working copy's changes (committed since the
// baseline, staged, unstaged, and untracked) to tasks (SPEC §11).
func detectChangedTasks(repo string, all []config.ResolvedTask) ([]config.ResolvedTask, error) {
	top, err := gitOut(repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, errors.New("not a git repository; name tasks explicitly: anygrade check <task-id>")
	}

	changed := map[string]bool{}
	collect := func(args ...string) {
		out, err := gitOut(top, args...)
		if err != nil || out == "" {
			return
		}
		for line := range strings.SplitSeq(out, "\n") {
			if line != "" {
				changed[filepath.Join(top, filepath.FromSlash(line))] = true
			}
		}
	}
	if baseline := detectBaseline(top); baseline != "" {
		collect("diff", "--name-only", baseline)
	}
	collect("diff", "--name-only")
	collect("diff", "--name-only", "--cached")
	collect("ls-files", "--others", "--exclude-standard")

	var tasks []config.ResolvedTask
	for _, t := range all {
		prefix := t.Dir + string(filepath.Separator)
		for p := range changed {
			if strings.HasPrefix(p, prefix) {
				tasks = append(tasks, t)
				break
			}
		}
	}
	return tasks, nil
}

// detectBaseline picks the diff baseline: the branch upstream if set, then an
// `upstream` remote, then HEAD (SPEC §11 "changed against upstream/HEAD").
func detectBaseline(top string) string {
	if _, err := gitOut(top, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil {
		return "@{upstream}"
	}
	if _, err := gitOut(top, "remote", "get-url", "upstream"); err == nil {
		if branch, err := gitOut(top, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			if _, err := gitOut(top, "rev-parse", "--verify", "--quiet", "upstream/"+branch); err == nil {
				return "upstream/" + branch
			}
		}
		if _, err := gitOut(top, "rev-parse", "--verify", "--quiet", "upstream/HEAD"); err == nil {
			return "upstream/HEAD"
		}
	}
	return "HEAD"
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// runTask assembles a workspace, runs the task's checks, and prints the
// result table. Returns ok=true iff every check passed.
func runTask(ctx context.Context, repo, dataDir string, t config.ResolvedTask, runnerOverride string, timeoutOverride time.Duration, keep, verbose bool) (bool, error) {
	relDir, err := filepath.Rel(repo, t.Dir)
	if err != nil {
		return false, err
	}
	relDir = filepath.ToSlash(relDir)

	// Hidden tests: local source only, and only if present (never fetch, §11).
	var hidden runner.Source
	if h := t.Hidden; h != nil && h.Source == "local" {
		if st, err := os.Stat(h.Path); err == nil && st.IsDir() {
			hidden = runner.WorkingCopySource{Root: h.Path}
		}
	}

	runID := time.Now().Format("20060102-150405") + "-" + t.ID
	ws, err := runner.Assemble(ctx, runner.Assembly{
		Dest:          filepath.Join(dataDir, "workspaces", runID),
		Task:          t,
		TaskRelDir:    relDir,
		Include:       t.Workspace.Include,
		Authoritative: runner.WorkingCopySource{Root: repo},
		Hidden:        hidden,
		RunAsUID:      -1,
		RunAsGID:      -1,
	})
	if err != nil {
		return false, err
	}
	if !keep {
		defer ws.Close()
	}

	spec := t.Runner
	if runnerOverride != "" {
		spec.Type = runnerOverride
	}
	if timeoutOverride > 0 {
		spec.Timeout = timeoutOverride
	}
	if spec.Type == "docker" && spec.Image == "" {
		return false, errors.New("task has no docker image configured; use --runner local")
	}

	var mirror io.Writer
	if verbose {
		mirror = os.Stderr
	}
	r, err := runner.New(spec, "", mirror)
	if err != nil {
		return false, err
	}

	outcomes, err := r.Run(ctx, runner.Job{
		WorkspaceDir: ws.Root,
		TaskRelDir:   relDir,
		Spec:         spec,
		Checks:       t.Checks,
		LogDir:       filepath.Join(dataDir, "logs", runID),
		// Usually empty here - `check` never fetches hidden tests - but a
		// course author with the local source at hand gets the same boundary
		// the server applies, which is the point of authoring against it.
		HiddenPaths: ws.HiddenPaths,
		HiddenDirs:  ws.HiddenDirs,
		// --keep is for inspecting the run afterwards, so the docker runner has
		// to copy its ephemeral /work back out; without it the kept workspace
		// would only hold the assembled inputs.
		ExportWorkspace: keep,
	})
	if err != nil {
		return false, err
	}

	printTaskResult(t, spec, outcomes)
	allPassed := !slices.ContainsFunc(outcomes, func(o runner.Outcome) bool { return !o.Passed })
	return allPassed, nil
}

func printTaskResult(t config.ResolvedTask, spec config.ResolvedRunner, outcomes []runner.Outcome) {
	loc := spec.Type
	if spec.Type == "docker" {
		loc += " " + spec.Image
	}
	fmt.Printf("%s  %s  [%s]\n", t.ID, t.Name, loc)

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 3, ' ', 0)
	fmt.Fprintln(w, "  CHECK\tRESULT\tTIME\t")
	results := make([]scoring.CheckResult, len(outcomes))
	var unparsed []string
	for i, o := range outcomes {
		res := "pass"
		note := ""
		switch {
		case o.Skipped:
			res = "skip"
		case o.TimedOut:
			res = "timeout"
			note = o.LogPath
		case !o.Passed:
			res = "fail"
			note = o.LogPath
		}
		if o.BuildFailed {
			// The run phase never happened, so LogPath is empty: point the
			// author at the phase that actually failed.
			res = "build " + res
			note = o.BuildLogPath
		}
		passedCases, scoredCases := testreport.Tally(o.Cases)
		if scoredCases > 0 {
			res = fmt.Sprintf("%s %d/%d", res, passedCases, scoredCases)
		}
		if o.ParseFailed {
			// `check` is the course-authoring tool, so a parser that reads
			// nothing is exactly what its author needs told.
			unparsed = append(unparsed, o.Name)
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", o.Name, res, o.Duration.Round(10*time.Millisecond), note)
		results[i] = scoring.CheckResult{
			Name:        o.Name,
			Required:    t.Checks[i].Required,
			Weight:      t.Checks[i].Weight,
			Passed:      o.Passed,
			PassedCases: passedCases,
			ScoredCases: scoredCases,
		}
	}
	w.Flush()
	for _, name := range unparsed {
		fmt.Printf("  %s: no test cases could be read from the report; scored by exit code\n", name)
	}
	fmt.Printf("  score: %.0f/%d\n\n", scoring.RawScore(t.Score, results), t.Score)
}
