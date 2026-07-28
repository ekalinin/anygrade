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

	"github.com/ekalinin/anygrade/internal/ident"
)

// defaultMaxInputSize is the receive.maxInputSize applied when
// RepoManager.MaxInputSize is unset.
const defaultMaxInputSize = 50 << 20

// ErrMirrorRefresh marks a failed mirror refresh from the --repo working
// copy. The mirror is the source of truth (teachers push to it directly),
// so a stale working copy is expected and non-fatal.
var ErrMirrorRefresh = errors.New("course mirror refresh skipped")

// RepoManager provisions and locates the bare repos in the data dir.
type RepoManager struct {
	DataDir      string // anygrade data dir; repos live under DataDir/repos
	HookBin      string // absolute path of the anygrade binary, exec'd by hook shims
	MaxInputSize int64  // receive.maxInputSize in bytes; 0 = default 50 MB

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

func (m *RepoManager) maxInputSize() int64 {
	if m.MaxInputSize > 0 {
		return m.MaxInputSize
	}
	return defaultMaxInputSize
}

// EnsureCourse creates the course mirror on first use, or refreshes it with
// a non-forced fetch on subsequent calls; hooks and config are (re)installed
// unconditionally so a HookBin move is picked up.
func (m *RepoManager) EnsureCourse(ctx context.Context, srcRepo string) error {
	dir := m.CourseDir()
	var refreshErr error
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
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

	if _, err := runGit(ctx, dir, "config", "receive.maxInputSize", strconv.FormatInt(m.maxInputSize(), 10)); err != nil {
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
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
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

	if _, err := runGit(ctx, dir, "config", "receive.maxInputSize", strconv.FormatInt(m.maxInputSize(), 10)); err != nil {
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
func installHook(gitDir, name, kind, hookBin string) error {
	path := filepath.Join(gitDir, "hooks", name)
	script := fmt.Sprintf("#!/bin/sh\n# installed by anygrade; do not edit\nexec %s hook %s\n", hookBin, kind)
	return os.WriteFile(path, []byte(script), 0o755)
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
