package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/ekalinin/anygrade/internal/version"
)

func cmdVersion(args []string) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	printVersion(os.Stdout)
	return 0
}

// printVersion writes the build report: the long version (with commit and build
// date, which is what a bug report needs), the toolchain and platform, and the
// project URL - the same one the web footer links to, so a user who only has
// the binary and no server still finds the source.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "anygrade %s\n", version.String())
	fmt.Fprintf(w, "%s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintln(w, version.URL)
}
