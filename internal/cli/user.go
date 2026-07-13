package cli

import (
	"fmt"
	"os"
)

func cmdUser(args []string) int {
	fmt.Fprintln(os.Stderr, "anygrade user: not implemented yet")
	return 1
}
