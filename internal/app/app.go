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
	Log              io.Writer // startup/diagnostic lines; nil = os.Stderr
}

// Run starts the whole server (git transports, intake socket, worker pool)
// and blocks until ctx is canceled or a component fails.
func Run(ctx context.Context, opts Options) error {
	logw := opts.Log
	if logw == nil {
		logw = os.Stderr
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

	holder := &intake.Holder{}
	holder.Set(course)

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
		Prep:    &intake.Prep{Repos: repos, Users: db, Course: holder, DataDir: opts.DataDir, Hidden: hcache},
		Workers: opts.Workers,
		Events:  hub,
	}
	ic := &intake.Server{
		DB: db, Queue: q, Repos: repos, Course: holder,
		BaseURL: baseURL(opts),
		Log:     log,
	}
	auth := storeAuth{db}
	// One failure budget per (client IP, login) pair, shared between git
	// basic auth and the web login form.
	limit := ratelimit.New(10, 10*time.Minute)
	site := web.New(&web.Handler{
		DB:     db,
		Course: holder,
		Hub:    hub,
		// intake.Server implements Recheck; web stays git-free.
		Recheck: ic,
		Cancel:  q,
		ReadCourseFile: func(ctx context.Context, commit, relPath string) ([]byte, bool, error) {
			return gitserver.GitSource{Dir: repos.CourseDir(), Commit: commit}.File(ctx, relPath)
		},
		ListStudentFiles: func(ctx context.Context, login, commit, relDir string) ([]string, error) {
			return gitserver.GitSource{Dir: repos.StudentDir(login), Commit: commit}.List(ctx, relDir)
		},
		ReadStudentFile: func(ctx context.Context, login, commit, relPath string) ([]byte, bool, error) {
			return gitserver.GitSource{Dir: repos.StudentDir(login), Commit: commit}.File(ctx, relPath)
		},
		DataDir: opts.DataDir,
		BaseURL: baseURL(opts),
		SSHAddr: opts.SSHAddr,
		Limit:   limit,
		Local:   localUser,
	})
	mux := http.NewServeMux()
	mux.Handle("/git/", &gitserver.HTTPHandler{Repos: repos, Auth: auth, Socket: socket, Local: localID, Limit: limit})
	mux.Handle("/", site)
	httpSrv := &http.Server{Addr: opts.HTTPAddr, Handler: mux}
	sshSrv := &gitserver.SSHServer{
		Repos: repos, Auth: auth, Socket: socket,
		HostKey: filepath.Join(opts.DataDir, "ssh_host_ed25519_key"),
		Local:   localID,
	}

	fmt.Fprintf(logw, "anygrade: course %q, %d task(s), head %.12s\n",
		course.Resolved.Course.Name, len(course.Resolved.Tasks), course.Head)
	fmt.Fprintf(logw, "anygrade: http %s, ssh %s, data dir %s\n",
		opts.HTTPAddr, opts.SSHAddr, opts.DataDir)

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
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
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

// baseURL derives the submission-link prefix when --base-url is not given.
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
	return "http://" + net.JoinHostPort(host, port)
}
