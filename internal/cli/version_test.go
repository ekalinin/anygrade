package cli

import (
	"runtime"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/version"
)

// TestPrintVersion pins the three lines the command owes a user who has only
// the binary: what build this is, what it was built for, and where the source
// lives. The version line is the long form on purpose - a bug report is worth
// little without the commit it was built from.
func TestPrintVersion(t *testing.T) {
	var buf strings.Builder
	printVersion(&buf)

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("printVersion wrote %d line(s), want 3:\n%s", len(lines), buf.String())
	}
	want := []string{
		"anygrade " + version.String(),
		runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH,
		version.URL,
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q", i+1, lines[i], w)
		}
	}
}
