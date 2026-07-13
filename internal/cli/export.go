package cli

import (
	"fmt"
	"os"
)

func cmdExport(args []string) int {
	fmt.Fprintln(os.Stderr, "anygrade export: not implemented yet")
	return 1
}
