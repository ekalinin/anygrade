package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ekalinin/anygrade/internal/config"
)

func cmdValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	repo := fs.String("repo", ".", "path to the course repo")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Absolute, like serve/check/export resolve their own --repo: the paths on
	// the loaded course then stay valid regardless of the working directory.
	repoDir, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate: %v\n", err)
		return 1
	}

	resolved, diags, err := config.LoadAll(repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate: %v\n", err)
		return 1
	}

	diags = append(diags, config.Validate(resolved)...)

	var errCount, warnCount int
	for _, d := range diags {
		switch d.Severity {
		case config.SevError:
			errCount++
			fmt.Fprintln(os.Stderr, d.String())
		case config.SevWarning:
			warnCount++
			fmt.Println(d.String())
		}
	}

	fmt.Printf("%d error(s), %d warning(s)\n", errCount, warnCount)

	if errCount > 0 {
		return 1
	}

	fmt.Println("OK: course is valid")
	return 0
}
