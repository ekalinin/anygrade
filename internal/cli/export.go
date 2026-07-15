package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/gitserver"
	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/store"
)

// cmdExport implements `anygrade export <subcommand>`.
func cmdExport(args []string) int {
	if len(args) == 0 {
		printExportUsage()
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "scores":
		return exportScores(rest)
	default:
		printExportUsage()
		return 2
	}
}

func printExportUsage() {
	fmt.Fprintln(os.Stderr, "usage: anygrade export scores [--format csv] [--out -] [--repo DIR] [--data-dir DIR]")
}

func exportScores(args []string) int {
	fs := flag.NewFlagSet("export scores", flag.ContinueOnError)
	format := fs.String("format", "csv", "output format: csv")
	out := fs.String("out", "-", "output file, \"-\" for stdout")
	repoFlag := fs.String("repo", "", "course repo root (default: git toplevel, else \".\")")
	dataFlag := fs.String("data-dir", "", "anygrade data directory (default: <repo>/.anygrade)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *format != "csv" {
		fmt.Fprintf(os.Stderr, "export: --format must be csv, got %q\n", *format)
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
		fmt.Fprintf(os.Stderr, "export: %v\n", err)
		return 2
	}

	dataDir := *dataFlag
	if dataDir == "" {
		dataDir = filepath.Join(repo, ".anygrade")
	}

	ctx := context.Background()
	cols, policy, code := loadExportCourse(ctx, repo, dataDir)
	if code != 0 {
		return code
	}

	db, err := store.Open(ctx, dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anygrade export: %v\n", err)
		return 1
	}
	defer db.Close()

	users, err := db.ListUsers(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anygrade export: %v\n", err)
		return 1
	}
	subs, err := db.ListAllSubmissions(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anygrade export: %v\n", err)
		return 1
	}
	overrides, err := db.ListScoreOverrides(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anygrade export: %v\n", err)
		return 1
	}

	m := gradebook.Build(users, cols, subs, overrides, policy)

	w := os.Stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "anygrade export: %v\n", err)
			return 1
		}
		defer f.Close()
		w = f
	}
	if err := gradebook.WriteCSV(w, m); err != nil {
		fmt.Fprintf(os.Stderr, "anygrade export: %v\n", err)
		return 1
	}
	return 0
}

// loadExportCourse loads task columns and the scoring policy: from the
// course mirror when provisioned (read-only, no refresh), else from the
// working copy's config (SPEC §12).
func loadExportCourse(ctx context.Context, repo, dataDir string) ([]gradebook.TaskCol, string, int) {
	rm := gitserver.RepoManager{DataDir: dataDir}
	courseDir := rm.CourseDir()
	if _, err := os.Stat(courseDir); err == nil {
		course, diags, err := intake.LoadCourse(ctx, courseDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "anygrade export: %v\n", err)
			return nil, "", 1
		}
		if course == nil {
			for _, d := range diags {
				if d.Severity == config.SevError {
					fmt.Fprintln(os.Stderr, d)
				}
			}
			fmt.Fprintln(os.Stderr, "anygrade export: course metadata is invalid; see `anygrade validate`")
			return nil, "", 2
		}
		return taskCols(course.Resolved.Tasks), course.Resolved.Course.ScoringPolicy, 0
	}

	resolved, diags, err := config.LoadAll(repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anygrade export: %v\n", err)
		return nil, "", 2
	}
	diags = append(diags, config.Validate(resolved)...)
	if config.HasErrors(diags) {
		for _, d := range diags {
			if d.Severity == config.SevError {
				fmt.Fprintln(os.Stderr, d)
			}
		}
		fmt.Fprintln(os.Stderr, "anygrade export: course metadata is invalid; see `anygrade validate`")
		return nil, "", 2
	}
	return taskCols(resolved.Tasks), resolved.Course.ScoringPolicy, 0
}

func taskCols(tasks []config.ResolvedTask) []gradebook.TaskCol {
	cols := make([]gradebook.TaskCol, len(tasks))
	for i, t := range tasks {
		cols[i] = gradebook.TaskCol{ID: t.ID, Name: t.Name, MaxScore: t.Score}
	}
	return cols
}
