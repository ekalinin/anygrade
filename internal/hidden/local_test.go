package hidden

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalRootsUnsetAllowsAnyPath pins the compatibility decision: an operator
// who never sets the variable keeps the behaviour every existing course.yaml
// was written against.
func TestLocalRootsUnsetAllowsAnyPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(LocalRootsEnv, "")
	if _, err := LocalSource(dir, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("unset %s must allow any path, got %v", LocalRootsEnv, err)
	}
}

// TestLocalRootsAllowlist: with the variable set, only paths inside one of the
// roots resolve; everything else is a terminal config fault whose message says
// nothing about the filesystem.
func TestLocalRootsAllowlist(t *testing.T) {
	allowed := t.TempDir()
	other := t.TempDir()
	inside := filepath.Join(allowed, "01-intro")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(LocalRootsEnv, "/nonexistent-root:"+allowed)

	if _, err := LocalSource(inside, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("path inside an allowed root: %v", err)
	}
	if _, err := LocalSource(allowed, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("the root itself must be allowed: %v", err)
	}

	_, err := LocalSource(other, slog.New(slog.DiscardHandler))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("path outside every root: got %v, want ErrConfig", err)
	}
	if strings.Contains(err.Error(), other) {
		t.Errorf("error text leaks the path: %s", err)
	}
}

// TestLocalRootsEscapeAttempts: neither `..` nor a symlink planted inside an
// allowed root widens it.
func TestLocalRootsEscapeAttempts(t *testing.T) {
	allowed := t.TempDir()
	secret := t.TempDir()
	link := filepath.Join(allowed, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	for _, path := range []string{link, filepath.Join(allowed, "..")} {
		if err := checkLocalRoots(path, allowed); err == nil {
			t.Errorf("checkLocalRoots(%q, %q) = nil, want a rejection", path, allowed)
		}
	}
}

// TestLocalSourceErrorClasses: only "not there" may be terminal. A path that
// exists but cannot be read right now (a network mount blinking, a permission
// being repaired) has to stay retryable, or one EIO terminally fails every
// in-flight submission (SPEC §13).
func TestLocalSourceErrorClasses(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	log := slog.New(slog.DiscardHandler)

	_, err := LocalSource(filepath.Join(t.TempDir(), "not-there"), log)
	if !errors.Is(err, ErrConfig) {
		t.Errorf("missing path: got %v, want ErrConfig", err)
	}

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LocalSource(file, log); !errors.Is(err, ErrConfig) {
		t.Errorf("path that is not a directory: got %v, want ErrConfig", err)
	}

	// A directory whose parent denies traversal: stat fails with EACCES.
	parent := t.TempDir()
	child := filepath.Join(parent, "hidden")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	_, err = LocalSource(child, log)
	if err == nil {
		t.Fatal("an unreadable path must still fail the submission")
	}
	if errors.Is(err, ErrConfig) {
		t.Errorf("an unreadable path must stay retryable, got ErrConfig: %v", err)
	}
	if strings.Contains(err.Error(), child) {
		t.Errorf("error text leaks the path: %s", err)
	}
}

// leakySource fails the way a git or working-copy source does: with the host
// path and the tool's stderr in the message.
type leakySource struct{ msg string }

func (s leakySource) Export(context.Context, string, string) error {
	return errors.New(s.msg)
}

func (s leakySource) Open(context.Context, string) (io.ReadCloser, bool, error) {
	return nil, false, errors.New(s.msg)
}

// leakyStream opens fine and then leaks the same way from the stream itself,
// which is how a git-backed source actually reports a bad object.
type leakyStream struct{ msg string }

func (s leakyStream) Export(context.Context, string, string) error { return nil }

func (s leakyStream) Open(context.Context, string) (io.ReadCloser, bool, error) {
	return failingReadCloser{msg: s.msg}, true, nil
}

type failingReadCloser struct{ msg string }

func (f failingReadCloser) Read([]byte) (int, error) { return 0, errors.New(f.msg) }
func (f failingReadCloser) Close() error             { return errors.New(f.msg) }

// TestScrubbedHidesHostPaths: an export or read failure reaches the student as
// the neutral wording, never as a host path - the error ends up in
// submissions.worker_note (SPEC §14).
func TestScrubbedHidesHostPaths(t *testing.T) {
	const leak = "git -C /srv/anygrade/hidden/9f3: fatal: bad object"
	s := scrubbed{inner: leakySource{msg: leak}, log: slog.New(slog.DiscardHandler)}

	err := s.Export(t.Context(), "", t.TempDir())
	if err == nil || err.Error() != "hidden tests temporarily unavailable" {
		t.Errorf("Export error = %v, want the scrubbed wording", err)
	}
	if _, _, err := s.Open(t.Context(), "x"); err == nil ||
		err.Error() != "hidden tests temporarily unavailable" {
		t.Errorf("Open error = %v, want the scrubbed wording", err)
	}

	// The source interface streams now, so the wrapper has to cover the stream
	// as well: an error that only surfaces from Read or Close reaches
	// worker_note by exactly the same route as one from Open.
	st := scrubbed{inner: leakyStream{msg: leak}, log: slog.New(slog.DiscardHandler)}
	rc, ok, err := st.Open(t.Context(), "x")
	if err != nil || !ok {
		t.Fatalf("Open = %v, %v, want a stream", ok, err)
	}
	if _, err := rc.Read(make([]byte, 8)); err == nil ||
		err.Error() != "hidden tests temporarily unavailable" {
		t.Errorf("Read error = %v, want the scrubbed wording", err)
	}
	if err := rc.Close(); err == nil ||
		err.Error() != "hidden tests temporarily unavailable" {
		t.Errorf("Close error = %v, want the scrubbed wording", err)
	}
}

// TestSourcesAreScrubbed pins the wiring: both hidden-tests sources hand out a
// wrapped source, so no path from either can reach worker_note.
func TestSourcesAreScrubbed(t *testing.T) {
	url, _ := newRemote(t)
	git, err := newCache(t).Source(t.Context(), spec(url, "main", "01-intro"))
	if err != nil {
		t.Fatal(err)
	}
	local, err := LocalSource(t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]any{"git": git, "local": local} {
		if _, ok := src.(scrubbed); !ok {
			t.Errorf("%s source is %T, want scrubbed", name, src)
		}
	}
}
