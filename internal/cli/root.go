// Package cli implements the anygrade command-line interface.
package cli

import (
	"fmt"
	"os"
)

// Run dispatches args[0] to the matching subcommand and returns its exit code.
func Run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 2
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return cmdServe(rest)
	case "check":
		return cmdCheck(rest)
	case "validate":
		return cmdValidate(rest)
	case "user":
		return cmdUser(rest)
	case "export":
		return cmdExport(rest)
	default:
		printUsage()
		return 2
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: anygrade <command> [flags]")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  serve      run the anygrade server")
	fmt.Fprintln(os.Stderr, "  check      run checks locally in the current working copy")
	fmt.Fprintln(os.Stderr, "  validate   validate course.yaml and all task.yaml files")
	fmt.Fprintln(os.Stderr, "  user       manage users")
	fmt.Fprintln(os.Stderr, "  export     export course data")
}
