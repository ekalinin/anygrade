package gitserver

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commitBigFile puts a file of n bytes into work and commits it. The content is
// random because zlib would otherwise squeeze any pattern back under the cap.
func commitBigFile(t *testing.T, work string, n int) {
	t.Helper()
	blob := make([]byte, n)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "big.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	runSrc(t, work, "add", ".")
	runSrc(t, work, "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-m", "big")
}

// TestMaxPushSizeIsConfigured: the value from course.yaml reaches both the
// in-process cap and the repos' own receive.maxInputSize backstop.
func TestMaxPushSizeIsConfigured(t *testing.T) {
	requireGit(t)
	ctx := t.Context()
	src := newSrcRepo(t)
	m := &RepoManager{DataDir: t.TempDir(), HookBin: "/usr/bin/true"}
	if err := m.EnsureCourse(ctx, src); err != nil {
		t.Fatal(err)
	}
	if got := m.MaxInputSize(); got != defaultMaxInputSize {
		t.Fatalf("default cap = %d, want %d", got, defaultMaxInputSize)
	}

	if err := m.SetMaxInputSize(ctx, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got := m.MaxInputSize(); got != 1<<20 {
		t.Fatalf("cap = %d, want %d", got, 1<<20)
	}
	if out := runSrc(t, m.CourseDir(), "config", "receive.maxInputSize"); out != "1048576" {
		t.Fatalf("course receive.maxInputSize = %q, want 1048576", out)
	}
	// Student repos adopt it at their next provisioning, which every git
	// access performs before receive-pack starts.
	dir, err := m.EnsureStudent(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if out := runSrc(t, dir, "config", "receive.maxInputSize"); out != "1048576" {
		t.Fatalf("student receive.maxInputSize = %q, want 1048576", out)
	}

	// 0 restores the built-in default rather than disabling the cap.
	if err := m.SetMaxInputSize(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := m.MaxInputSize(); got != defaultMaxInputSize {
		t.Fatalf("cap after 0 = %d, want the default %d", got, defaultMaxInputSize)
	}
}

// TestHTTPPushOverMaxPushSize: SPEC §13 asks for an explanatory rejection, and
// git's own is either "unpack-objects abnormal exit" (HTTP swallows the fatal
// on stderr) or a message that names neither the limit nor a way out.
func TestHTTPPushOverMaxPushSize(t *testing.T) {
	ts, rm := newHTTPFixture(t)
	if err := rm.SetMaxInputSize(t.Context(), 64<<10); err != nil {
		t.Fatal(err)
	}
	repoURL := authURL(t, ts.URL, "alice", "tok-a", "/git/alice/course.git")

	work := filepath.Join(t.TempDir(), "wc")
	if out, err := runGitCmd(t, ".", "clone", repoURL, work); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	commitBigFile(t, work, 2<<20)

	out, err := runGitCmd(t, work, "push", "origin", "main")
	if err == nil {
		t.Fatalf("an oversized push must be rejected, got: %s", out)
	}
	for _, want := range []string{"anygrade: push rejected", "max_push_size", "64 KB"} {
		if !strings.Contains(out, want) {
			t.Errorf("push output %q is missing %q", out, want)
		}
	}
	if _, err := runGitCmd(t, rm.StudentDir("alice"), "rev-parse", "--verify", "refs/heads/main^{tree}"); err != nil {
		t.Fatal("the pre-existing branch should still be there:", err)
	}
	head := strings.TrimSpace(runSrc(t, work, "rev-parse", "HEAD"))
	if bare := runSrc(t, rm.StudentDir("alice"), "rev-parse", "main"); bare == head {
		t.Fatal("the oversized push landed anyway")
	}

	// A push under the cap still works, on the very same connection settings.
	runSrc(t, work, "reset", "--hard", "HEAD~1")
	if err := os.WriteFile(filepath.Join(work, "small.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSrc(t, work, "add", ".")
	runSrc(t, work, "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-m", "small")
	if out, err := runGitCmd(t, work, "push", "origin", "main"); err != nil {
		t.Fatalf("a push under the cap must still work: %v: %s", err, out)
	}
}

// TestSSHPushOverMaxPushSize is the same rejection over the other transport;
// the wording must not depend on how the student is connected.
func TestSSHPushOverMaxPushSize(t *testing.T) {
	port, sshCmd, rm := newSSHFixture(t)
	if err := rm.SetMaxInputSize(t.Context(), 64<<10); err != nil {
		t.Fatal(err)
	}
	repoURL := fmt.Sprintf("ssh://git@127.0.0.1:%d/alice/course.git", port)

	work := filepath.Join(t.TempDir(), "wc")
	if out, err := gitSSH(t, sshCmd, ".", "clone", repoURL, work); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	commitBigFile(t, work, 2<<20)

	out, err := gitSSH(t, sshCmd, work, "push", "origin", "main")
	if err == nil {
		t.Fatalf("an oversized push must be rejected, got: %s", out)
	}
	for _, want := range []string{"anygrade: push rejected", "max_push_size", "64 KB"} {
		if !strings.Contains(out, want) {
			t.Errorf("push output %q is missing %q", out, want)
		}
	}
	head := strings.TrimSpace(runSrc(t, work, "rev-parse", "HEAD"))
	if bare := runSrc(t, rm.StudentDir("alice"), "rev-parse", "main"); bare == head {
		t.Fatal("the oversized push landed anyway")
	}
}

// TestOversizeReaderBoundary: a push of exactly the cap passes, one byte more
// trips - an off-by-one here either rejects legitimate pushes or lets the cap
// be walked past.
func TestOversizeReaderBoundary(t *testing.T) {
	for _, tc := range []struct {
		size int64
		hit  bool
	}{{4, false}, {5, true}} {
		r := newOversizeReader(bytes.NewReader(make([]byte, tc.size)), 4)
		buf := make([]byte, 8)
		for {
			if _, err := r.Read(buf); err != nil {
				break
			}
		}
		if r.hit != tc.hit {
			t.Errorf("%d bytes over a cap of 4: hit = %v, want %v", tc.size, r.hit, tc.hit)
		}
	}
}

func TestHumanSize(t *testing.T) {
	tests := map[int64]string{
		50 << 20: "50 MB",
		64 << 10: "64 KB",
		2 << 30:  "2 GB",
		1234:     "1234 bytes",
	}
	for n, want := range tests {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}
