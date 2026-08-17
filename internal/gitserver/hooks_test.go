package gitserver

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestInstallHookLeavesIdenticalContentAlone: every git access re-installs the
// hooks while other pushes of the same student are running, so the common case
// - the shim is already what it should be - must not touch the file at all.
func TestInstallHookLeavesIdenticalContentAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hooks", "pre-receive")

	if err := installHook(dir, "pre-receive", "pre-receive", "/opt/anygrade"); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := installHook(dir, "pre-receive", "pre-receive", "/opt/anygrade"); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("an unchanged hook must not be rewritten")
	}
	if perm := second.Mode().Perm(); perm&0o111 == 0 {
		t.Errorf("hook mode %v is not executable", perm)
	}
}

// TestInstallHookIsAtomic: a reader must never see a truncated shim. A
// half-written pre-receive skips the reserved-ref guard and a half-written
// post-receive drops the submission, and both are what an in-place O_TRUNC
// rewrite produces for a concurrent push.
func TestInstallHookIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hooks", "post-receive")
	if err := installHook(dir, "post-receive", "post-receive", "/opt/a"); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"#!/bin/sh\n# installed by anygrade; do not edit\nexec /opt/a hook post-receive\n": true,
		"#!/bin/sh\n# installed by anygrade; do not edit\nexec /opt/b hook post-receive\n": true,
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	bad := make(chan string, 1)
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(path)
				if err != nil {
					continue // the rename window has no missing-file state, but be lenient
				}
				if !want[string(data)] {
					select {
					case bad <- string(data):
					default:
					}
					return
				}
			}
		})
	}
	for i := range 200 {
		bin := "/opt/a"
		if i%2 == 1 {
			bin = "/opt/b"
		}
		if err := installHook(dir, "post-receive", "post-receive", bin); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	select {
	case got := <-bad:
		t.Fatalf("a reader saw a partial hook: %q", got)
	default:
	}

	// No temp file may be left behind for git to try to run.
	entries, err := os.ReadDir(filepath.Join(dir, "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("hooks dir = %v, want only post-receive", entries)
	}
}

// TestRepoDirsAreOwnerOnly: the data dir is owner-only, and the trees below it
// hold student code, so they must not be wider. Whatever git creates inside a
// repo keeps git's own modes - the 0700 repo root is what covers those.
func TestRepoDirsAreOwnerOnly(t *testing.T) {
	requireGit(t)
	ctx := t.Context()
	src := newSrcRepo(t)
	data := t.TempDir()

	// Start from the wider layout an older install left behind.
	wide := filepath.Join(data, "repos", "students")
	if err := os.MkdirAll(wide, 0o755); err != nil {
		t.Fatal(err)
	}

	m := &RepoManager{DataDir: data, HookBin: "/usr/bin/true"}
	if err := m.EnsureCourse(ctx, src); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureStudent(ctx, "alice"); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{
		filepath.Join(data, "repos"),
		filepath.Join(data, "repos", "students"),
		m.CourseDir(),
		m.StudentDir("alice"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %v, want 0700", dir, perm)
		}
	}
}
