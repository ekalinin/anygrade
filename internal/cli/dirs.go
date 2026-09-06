package cli

import (
	"fmt"
	"path/filepath"
)

// courseDirs resolves the course repo root and the data directory shared by
// every subcommand that operates on a provisioned course: an explicit --repo
// wins, otherwise the git toplevel of the cwd; an explicit --data-dir wins,
// otherwise <repo>/.anygrade.
//
// requireRepo controls what happens when neither --repo nor a git toplevel
// resolves the repo root. export, serve and check have always fallen back to
// the cwd itself in that case, and keep doing so (requireRepo false). The
// user subcommands pass true: falling back silently there means store.Open
// materialises a fresh, empty database next to wherever the command happened
// to run, indistinguishable from a real course's - but an explicit
// --data-dir still needs no repo at all, so only the case where neither is
// given fails.
func courseDirs(repoFlag, dataFlag string, requireRepo bool) (repo, dataDir string, err error) {
	repo = repoFlag
	if repo == "" {
		if top, gitErr := gitOut(".", "rev-parse", "--show-toplevel"); gitErr == nil {
			repo = top
		} else if requireRepo && dataFlag == "" {
			return "", "", fmt.Errorf("not inside a git repository; pass --repo or --data-dir")
		} else {
			repo = "."
		}
	}
	if repo, err = filepath.Abs(repo); err != nil {
		return "", "", err
	}
	dataDir = dataFlag
	if dataDir == "" {
		dataDir = filepath.Join(repo, ".anygrade")
	}
	return repo, dataDir, nil
}
