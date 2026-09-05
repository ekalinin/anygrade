package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ekalinin/anygrade/internal/app"
)

// loopbackHost is prefixed onto the address defaults under --local. The
// shipped defaults have an empty host, which binds every interface, and that
// mode refuses to serve on one.
const loopbackHost = "127.0.0.1"

// serveFlags is the `serve` flag set, kept in one place so the address
// defaulting below can be exercised without starting a server.
type serveFlags struct {
	fs               *flag.FlagSet
	repo             *string
	data             *string
	httpAddr         *string
	sshAddr          *string
	baseURL          *string
	workers          *int
	local            *bool
	allowLocalRunner *bool
	tlsCert          *string
	tlsKey           *string
	behindProxy      *bool
	retryBackoff     *time.Duration
	retryBackoffCap  *time.Duration
	maxRetries       *int
}

func newServeFlags() *serveFlags {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	return &serveFlags{
		fs:       fs,
		repo:     fs.String("repo", "", "course repo root (default: git toplevel, else \".\")"),
		data:     fs.String("data-dir", "", "data directory (default: <repo>/.anygrade)"),
		httpAddr: fs.String("http-addr", ":8080", "git smart HTTP + web UI listen address"),
		sshAddr:  fs.String("ssh-addr", ":2222", "git SSH listen address"),
		workers:  fs.Int("workers", 4, "check worker pool size"),
		baseURL:  fs.String("base-url", "", "public base URL for links in push output"),
		local: fs.Bool("local", false,
			"local mode: no auth, single implicit user, loopback only (addresses default to "+loopbackHost+")"),
		allowLocalRunner: fs.Bool("allow-local-runner", false,
			"allow local-runner tasks on a non-loopback bind (executes untrusted code on the host)"),
		tlsCert: fs.String("tls-cert", "", "PEM certificate chain; serves HTTPS (requires --tls-key)"),
		tlsKey:  fs.String("tls-key", "", "PEM private key for --tls-cert"),
		behindProxy: fs.Bool("behind-proxy", false,
			"trust X-Forwarded-Proto from a TLS-terminating reverse proxy"),
		// The infra-error retry schedule (SPEC §13). The defaults are the
		// queue's own, so an invocation that names none of the three keeps
		// behaving exactly as it did before the flags existed.
		retryBackoff: fs.Duration("retry-backoff", app.DefaultRetryBackoff,
			"delay before the first retry of an infra_error submission; doubles per retry"),
		retryBackoffCap: fs.Duration("retry-backoff-cap", app.DefaultRetryBackoffCap,
			"upper bound on that delay"),
		maxRetries: fs.Int("max-retries", app.DefaultMaxRetries,
			"infra_error retries before a submission becomes terminal"),
	}
}

// parse reads args and, under --local, moves the addresses the user did not
// name onto the loopback interface. Without this `anygrade serve --local`
// cannot start at all: the shipped defaults bind every interface, which the
// mode refuses, so the documented invocation needed two more flags to work.
//
// Only an unset flag is touched, and the test is flag.Visit rather than a
// comparison against the default string: `--http-addr :8080` is the user
// asking for every interface, and it must keep being refused.
func (f *serveFlags) parse(args []string) error {
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	if !*f.local {
		return nil
	}
	set := make(map[string]bool)
	f.fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if !set["http-addr"] {
		*f.httpAddr = loopbackHost + *f.httpAddr
	}
	if !set["ssh-addr"] {
		*f.sshAddr = loopbackHost + *f.sshAddr
	}
	return nil
}

// cmdServe implements `anygrade serve` (SPEC §11): git server + submission
// queue (+ web UI in M5) over one course repo.
func cmdServe(args []string) int {
	f := newServeFlags()
	if err := f.parse(args); err != nil {
		return 2
	}
	repo := *f.repo
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
	dataDir := *f.data
	if dataDir == "" {
		dataDir = filepath.Join(repo, ".anygrade")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = app.Run(ctx, app.Options{
		RepoDir:          repo,
		DataDir:          dataDir,
		HTTPAddr:         *f.httpAddr,
		SSHAddr:          *f.sshAddr,
		BaseURL:          *f.baseURL,
		Workers:          *f.workers,
		Local:            *f.local,
		AllowLocalRunner: *f.allowLocalRunner,
		TLSCert:          *f.tlsCert,
		TLSKey:           *f.tlsKey,
		BehindProxy:      *f.behindProxy,
		RetryBackoff:     *f.retryBackoff,
		RetryBackoffCap:  *f.retryBackoffCap,
		MaxRetries:       *f.maxRetries,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 1
	}
	return 0
}
