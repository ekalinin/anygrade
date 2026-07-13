// Command anygrade is the single-binary entrypoint for the anygrade grading system.
package main

import (
	"os"

	"github.com/ekalinin/anygrade/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
