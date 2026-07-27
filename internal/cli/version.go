package cli

import (
	"flag"
	"fmt"
	"runtime"

	"github.com/ekalinin/anygrade/internal/version"
)

func cmdVersion(args []string) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fmt.Printf("anygrade %s\n", version.String())
	fmt.Printf("%s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return 0
}
