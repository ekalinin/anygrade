// Package gitserver serves git over smart HTTP and SSH on top of the system
// git binary. It is transport + provisioning only: no store/queue imports.
package gitserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ekalinin/anygrade/internal/ident"
)

// defaultMaxInputSize is the push cap applied until the composition root has
// resolved course.yaml's `limits.max_push_size` (SPEC §13).
const defaultMaxInputSize = 50 << 20

// ownerOnly is the mode of every directory anygrade creates under the data
// dir. The bare repos hold student code and the course's hidden-test wiring,
// and the data dir itself is owner-only, so nothing below it should be wider.
// Whatever git creates *inside* a repo keeps git's own bookkeeping modes; the
// repo root being 0700 is what keeps other accounts out of all of it.
const ownerOnly = 0o700

// ensureDir creates path when missing and narrows it to ownerOnly, so a data
// dir carried over from an install that made these 0755 is tightened in place
// rather than left as it was.
func ensureDir(path string) error {
	if err := os.MkdirAll(path, ownerOnly); err != nil {
		return err
	}
	return os.Chmod(path, ownerOnly)
}

// ErrMirrorRefresh marks a failed mirror refresh from the --repo working
// copy. The mirror is the source of truth (teachers push to it directly),
// so a stale working copy is expected and non-fatal.
var ErrMirrorRefresh = errors.New("course mirror refresh skipped")

// RepoManager provisions and locates the bare repos in the data dir.
type RepoManager struct {
	DataDir string // anygrade data dir; repos live under DataDir/repos
	HookBin string // absolute path of the anygrade binary, exec'd by hook shims

	maxInput atomic.Int64 // max_push_size in bytes; 0 = defaultMaxInputSize

	mu    sync.Mutex             // guards locks
	locks map[string]*sync.Mutex // per-login provisioning locks
}

// CourseDir is the bare mirror of the course repo.
func (m *RepoManager) CourseDir() string {
	return filepath.Join(m.DataDir, "repos", "course.git")
}

// StudentDir is the bare repo for a student's login.
func (m *RepoManager) StudentDir(login string) string {
	return filepath.Join(m.DataDir, "repos", "students", login+".git")
}

// MaxInputSize is the push cap currently in force, in bytes. The transports
// enforce it themselves so the student gets our explanation rather than git's
// "pack exceeds maximum allowed size"; receive.maxInputSize below is the same
// number left in the repos as a backstop.
func (m *RepoManager) MaxInputSize() int64 {
	if n := m.maxInput.Load(); n > 0 {
		return n
	}
	return defaultMaxInputSize
}

// SetMaxInputSize adopts course.yaml's `limits.max_push_size` (0 restores the
// default) and re-applies it to the course mirror. Student repos pick the new
// value up at their next EnsureStudent, which every git access performs before
// receive-pack starts.
func (m *RepoManager) SetMaxInputSize(ctx context.Context, n int64) error {
	m.maxInput.Store(n)
	if _, err := os.Stat(m.CourseDir()); err != nil {
		return nil // not provisioned yet; EnsureCourse writes it
	}
	_, err := runGit(ctx, m.CourseDir(), "config", "receive.maxInputSize",
		strconv.FormatInt(m.MaxInputSize(), 10))
	return err
}

// EnsureCourse creates the course mirror on first use, or refreshes it with
// a non-forced fetch on subsequent calls; hooks and config are (re)installed
// unconditionally so a HookBin move is picked up.
func (m *RepoManager) EnsureCourse(ctx context.Context, srcRepo string) error {
	dir := m.CourseDir()
	if err := ensureDir(filepath.Dir(dir)); err != nil {
		return err
	}
	var refreshErr error
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if _, err := runGitAt(ctx, "clone", "--mirror", srcRepo, dir); err != nil {
			return err
		}
	} else {
		// Non-forced fetch: teacher pushes to the mirror must never be
		// rolled back by a stale working copy. The mirror being AHEAD of
		// --repo is normal after teacher pushes, so this is reported via
		// the sentinel (after hooks/config are still refreshed below).
		if _, err := runGit(ctx, dir, "fetch", srcRepo, "refs/heads/*:refs/heads/*"); err != nil {
			refreshErr = fmt.Errorf("%w (mirror ahead of %s?): %v", ErrMirrorRefresh, srcRepo, err)
		}
	}

	// git clone leaves the repo root at the umask default; narrow it here so
	// the run that created it and the upgrade that inherited it agree.
	if err := ensureDir(dir); err != nil {
		return err
	}
	if _, err := runGit(ctx, dir, "config", "receive.maxInputSize", strconv.FormatInt(m.MaxInputSize(), 10)); err != nil {
		return err
	}
	if err := installHook(dir, "pre-receive", "validate-course", m.HookBin); err != nil {
		return err
	}
	if err := installHook(dir, "post-receive", "post-receive", m.HookBin); err != nil {
		return err
	}
	return refreshErr
}

