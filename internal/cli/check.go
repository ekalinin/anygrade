package cli

import (
	"fmt"
	"os"
)

func cmdCheck(args []string) int {
	fmt.Fprintln(os.Stderr, "anygrade check: not implemented yet")
	return 1
}
