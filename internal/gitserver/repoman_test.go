package gitserver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// requireGit skips the test if the system git binary is unavailable.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

// newSrcRepo creates a source repo with one commit on "main".
func newSrcRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runSrc(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSrc(t, dir, "add", "README.md")
	runSrc(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "init")
	return dir
}

func runSrc(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestEnsureCourseCreatesMirror(t *testing.T) {
	requireGit(t)
	ctx := t.Context()
	src := newSrcRepo(t)
	m := &RepoManager{DataDir: t.TempDir(), HookBin: "/usr/local/bin/anygrade"}

	if err := m.EnsureCourse(ctx, src); err != nil {
		t.Fatalf("EnsureCourse: %v", err)
	}

	dir := m.CourseDir()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("course dir missing: %v", err)
	}
	if out := runSrc(t, dir, "rev-parse", "--is-bare-repository"); out != "true" {
		t.Fatalf("expected bare repo, got %q", out)
	}

	checkHook(t, dir, "pre-receive", "hook validate-course")
	checkHook(t, dir, "post-receive", "hook post-receive")

	if out := runSrc(t, dir, "config", "receive.maxInputSize"); out != "52428800" {
		t.Fatalf("receive.maxInputSize = %q, want 52428800", out)
	}
	// Left at git's default, a fetch or a push returns while a detached repack
	// is still writing into the mirror.
	if out := runSrc(t, dir, "config", "gc.autoDetach"); out != "false" {
		t.Fatalf("gc.autoDetach = %q, want false", out)
	}

	branch, err := m.DefaultBranch(ctx, dir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if branch != "main" {
		t.Fatalf("DefaultBranch = %q, want main", branch)
	}
}

func TestEnsureCourseRefreshFastForward(t *testing.T) {
	requireGit(t)
	ctx := t.Context()
	src := newSrcRepo(t)
	m := &RepoManager{DataDir: t.TempDir(), HookBin: "/usr/local/bin/anygrade"}

	if err := m.EnsureCourse(ctx, src); err != nil {
		t.Fatalf("EnsureCourse: %v", err)
	}

	if err := os.WriteFile(filepath.Join(src, "second.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSrc(t, src, "add", "second.txt")
	runSrc(t, src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "second")

	if err := m.EnsureCourse(ctx, src); err != nil {
		t.Fatalf("EnsureCourse (refresh): %v", err)
	}

	wantHead := runSrc(t, src, "rev-parse", "main")
	gotHead := runSrc(t, m.CourseDir(), "rev-parse", "main")
	if gotHead != wantHead {
		t.Fatalf("mirror head = %q, want %q", gotHead, wantHead)
	}
}

func TestEnsureStudent(t *testing.T) {
	requireGit(t)
	ctx := t.Context()
	src := newSrcRepo(t)
	m := &RepoManager{DataDir: t.TempDir(), HookBin: "/usr/local/bin/anygrade"}

	if err := m.EnsureCourse(ctx, src); err != nil {
		t.Fatalf("EnsureCourse: %v", err)
	}

	dir, err := m.EnsureStudent(ctx, "alice")
	if err != nil {
		t.Fatalf("EnsureStudent: %v", err)
	}
	if dir != m.StudentDir("alice") {
		t.Fatalf("dir = %q, want %q", dir, m.StudentDir("alice"))
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("student dir missing: %v", err)
	}
	if out := runSrc(t, dir, "rev-parse", "--is-bare-repository"); out != "true" {
		t.Fatalf("expected bare repo, got %q", out)
	}
	if out := runSrc(t, dir, "config", "transfer.hideRefs"); out != "refs/anygrade" {
		t.Fatalf("transfer.hideRefs = %q, want refs/anygrade", out)
	}
	// The student repos take pushes, and receive-pack fires the same detached
	// auto-gc the mirror is protected from.
	if out := runSrc(t, dir, "config", "gc.autoDetach"); out != "false" {
		t.Fatalf("gc.autoDetach = %q, want false", out)
	}
	if out := runSrc(t, dir, "symbolic-ref", "HEAD"); out != "refs/heads/main" {
		t.Fatalf("HEAD = %q, want refs/heads/main", out)
	}
	checkHook(t, dir, "pre-receive", "hook pre-receive")
	checkHook(t, dir, "post-receive", "hook post-receive")

	// Idempotent: second call returns the same path with no error.
	dir2, err := m.EnsureStudent(ctx, "alice")
	if err != nil {
		t.Fatalf("EnsureStudent (second): %v", err)
	}
	if dir2 != dir {
		t.Fatalf("second call dir = %q, want %q", dir2, dir)
	}

	if _, err := m.EnsureStudent(ctx, "../evil"); err == nil {
		t.Fatal("expected error for login with \"..\"")
	}
	if _, err := m.EnsureStudent(ctx, "UPPER"); err == nil {
		t.Fatal("expected error for uppercase login")
	}
}

func TestEnsureStudentConcurrent(t *testing.T) {
	requireGit(t)
	ctx := t.Context()
	src := newSrcRepo(t)
	m := &RepoManager{DataDir: t.TempDir(), HookBin: "/usr/local/bin/anygrade"}

	if err := m.EnsureCourse(ctx, src); err != nil {
		t.Fatalf("EnsureCourse: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range 8 {
		wg.Go(func() {
			_, err := m.EnsureStudent(ctx, "bob")
			errs[i] = err
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if out := runSrc(t, m.StudentDir("bob"), "rev-parse", "--is-bare-repository"); out != "true" {
		t.Fatalf("expected bare repo, got %q", out)
	}
}

func checkHook(t *testing.T, gitDir, name, want string) {
	t.Helper()
	path := filepath.Join(gitDir, "hooks", name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("hook %s missing: %v", name, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("hook %s not executable: %v", name, info.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook %s: %v", name, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("hook %s = %q, want it to contain %q", name, data, want)
	}
}
