// Package app is the composition root (SPEC §5): the only place where store,
// queue, gitserver, and intake concrete types are wired together.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/gitserver"
	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/hidden"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/queue"
	"github.com/ekalinin/anygrade/internal/ratelimit"
	"github.com/ekalinin/anygrade/internal/store"
	"github.com/ekalinin/anygrade/internal/web"
)

// Options are the `anygrade serve` settings (SPEC §11).
type Options struct {
	RepoDir          string
	DataDir          string
	HTTPAddr         string
	SSHAddr          string
	BaseURL          string
	Workers          int
	Local            bool
	AllowLocalRunner bool
	// TLSCert/TLSKey serve the HTTP listener over TLS; both or neither.
	TLSCert string
	TLSKey  string
	// BehindProxy trusts X-Forwarded-Proto from a TLS-terminating reverse
	// proxy (session cookie Secure flag).
	BehindProxy bool
	// RetryBackoff/RetryBackoffCap/MaxRetries are the infra-error retry
	// schedule the worker pool runs on (SPEC §13). They belong to the
	// deployment, not to the course: what makes 10s/5m/8 wrong is a slow
	// registry or a hidden-tests remote behind a flaky link, and the operator
	// is the one who knows that - a teacher pushing course.yaml is not.
	RetryBackoff    time.Duration
	RetryBackoffCap time.Duration
	MaxRetries      int
	Log             io.Writer // startup/diagnostic lines; nil = os.Stderr
}

// The shipped retry schedule (SPEC §13), re-exported so `serve` can advertise
// in its own help exactly what it passes on, without the CLI reaching past the
// composition root into the queue.
const (
	DefaultRetryBackoff    = queue.DefaultBackoffBase
	DefaultRetryBackoffCap = queue.DefaultBackoffCap
	DefaultMaxRetries      = queue.DefaultMaxRetries
)

