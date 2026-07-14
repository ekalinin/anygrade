package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ekalinin/anygrade/internal/app"
)

// cmdServe implements `anygrade serve` (SPEC §11): git server + submission
// queue (+ web UI in M5) over one course repo.
func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	repoFlag := fs.String("repo", "", "course repo root (default: git toplevel, else \".\")")
	dataFlag := fs.String("data-dir", "", "data directory (default: <repo>/.anygrade)")
	httpAddr := fs.String("http-addr", ":8080", "git smart HTTP + web UI listen address")
	sshAddr := fs.String("ssh-addr", ":2222", "git SSH listen address")
	workers := fs.Int("workers", 4, "check worker pool size")
	baseURL := fs.String("base-url", "", "public base URL for links in push output")
	local := fs.Bool("local", false, "local mode: no auth, single implicit user, loopback only")
	allowLocalRunner := fs.Bool("allow-local-runner", false,
		"allow local-runner tasks on a non-loopback bind (executes untrusted code on the host)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	repo := *repoFlag
	if repo == "" {
		if top, err := gitOut(".", "rev-parse", "--show-toplevel"); err == nil {
			repo = top
		} else {
			repo = "."
		}
	}
	repo, err := filepath.Abs(repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 2
	}
	dataDir := *dataFlag
	if dataDir == "" {
		dataDir = filepath.Join(repo, ".anygrade")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = app.Run(ctx, app.Options{
		RepoDir:          repo,
		DataDir:          dataDir,
		HTTPAddr:         *httpAddr,
		SSHAddr:          *sshAddr,
		BaseURL:          *baseURL,
		Workers:          *workers,
		Local:            *local,
		AllowLocalRunner: *allowLocalRunner,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 1
	}
	return 0
}