// EnsureStudent creates (or reuses) the bare repo for login, cloned from the
// course mirror on first use. Provisioning for a given login is serialized.
func (m *RepoManager) EnsureStudent(ctx context.Context, login string) (string, error) {
	if !ident.ValidLogin(login) {
		return "", fmt.Errorf("invalid login %q", login)
	}

	lock := m.loginLock(login)
	lock.Lock()
	defer lock.Unlock()

	dir := m.StudentDir(login)
	if err := ensureDir(filepath.Dir(dir)); err != nil {
		return "", err
	}
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		if _, err := runGitAt(ctx, "clone", "--bare", m.CourseDir(), dir); err != nil {
			return "", err
		}
		// Seed the intake baseline to the cloned head so the student's first
		// push diffs against the course template, not the empty tree: only the
		// tasks they actually change are detected, not every task at once.
		if _, err := runGit(ctx, dir, "update-ref", "refs/anygrade/baseline", "HEAD"); err != nil {
			return "", err
		}
	}

	if err := ensureDir(dir); err != nil {
		return "", err
	}
	if _, err := runGit(ctx, dir, "config", "receive.maxInputSize", strconv.FormatInt(m.MaxInputSize(), 10)); err != nil {
		return "", err
	}
	if _, err := runGit(ctx, dir, "config", "transfer.hideRefs", "refs/anygrade"); err != nil {
		return "", err
	}

	branch, err := m.DefaultBranch(ctx, m.CourseDir())
	if err != nil {
		return "", err
	}
	if _, err := runGit(ctx, dir, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return "", err
	}

	if err := installHook(dir, "pre-receive", "pre-receive", m.HookBin); err != nil {
		return "", err
	}
	if err := installHook(dir, "post-receive", "post-receive", m.HookBin); err != nil {
		return "", err
	}
	return dir, nil
}

// loginLock lazily creates (and reuses) the per-login provisioning mutex.
func (m *RepoManager) loginLock(login string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = make(map[string]*sync.Mutex)
	}
	lock, ok := m.locks[login]
	if !ok {
		lock = &sync.Mutex{}
		m.locks[login] = lock
	}
	return lock
}

// DefaultBranch reads the branch gitDir's HEAD points at.
func (m *RepoManager) DefaultBranch(ctx context.Context, gitDir string) (string, error) {
	out, err := runGit(ctx, gitDir, "symbolic-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(out, "refs/heads/"), nil
}

// installHook writes a shim at gitDir/hooks/name that execs `hookBin hook
// kind`, matching the signature git invokes receive hooks with.
//
// Every git access re-installs the hooks, and the per-login lock is released
// before receive-pack starts, so a rewrite here overlaps live pushes of the
// same student. An in-place O_TRUNC write would give one of them an empty or
// half-written file: a truncated pre-receive lets a reserved ref through, a
// truncated post-receive drops the submission. The content is therefore left
// alone when it already matches, and otherwise swapped in by rename, which
// never exposes a partial file to an exec.
func installHook(gitDir, name, kind, hookBin string) error {
	path := filepath.Join(gitDir, "hooks", name)
	script := fmt.Sprintf("#!/bin/sh\n# installed by anygrade; do not edit\nexec %s hook %s\n", hookBin, kind)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == script {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeded
	if _, err := tmp.WriteString(script); err != nil {
		tmp.Close()
		return err
	}
	// CreateTemp makes the file 0600; git only runs a hook that is executable.
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Git runs `git -C dir <args...>`, returning trimmed combined output.
// Exported for intake's ref bookkeeping (baseline and submission refs).
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	return runGit(ctx, dir, args...)
}

// runGit runs `git -C dir <args...>`, returning trimmed combined output.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	return run(ctx, append([]string{"-C", dir}, args...)...)
}

// runGitAt runs `git <args...>` with no -C target (e.g. clone, where the
// destination directory does not exist yet).
func runGitAt(ctx context.Context, args ...string) (string, error) {
	return run(ctx, args...)
}

// run execs the system git binary and returns trimmed combined output,
// wrapping any failure with the arguments and output for context.
func run(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, trimmed)
	}
	return trimmed, nil
}
