// Package version reports the build metadata of the running binary. The values
// are injected with -ldflags at build time (see .goreleaser.yaml and the
// Makefile); a plain `go build` leaves them empty, and the fallback reads
// runtime/debug build info instead - so `go install ...@v0.1.0` still reports a
// real version. It is a leaf package (stdlib only), imported by cli and web.
package version

import (
	"runtime/debug"
	"strings"
	"sync"
)

// Injected with -ldflags "-X github.com/ekalinin/anygrade/internal/version.Version=v0.1.0".
var (
	Version string
	Commit  string
	Date    string
)

// build is the resolved metadata: the ldflags values where present, build info
// elsewhere.
type build struct {
	version string
	commit  string
	date    string
}

// resolved caches the lookup - debug.ReadBuildInfo walks the whole build info
// table, and every rendered page asks for the version.
var resolved = sync.OnceValue(func() build {
	b := build{version: Version, commit: Commit, date: Date}

	var rev, vcsTime string
	var dirty bool
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				vcsTime = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		// The module version is set for `go install <pkg>@<version>`; a build
		// from a working copy reports "(devel)".
		if b.version == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			b.version = info.Main.Version
		}
	}

	if b.commit == "" {
		b.commit = shortRev(rev)
	}
	if b.date == "" {
		b.date = vcsTime
	}
	if b.version == "" {
		if rev != "" {
			b.version = "dev+" + shortRev(rev)
		} else {
			b.version = "dev"
		}
		if dirty {
			b.version += "-dirty"
		}
	}
	return b
})

// shortRev abbreviates a full commit sha the way git does.
func shortRev(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

// Short returns just the version, e.g. "v0.1.0" or "dev+a1b2c3d". It is never
// empty.
func Short() string { return resolved().version }

// String returns the version with the commit and build date when known, e.g.
// "v0.1.0 (a1b2c3d, 2026-07-27T10:00:00Z)".
func String() string {
	b := resolved()
	var extra []string
	if b.commit != "" {
		extra = append(extra, b.commit)
	}
	if b.date != "" {
		extra = append(extra, b.date)
	}
	if len(extra) == 0 {
		return b.version
	}
	return b.version + " (" + strings.Join(extra, ", ") + ")"
}
