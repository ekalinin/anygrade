package gitserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

// newSSHFixture starts an SSH server on a random loopback port and returns
// the port plus a GIT_SSH_COMMAND that authenticates as a freshly generated
// client key registered for "alice".
func newSSHFixture(t *testing.T) (port int, sshCmd string, rm *RepoManager) {
	t.Helper()
	requireGit(t)
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not found")
	}

	keyDir := t.TempDir()
	keyPath := filepath.Join(keyDir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pubData, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := gossh.ParseAuthorizedKey(pubData)
	if err != nil {
		t.Fatal(err)
	}
	fp := gossh.FingerprintSHA256(pub)

	src := newSrcRepo(t)
	rm = &RepoManager{DataDir: t.TempDir(), HookBin: "/usr/bin/true"}
	if err := rm.EnsureCourse(t.Context(), src); err != nil {
		t.Fatal(err)
	}

	srv := &SSHServer{
		Repos: rm,
		Auth: fakeAuth{
			ids: map[string]Identity{fp: {UserID: 1, Login: "alice", Role: "student"}},
		},
		Socket:  filepath.Join(t.TempDir(), "no.sock"),
		HostKey: filepath.Join(rm.DataDir, "ssh_host_ed25519_key"),
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx, l); err != nil {
			t.Error("ssh serve:", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })

	port = l.Addr().(*net.TCPAddr).Port
	sshCmd = fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes", keyPath)
	return port, sshCmd, rm
}

func gitSSH(t *testing.T, sshCmd, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+sshCmd, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestSSHCloneAndPush: the student flow over SSH: keyed auth, lazy repo
// provisioning, push landing in the bare repo.
func TestSSHCloneAndPush(t *testing.T) {
	port, sshCmd, rm := newSSHFixture(t)
	repoURL := fmt.Sprintf("ssh://git@127.0.0.1:%d/alice/course.git", port)

	work := filepath.Join(t.TempDir(), "wc")
	if out, err := gitSSH(t, sshCmd, ".", "clone", repoURL, work); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}

	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("ssh solve\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSrc(t, work, "add", ".")
	runSrc(t, work, "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-m", "solve")
	if out, err := gitSSH(t, sshCmd, work, "push", "origin", "main"); err != nil {
		t.Fatalf("push: %v: %s", err, out)
	}

	head := runSrc(t, work, "rev-parse", "HEAD")
	if bareHead := runSrc(t, rm.StudentDir("alice"), "rev-parse", "main"); bareHead != head {
		t.Fatalf("bare head %s, want %s", bareHead, head)
	}

	// Student write to the course repo is denied over SSH too.
	courseURL := fmt.Sprintf("ssh://git@127.0.0.1:%d/course.git", port)
	runSrc(t, work, "remote", "add", "course", courseURL)
	if out, err := gitSSH(t, sshCmd, work, "push", "course", "main"); err == nil {
		t.Errorf("student push to course repo must fail, got: %s", out)
	}
}

func TestParseGitCommand(t *testing.T) {
	tests := []struct {
		argv  []string
		svc   string
		owner string
		fail  bool
	}{
		{argv: []string{"git-upload-pack", "/alice/course.git"}, svc: "git-upload-pack", owner: "alice"},
		{argv: []string{"git-receive-pack", "alice/course.git"}, svc: "git-receive-pack", owner: "alice"},
		{argv: []string{"git", "upload-pack", "/course.git"}, svc: "git-upload-pack", owner: ""},
		{argv: []string{"git-upload-pack", "'/alice/course.git'"}, svc: "git-upload-pack", owner: "alice"},
		{argv: []string{"scp", "-f", "x"}, fail: true},
		{argv: []string{"git-upload-pack", "/alice/other.git"}, fail: true},
		{argv: []string{"git-upload-pack", "/a/b/course.git"}, fail: true},
		{argv: []string{"git-upload-pack"}, fail: true},
		{argv: nil, fail: true},
	}
	for _, tc := range tests {
		svc, owner, err := parseGitCommand(tc.argv)
		if tc.fail {
			if err == nil {
				t.Errorf("parseGitCommand(%v): expected error, got %q %q", tc.argv, svc, owner)
			}
			continue
		}
		if err != nil || svc != tc.svc || owner != tc.owner {
			t.Errorf("parseGitCommand(%v) = (%q, %q, %v), want (%q, %q)",
				tc.argv, svc, owner, err, tc.svc, tc.owner)
		}
	}
}