// Run starts the whole server (git transports, intake socket, worker pool)
// and blocks until ctx is canceled or a component fails.
func Run(ctx context.Context, opts Options) error {
	logw := opts.Log
	if logw == nil {
		logw = os.Stderr
	}
	if err := checkTLSOptions(opts.TLSCert, opts.TLSKey); err != nil {
		return err
	}
	if err := checkRetryOptions(opts.RetryBackoff, opts.RetryBackoffCap, opts.MaxRetries); err != nil {
		return err
	}
	hookBin, err := os.Executable()
	if err != nil {
		return err
	}
	// Hooks run with cwd = the bare repo, so every path handed to them
	// (socket, data dir) must be absolute.
	if opts.DataDir, err = filepath.Abs(opts.DataDir); err != nil {
		return err
	}
	if opts.RepoDir, err = filepath.Abs(opts.RepoDir); err != nil {
		return err
	}

	db, err := store.Open(ctx, opts.DataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	repos := &gitserver.RepoManager{DataDir: opts.DataDir, HookBin: hookBin}
	if err := repos.EnsureCourse(ctx, opts.RepoDir); err != nil {
		if !errors.Is(err, gitserver.ErrMirrorRefresh) {
			return err
		}
		fmt.Fprintf(logw, "anygrade: warning: %v; serving the mirror as is\n", err)
	}
	course, diags, err := intake.LoadCourse(ctx, repos.CourseDir())
	if err != nil {
		return err
	}
	if course == nil {
		for _, d := range diags {
			if d.Severity == config.SevError {
				fmt.Fprintln(logw, d)
			}
		}
		return errors.New("course metadata is invalid; see `anygrade validate`")
	}
	if err := checkServeSafety(course.Resolved, opts.HTTPAddr, opts.SSHAddr, opts.Local, opts.AllowLocalRunner); err != nil {
		return err
	}
	// EnsureCourse ran before the metadata was readable, so the mirror still
	// carries the built-in push cap; adopt the course's own (SPEC §13).
	if err := repos.SetMaxInputSize(ctx, course.Resolved.Course.MaxPushSize); err != nil {
		return err
	}

	holder := &intake.Holder{}
	holder.Set(course)
	// Read through the holder, not off `course`: a teacher metadata push swaps
	// the snapshot, and the UI must follow the new `timezone:` (SPEC §13).
	web.SetTimezoneSource(func() *time.Location { return holder.Get().Resolved.Course.Timezone })

	// Both are nil unless --local: the zero value keeps git auth and web
	// sessions on. checkServeSafety above has already refused a non-loopback
	// bind in that mode.
	var localID *gitserver.Identity
	var localUser *store.User
	if opts.Local {
		u, err := ensureLocalUser(ctx, db)
		if err != nil {
			return err
		}
		localID = &gitserver.Identity{UserID: u.ID, Login: u.Login, Role: u.Role}
		localUser = &u
	}

	socket := filepath.Join(opts.DataDir, "anygrade.sock")
	hub := web.NewHub()
	log := slog.New(slog.NewTextHandler(logw, nil))
	// Hidden-tests credentials come from the environment only (SPEC §11),
	// never from the course repo.
	hcache := &hidden.Cache{
		Dir:   filepath.Join(opts.DataDir, "hidden"),
		Token: os.Getenv("ANYGRADE_HIDDEN_GIT_TOKEN"),
		Log:   log,
	}
	q := &queue.Queue{
		Store:   db,
		Prep:    &intake.Prep{Repos: repos, Users: db, Course: holder, DataDir: opts.DataDir, Hidden: hcache, Log: log},
		Workers: opts.Workers,
		Events:  hub,
		// The retry schedule is fixed for the life of the process, like the
		// worker count next to it. A teacher push swaps the course snapshot,
		// never this: a submission already waiting on a backoff was scheduled
		// against the schedule in force when it failed, and re-deriving its
		// deadline from a newer one would either strand it or stampede the
		// whole backlog at once.
		BackoffBase: opts.RetryBackoff,
		BackoffCap:  opts.RetryBackoffCap,
		MaxRetries:  opts.MaxRetries,
	}
	ic := &intake.Server{
		DB: db, Queue: q, Repos: repos, Course: holder,
		BaseURL: baseURL(opts),
		Log:     log,
		// The §14 gate is a property of how this process was started, so intake
		// gets it as a closure instead of reaching up for the serve options: a
		// teacher push that switches a task to the local runner is rejected on
		// a public bind, exactly as it would be at startup.
		Safety: func(res *config.Resolved) error {
			return checkServeSafety(res, opts.HTTPAddr, opts.SSHAddr, opts.Local, opts.AllowLocalRunner)
		},
	}
	auth := storeAuth{db}
	// One failure budget per (client IP, login) pair, shared between git
	// basic auth and the web login form.
	limit := ratelimit.New(10, 10*time.Minute)
	aliasSecret, err := loadLeaderboardSecret(opts.DataDir)
	if err != nil {
		return err
	}
	site := web.New(&web.Handler{
		DB:     db,
		Course: holder,
		Hub:    hub,
		// intake.Server implements Recheck; web stays git-free.
		Recheck: ic,
		Cancel:  q,
		ReadCourseFile: func(ctx context.Context, commit, relPath string) ([]byte, bool, error) {
			return webFile(gitserver.GitSource{Dir: repos.CourseDir(), Commit: commit}.File(ctx, relPath))
		},
		ListStudentFiles: func(ctx context.Context, login, commit, relDir string) ([]string, error) {
			return gitserver.GitSource{Dir: repos.StudentDir(login), Commit: commit}.List(ctx, relDir)
		},
		ReadStudentFile: func(ctx context.Context, login, commit, relPath string) ([]byte, bool, error) {
			return webFile(gitserver.GitSource{Dir: repos.StudentDir(login), Commit: commit}.File(ctx, relPath))
		},
		// SPEC §7: the personal repo is created at activation. web only asks;
		// reporting a failure is this side's job, because the caller must not
		// fail an activation over it (the transports still provision lazily).
		EnsureRepo: func(ctx context.Context, login string) error {
			_, err := repos.EnsureStudent(ctx, login)
			if err != nil {
				log.Warn("provisioning the student repo at activation failed; it will be created on first git access",
					"login", login, "err", err)
			}
			return err
		},
		DataDir:     opts.DataDir,
		BaseURL:     baseURL(opts),
		SSHAddr:     opts.SSHAddr,
		Limit:       limit,
		Local:       localUser,
		BehindProxy: opts.BehindProxy,
		Alias:       gradebook.NewAliaser(aliasSecret),
	})
	mux := http.NewServeMux()
	mux.Handle("/git/", &gitserver.HTTPHandler{
		Repos: repos, Auth: auth, Socket: socket, Local: localID,
		Limit: limit, BehindProxy: opts.BehindProxy,
	})
	mux.Handle("/", site)
	httpSrv := newHTTPServer(opts.HTTPAddr, mux)
	// No Limit here on purpose (SPEC §14): the failure limiter budgets guessable
	// credentials, and SSH has none - it authenticates a public-key fingerprint,
	// and an honest client offers every key in its agent until one matches, so
	// charging the misses would throttle students and not attackers. The SSH
	// transport carries its own budgets instead, on handshake concurrency and
	// handshake/idle time.
	sshSrv := &gitserver.SSHServer{
		Repos: repos, Auth: auth, Socket: socket,
		HostKey: filepath.Join(opts.DataDir, "ssh_host_ed25519_key"),
		Local:   localID,
	}

	fmt.Fprintf(logw, "anygrade: course %q, %d task(s), head %.12s\n",
		course.Resolved.Course.Name, len(course.Resolved.Tasks), course.Head)
	fmt.Fprintf(logw, "anygrade: %s %s, ssh %s, data dir %s\n",
		scheme(opts), opts.HTTPAddr, opts.SSHAddr, opts.DataDir)
	if w := plaintextWarning(opts); w != "" {
		fmt.Fprint(logw, w)
	}

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 4)
	var wg sync.WaitGroup
	start := func(name string, fn func() error) {
		wg.Go(func() {
			if err := fn(); err != nil {
				errCh <- fmt.Errorf("%s: %w", name, err)
				cancel()
			}
		})
	}
	start("intake", func() error { return ic.ListenAndServe(rctx, socket) })
	start("queue", func() error { return q.Start(rctx) })
	start("ssh", func() error { return sshSrv.ListenAndServe(rctx, opts.SSHAddr) })
	start("http", func() error {
		go func() {
			<-rctx.Done()
			sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer scancel()
			_ = httpSrv.Shutdown(sctx)
		}()
		serve := httpSrv.ListenAndServe
		if opts.TLSCert != "" {
			serve = func() error { return httpSrv.ListenAndServeTLS(opts.TLSCert, opts.TLSKey) }
		}
		if err := serve(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

// storeAuth adapts the store to gitserver.Authenticator, keeping gitserver
// store-free.
type storeAuth struct {
	db store.Store
}

func (a storeAuth) ByToken(ctx context.Context, login, token string) (gitserver.Identity, bool, error) {
	u, ok, err := a.db.VerifyToken(ctx, token)
	if err != nil || !ok || u.Login != login {
		return gitserver.Identity{}, false, err
	}
	return gitserver.Identity{UserID: u.ID, Login: u.Login, Role: u.Role}, true, nil
}

func (a storeAuth) ByFingerprint(ctx context.Context, fingerprint string) (gitserver.Identity, bool, error) {
	u, ok, err := a.db.UserByFingerprint(ctx, fingerprint)
	if err != nil || !ok {
		return gitserver.Identity{}, false, err
	}
	return gitserver.Identity{UserID: u.ID, Login: u.Login, Role: u.Role}, true, nil
}

// ensureLocalUser backs `serve --local`: one implicit teacher-role account
// (SPEC §8), created on first use.
func ensureLocalUser(ctx context.Context, db store.Store) (store.User, error) {
	if u, err := db.GetUserByLogin(ctx, "local"); err == nil {
		return u, nil
	}
	return db.CreateUser(ctx, "local", "Local User", "teacher")
}

// scheme is what this process's own HTTP listener speaks (SPEC §11): --tls-cert
// and --tls-key together turn it into HTTPS. Deliberately not --behind-proxy:
// that flag says a proxy terminates TLS in front of us, so the listener here is
// still plaintext, and the public origin is the proxy's - usually a different
// host and port than --http-addr, which is exactly what --base-url is for.
func scheme(opts Options) string {
	if opts.TLSCert != "" {
		return "https"
	}
	return "http"
}

// baseURL derives the submission-link prefix when --base-url is not given. The
// links go into push output (SPEC §11) and onto the activation pages, so the
// scheme has to be the one the listener answers, not a fixed "http".
func baseURL(opts Options) string {
	if opts.BaseURL != "" {
		return opts.BaseURL
	}
	host, port, err := net.SplitHostPort(opts.HTTPAddr)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return scheme(opts) + "://" + net.JoinHostPort(host, port)
}
