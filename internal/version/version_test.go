package version

import (
	"strings"
	"testing"
)

// TestShortNeverEmpty guards the fallback chain: tests run without ldflags, so
// the value comes from build info (or the "dev" default) - never an empty
// string, which would render as a blank version in the CLI and the web footer.
func TestShortNeverEmpty(t *testing.T) {
	if Short() == "" {
		t.Fatal("Short() is empty")
	}
}

// TestStringContainsShort keeps the long form a superset of the short one.
func TestStringContainsShort(t *testing.T) {
	if s := String(); !strings.Contains(s, Short()) {
		t.Errorf("String() = %q, want it to contain Short() = %q", s, Short())
	}
}

func TestShortRev(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"a1b2c3d": "a1b2c3d",
		"a1b2c3d4e5f60718293a4b5c6d7e8f9012345678": "a1b2c3d",
	}
	for in, want := range cases {
		if got := shortRev(in); got != want {
			t.Errorf("shortRev(%q) = %q, want %q", in, got, want)
		}
	}
}
