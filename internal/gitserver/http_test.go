package gitserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAuth authenticates from a fixed login -> (token, Identity) map.
type fakeAuth struct {
	tokens map[string]string // login -> token
	ids    map[string]Identity
}

func (f fakeAuth) ByToken(_ context.Context, login, token string) (Identity, bool, error) {
	if f.tokens[login] != token || token == "" {
		return Identity{}, false, nil
	}
	return f.ids[login], true, nil
}

func (f fakeAuth) ByFingerprint(_ context.Context, fp string) (Identity, bool, error) {
	id, ok := f.ids[fp]
	return id, ok, nil
}

func newHTTPFixture(t *testing.T) (ts *httptest.Server, rm *RepoManager) {
	t.Helper()
	requireGit(t)
	src := newSrcRepo(t)
	rm = &RepoManager{DataDir: t.TempDir(), HookBin: "/usr/bin/true"}
	if err := rm.EnsureCourse(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	h := &HTTPHandler{
		Repos: rm,
		Auth: fakeAuth{
			tokens: map[string]string{"alice": "tok-a", "prof": "tok-p"},
			ids: map[string]Identity{
				"alice": {UserID: 1, Login: "alice", Role: "student"},
				"prof":  {UserID: 2, Login: "prof", Role: "teacher"},
			},
		},
		Socket: filepath.Join(t.TempDir(), "no.sock"),
	}
	ts = httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, rm
}

// authURL injects basic-auth credentials into the test server URL.
func authURL(t *testing.T, base, login, token, repoPath string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(login, token)
	u.Path = repoPath
	return u.String()
}

func gitEnvQuiet() []string {
	// Fail instead of prompting when auth is rejected.
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/usr/bin/false")
}

func runGitCmd(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnvQuiet()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestHTTPCloneAndPush: the full student flow over smart HTTP: lazy repo
// provisioning on first clone, then a push that lands in the bare repo.
func TestHTTPCloneAndPush(t *testing.T) {
	ts, rm := newHTTPFixture(t)
	repoURL := authURL(t, ts.URL, "alice", "tok-a", "/git/alice/course.git")

	work := filepath.Join(t.TempDir(), "wc")
	if out, err := runGitCmd(t, ".", "clone", repoURL, work); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(work, "README.md")); err != nil {
		t.Fatal("clone is missing course content:", err)
	}

	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("solved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSrc(t, work, "add", ".")
	runSrc(t, work, "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-m", "solve")
	if out, err := runGitCmd(t, work, "push", "origin", "main"); err != nil {
		t.Fatalf("push: %v: %s", err, out)
	}

	head := runSrc(t, work, "rev-parse", "HEAD")
	bareHead := runSrc(t, rm.StudentDir("alice"), "rev-parse", "main")
	if head != bareHead {
		t.Fatalf("bare repo head %s, want %s", bareHead, head)
	}
}

// TestHTTPAuthz: the §7 access matrix over live git operations.
func TestHTTPAuthz(t *testing.T) {
	ts, _ := newHTTPFixture(t)

	clone := func(login, token, repoPath string) error {
		_, err := runGitCmd(t, ".", "clone",
			authURL(t, ts.URL, login, token, repoPath),
			filepath.Join(t.TempDir(), "wc"))
		return err
	}

	if err := clone("alice", "wrong", "/git/alice/course.git"); err == nil {
		t.Error("clone with a bad token must fail")
	}
	if err := clone("alice", "tok-a", "/git/course.git"); err != nil {
		t.Error("student read of the course repo must work:", err)
	}
	if err := clone("alice", "tok-a", "/git/bob/course.git"); err == nil {
		t.Error("cross-student clone must fail")
	}
	// Teacher reads a provisioned student repo.
	if err := clone("alice", "tok-a", "/git/alice/course.git"); err != nil {
		t.Fatal(err)
	}
	if err := clone("prof", "tok-p", "/git/alice/course.git"); err != nil {
		t.Error("teacher read of a student repo must work:", err)
	}

	// Student push to the course repo is rejected.
	work := filepath.Join(t.TempDir(), "wc")
	if _, err := runGitCmd(t, ".", "clone", authURL(t, ts.URL, "alice", "tok-a", "/git/course.git"), work); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "hack.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSrc(t, work, "add", ".")
	runSrc(t, work, "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-m", "hack")
	if out, err := runGitCmd(t, work, "push", "origin", "main"); err == nil {
		t.Errorf("student push to the course repo must fail, got: %s", out)
	}
}

// TestHTTPGzippedRequestBody forces the gzip request path (design risk #2):
// small pushes never compress, so a proxy gzips every POST body between a
// real git client and the handler.
func TestHTTPGzippedRequestBody(t *testing.T) {
	requireGit(t)
	src := newSrcRepo(t)
	rm := &RepoManager{DataDir: t.TempDir(), HookBin: "/usr/bin/true"}
	if err := rm.EnsureCourse(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	h := &HTTPHandler{
		Repos:  rm,
		Auth:   fakeAuth{tokens: map[string]string{"alice": "tok"}, ids: map[string]Identity{"alice": {UserID: 1, Login: "alice", Role: "student"}}},
		Socket: filepath.Join(t.TempDir(), "no.sock"),
	}
	gzipping := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			h.ServeHTTP(w, r)
			return
		}
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := io.Copy(gz, r.Body); err != nil {
			t.Error(err)
		}
		if err := gz.Close(); err != nil {
			t.Error(err)
		}
		r.Body = io.NopCloser(&buf)
		r.ContentLength = int64(buf.Len())
		r.Header.Set("Content-Encoding", "gzip")
		h.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(gzipping)
	t.Cleanup(ts.Close)

	work := filepath.Join(t.TempDir(), "wc")
	repoURL := authURL(t, ts.URL, "alice", "tok", "/git/alice/course.git")
	if out, err := runGitCmd(t, ".", "clone", repoURL, work); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(work, "big.txt"), bytes.Repeat([]byte("data\n"), 200_000), 0o644); err != nil {
		t.Fatal(err)
	}
	runSrc(t, work, "add", ".")
	runSrc(t, work, "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-m", "big")
	if out, err := runGitCmd(t, work, "push", "origin", "main"); err != nil {
		t.Fatalf("gzipped push: %v: %s", err, out)
	}
	if head := runSrc(t, work, "rev-parse", "HEAD"); head != runSrc(t, rm.StudentDir("alice"), "rev-parse", "main") {
		t.Fatal("push did not land")
	}
}

// TestSplitRepoPath covers the URL shapes of SPEC §7.
func TestSplitRepoPath(t *testing.T) {
	tests := []struct {
		path        string
		owner, rest string
		ok          bool
	}{
		{"/git/course.git/info/refs", "", "info/refs", true},
		{"/git/alice/course.git/git-receive-pack", "alice", "git-receive-pack", true},
		{"/git/alice/other.git/info/refs", "", "", false},
		{"/git/course.git", "", "", false},
		{"/other/course.git/info/refs", "", "", false},
		{"/git//course.git/info/refs", "", "", false},
	}
	for _, tc := range tests {
		owner, rest, ok := splitRepoPath(tc.path)
		if owner != tc.owner || rest != tc.rest || ok != tc.ok {
			t.Errorf("splitRepoPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.path, owner, rest, ok, tc.owner, tc.rest, tc.ok)
		}
	}
}

// TestAuthorize is the pure policy matrix.
func TestAuthorize(t *testing.T) {
	student := Identity{Login: "alice", Role: "student"}
	teacher := Identity{Login: "prof", Role: "teacher"}
	tests := []struct {
		id    Identity
		owner string
		write bool
		want  bool
	}{
		{student, "alice", true, true},
		{student, "alice", false, true},
		{student, "bob", false, false},
		{student, "", false, true},
		{student, "", true, false},
		{teacher, "", true, true},
		{teacher, "alice", true, true},
	}
	for _, tc := range tests {
		if got := Authorize(tc.id, tc.owner, tc.write); got != tc.want {
			t.Errorf("Authorize(%s, owner=%q, write=%v) = %v, want %v",
				tc.id.Login, tc.owner, tc.write, got, tc.want)
		}
	}
}
