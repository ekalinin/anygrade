package hidden

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
)

// fileAllowEnv permits file:// remotes in the cache's fetches (the test
// stand-in for http(s); some git installs restrict the file transport).
var fileAllowEnv = []string{
	"GIT_CONFIG_COUNT=1",
	"GIT_CONFIG_KEY_0=protocol.file.allow",
	"GIT_CONFIG_VALUE_0=always",
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), fileAllowEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRemote builds a work repo (top.txt + 01-intro/hidden_check.sh, branch
// main, tag v1.0) and a bare clone acting as the private remote. It returns
// the remote's file:// URL and the work dir for later pushes.
func newRemote(t *testing.T) (url, work string) {
	t.Helper()
	requireGit(t)
	work = filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(filepath.Join(work, "01-intro"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, ".", "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "top.txt"), []byte("top\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "01-intro", "hidden_check.sh"), []byte("echo v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "hidden v1")
	runGit(t, work, "tag", "v1.0")

	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, ".", "clone", "-q", "--bare", work, remote)
	return "file://" + remote, work
}

func newCache(t *testing.T) *Cache {
	t.Helper()
	return &Cache{
		Dir: t.TempDir(),
		Env: fileAllowEnv,
		Log: slog.New(slog.DiscardHandler),
	}
}

func spec(url, ref, path string) config.HiddenTests {
	return config.HiddenTests{Source: "git", URL: url, Ref: ref, Path: path}
}

func exportDir(t *testing.T, c *Cache, s config.HiddenTests) string {
	t.Helper()
	src, err := c.Source(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := src.Export(t.Context(), "", dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestSourceSubdirOverlay: path: re-roots the overlay so the subdir contents
// land directly in the destination (SPEC §6.1 step 3).
func TestSourceSubdirOverlay(t *testing.T) {
	url, _ := newRemote(t)
	c := newCache(t)
	// Trailing slash as in the SPEC §4.3 example.
	dst := exportDir(t, c, spec(url, "main", "01-intro/"))

	if _, err := os.Stat(filepath.Join(dst, "hidden_check.sh")); err != nil {
		t.Fatal("subdir content not re-rooted:", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "01-intro")); err == nil {
		t.Fatal("path prefix leaked into the overlay")
	}

	src, err := c.Source(t.Context(), spec(url, "main", "01-intro"))
	if err != nil {
		t.Fatal(err)
	}
	data, ok, err := src.File(t.Context(), "hidden_check.sh")
	if err != nil || !ok || string(data) != "echo v1\n" {
		t.Fatalf("File: %q %v %v", data, ok, err)
	}
}

// TestSourceTagWholeRepo: a tag ref, no path: = the whole repo overlaid.
func TestSourceTagWholeRepo(t *testing.T) {
	url, _ := newRemote(t)
	dst := exportDir(t, newCache(t), spec(url, "v1.0", ""))
	for _, p := range []string{"top.txt", "01-intro/hidden_check.sh"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(p))); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

// TestTTLSkipsNetwork: within the TTL the remote is not contacted at all.
func TestTTLSkipsNetwork(t *testing.T) {
	url, _ := newRemote(t)
	c := newCache(t) // default TTL 60s
	if _, err := c.Source(t.Context(), spec(url, "main", "")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(strings.TrimPrefix(url, "file://")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Source(t.Context(), spec(url, "main", "")); err != nil {
		t.Fatal("fresh entry hit the network:", err)
	}
}

// TestOfflineWarmFallback: remote gone, TTL expired - the pinned last-good
// commit is served without an error (SPEC §13).
func TestOfflineWarmFallback(t *testing.T) {
	url, _ := newRemote(t)
	c := newCache(t)
	c.TTL = time.Nanosecond
	if _, err := c.Source(t.Context(), spec(url, "main", "01-intro")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(strings.TrimPrefix(url, "file://")); err != nil {
		t.Fatal(err)
	}
	src, err := c.Source(t.Context(), spec(url, "main", "01-intro"))
	if err != nil {
		t.Fatal("warm cache must survive a dead remote:", err)
	}
	if _, ok, _ := src.File(t.Context(), "hidden_check.sh"); !ok {
		t.Fatal("fallback source lost the content")
	}
}

// TestOfflineColdError: no cache and a dead remote is a retryable error whose
// text leaks neither the URL nor git output (SPEC §14).
func TestOfflineColdError(t *testing.T) {
	requireGit(t)
	url := "file:///no/such/remote.git"
	_, err := newCache(t).Source(t.Context(), spec(url, "main", ""))
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrConfig) {
		t.Fatal("cold offline must be retryable, not ErrConfig")
	}
	for _, leak := range []string{url, "fatal", "/no/such"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error text leaks %q: %s", leak, err)
		}
	}
}

// TestPathMissingIsConfigError: a path: absent at the resolved commit is a
// teacher config fault - terminal, not retried.
func TestPathMissingIsConfigError(t *testing.T) {
	url, _ := newRemote(t)
	_, err := newCache(t).Source(t.Context(), spec(url, "main", "no-such-dir"))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("got %v, want ErrConfig", err)
	}
	if strings.Contains(err.Error(), url) {
		t.Errorf("error text leaks the URL: %s", err)
	}
}

// TestForcePushPicksNewCommit: a rewritten ref is picked up once the TTL
// window expires.
func TestForcePushPicksNewCommit(t *testing.T) {
	url, work := newRemote(t)
	c := newCache(t)
	c.TTL = time.Nanosecond

	src, err := c.Source(t.Context(), spec(url, "main", "01-intro"))
	if err != nil {
		t.Fatal(err)
	}
	if data, _, _ := src.File(t.Context(), "hidden_check.sh"); string(data) != "echo v1\n" {
		t.Fatalf("v1 content: %q", data)
	}

	if err := os.WriteFile(filepath.Join(work, "01-intro", "hidden_check.sh"), []byte("echo v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--amend", "-m", "hidden v2")
	runGit(t, work, "push", "-q", "-f", strings.TrimPrefix(url, "file://"), "main")

	src, err = c.Source(t.Context(), spec(url, "main", "01-intro"))
	if err != nil {
		t.Fatal(err)
	}
	if data, _, _ := src.File(t.Context(), "hidden_check.sh"); string(data) != "echo v2\n" {
		t.Fatalf("after force push: %q", data)
	}
}

// TestConcurrentSameURL: the worker pool shape - N goroutines, one URL.
func TestConcurrentSameURL(t *testing.T) {
	url, _ := newRemote(t)
	c := newCache(t)
	s := spec(url, "main", "01-intro")

	var wg sync.WaitGroup
	contents := make([]string, 8)
	errs := make([]error, 8)
	for i := range 8 {
		wg.Go(func() {
			src, err := c.Source(context.Background(), s)
			if err != nil {
				errs[i] = err
				return
			}
			data, _, err := src.File(context.Background(), "hidden_check.sh")
			contents[i], errs[i] = string(data), err
		})
	}
	wg.Wait()
	for i := range 8 {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if contents[i] != contents[0] {
			t.Fatalf("divergent content: %q vs %q", contents[i], contents[0])
		}
	}
}
