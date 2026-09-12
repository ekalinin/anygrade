package gitserver

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
// on stderr) or a message that names neither the limit nor a way out. The tail
// of the output is pinned as well: git reports the ref as failed only when it
// got to read the answer to its own upload, so "! [remote failure]" is what
// tells a refusal apart from a connection cut from under the client.
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
	for _, want := range []string{"anygrade: push rejected", "max_push_size", "64 KB",
		"! [remote failure]", "failed to push some refs"} {
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

// trickle is a request body that never ends: one byte at a time until the test
// lets go of it, which is what a client holding a chunked body open looks like
// from the server's side.
type trickle struct {
	gap  time.Duration
	stop <-chan struct{}
}

func (tr *trickle) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	select {
	case <-tr.stop:
		return 0, io.EOF
	case <-time.After(tr.gap):
		p[0] = 0
		return 1, nil
	}
}

// TestHTTPOversizePushDrainIsBounded: the drain that follows a tripped guard is
// there so git reads the answer instead of retrying the whole push, but it must
// not become a way to hold the server. The listener carries no read timeout on
// purpose (SPEC §14), so the drain brings its own deadline, and a client that
// keeps its body open past it is let go of rather than waited for.
func TestHTTPOversizePushDrainIsBounded(t *testing.T) {
	requireGit(t)
	const limit = 64 << 10
	src := newSrcRepo(t)
	rm := &RepoManager{DataDir: t.TempDir(), HookBin: "/usr/bin/true"}
	if err := rm.EnsureCourse(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	if err := rm.SetMaxInputSize(t.Context(), limit); err != nil {
		t.Fatal(err)
	}
	h := &HTTPHandler{
		Repos:  rm,
		Auth:   fakeAuth{tokens: map[string]string{"alice": "tok"}, ids: map[string]Identity{"alice": {UserID: 1, Login: "alice", Role: "student"}}},
		Socket: filepath.Join(t.TempDir(), "no.sock"),
	}
	// A real oversized push comes first, to keep a real receive-pack request to
	// replay: the guard has to trip on git's own bytes, not on filler. Only
	// that push is buffered - the replay must reach the handler as it arrives,
	// which is the whole point of it.
	var mu sync.Mutex
	var captured []byte
	var capturing atomic.Bool
	capturing.Store(true)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capturing.Load() && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error("reading the request body:", err)
			}
			mu.Lock()
			if len(body) > len(captured) {
				captured = body
			}
			mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	work := filepath.Join(t.TempDir(), "wc")
	if out, err := runGitCmd(t, ".", "clone", authURL(t, ts.URL, "alice", "tok", "/git/alice/course.git"), work); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	commitBigFile(t, work, 2<<20)
	if out, err := runGitCmd(t, work, "push", "origin", "main"); err == nil {
		t.Fatalf("an oversized push must be rejected, got: %s", out)
	}
	capturing.Store(false)
	mu.Lock()
	pack := captured
	mu.Unlock()
	if int64(len(pack)) <= limit {
		t.Fatalf("the captured request is %d bytes, too small to trip the %d-byte cap", len(pack), limit)
	}

	// The same request again, from a client that never finishes it.
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	body := io.MultiReader(bytes.NewReader(pack), &trickle{gap: 100 * time.Millisecond, stop: stop})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/git/alice/course.git/git-receive-pack", body)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("alice", "tok")
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.ContentLength = -1 // chunked: the client never says how much is coming

	type answer struct {
		took time.Duration
		body string
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		started := time.Now()
		got := answer{}
		resp, err := (&http.Client{}).Do(req)
		if got.err = err; err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			got.body = string(b)
		}
		got.took = time.Since(started)
		done <- got
	}()

	bound := 2 * oversizeDrainTimeout
	select {
	case got := <-done:
		if got.took > bound {
			t.Errorf("the refused push was held for %v, want at most %v", got.took, bound)
		}
		if got.err != nil {
			t.Fatalf("the refused push got no answer: %v", got.err)
		}
		if !strings.Contains(got.body, "anygrade: push rejected") {
			t.Errorf("the answer to the refused push is missing the rejection: %q", got.body)
		}
	case <-time.After(bound):
		t.Fatalf("the refused push was still being read %v after the guard tripped", bound)
	}
}

// TestSSHPushOverMaxPushSize is the same rejection over the other transport;
// the wording must not depend on how the student is connected.
func TestSSHPushOverMaxPushSize(t *testing.T) {
	port, sshCmd, _, rm := newSSHFixture(t)
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
