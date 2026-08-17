package gitserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// identityKey stores the authenticated Identity in the ssh.Context between
// the publickey callback and the session handler.
const identityKey = "anygrade-identity"

// SSHServer serves git over SSH (SPEC §7): publickey auth by registered
// fingerprint, exec-only (`git-upload-pack`/`git-receive-pack`), no shell,
// no pty. A single system identity; users are told apart by key.
type SSHServer struct {
	Repos   *RepoManager
	Auth    Authenticator
	Socket  string // intake unix socket, injected into receive-pack hooks
	HostKey string // host key path; generated on first use
	// Local, when non-nil, accepts any key/none and acts as this identity
	// (serve --local; loopback bind is enforced by the caller).
	Local *Identity
}

// ListenAndServe blocks until ctx is canceled.
func (s *SSHServer) ListenAndServe(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, l)
}

// Serve accepts sessions on l until ctx is canceled (l is closed either way).
func (s *SSHServer) Serve(ctx context.Context, l net.Listener) error {
	signer, err := ensureHostKey(s.HostKey)
	if err != nil {
		l.Close()
		return err
	}
	srv := &ssh.Server{Handler: s.handle}
	srv.AddHostKey(signer)
	if s.Local == nil {
		srv.PublicKeyHandler = func(ctx ssh.Context, key ssh.PublicKey) bool {
			id, ok, err := s.Auth.ByFingerprint(ctx, gossh.FingerprintSHA256(key))
			if err != nil || !ok {
				return false
			}
			ctx.SetValue(identityKey, id)
			return true
		}
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
		case <-done:
		}
	}()
	err = srv.Serve(l)
	close(done)
	if ctx.Err() != nil || errors.Is(err, ssh.ErrServerClosed) {
		return nil
	}
	return err
}

// handle runs one exec session: parse the git command, authorize, pipe stdio.
func (s *SSHServer) handle(sess ssh.Session) {
	if _, _, isPty := sess.Pty(); isPty {
		fmt.Fprintln(sess.Stderr(), "anygrade: interactive sessions are not allowed")
		_ = sess.Exit(1)
		return
	}
	id, ok := s.identity(sess)
	if !ok {
		fmt.Fprintln(sess.Stderr(), "anygrade: unauthenticated session")
		_ = sess.Exit(1)
		return
	}

	svc, owner, err := parseGitCommand(sess.Command())
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "anygrade: %v\n", err)
		_ = sess.Exit(1)
		return
	}
	write := svc == "git-receive-pack"
	if !Authorize(id, owner, write) {
		fmt.Fprintln(sess.Stderr(), "anygrade: access denied")
		_ = sess.Exit(1)
		return
	}
	dir, err := s.repoDir(sess.Context(), id, owner)
	if err != nil {
		fmt.Fprintln(sess.Stderr(), "anygrade: repository not found")
		_ = sess.Exit(1)
		return
	}

	env := append(os.Environ(), hookEnv(s.Socket, owner, id)...)
	// The only client env honored is GIT_PROTOCOL (protocol v2 negotiation).
	for _, kv := range sess.Environ() {
		if strings.HasPrefix(kv, "GIT_PROTOCOL=") {
			env = append(env, kv)
		}
	}

	// The cap is ours to enforce so the rejection is ours to word (SPEC §13).
	var guard *oversizeReader
	stdin := io.Reader(sess)
	limit := s.Repos.MaxInputSize()
	if write {
		guard = newOversizeReader(sess, limit)
		stdin = guard
	}

	cmd := exec.CommandContext(sess.Context(), "git", gitSub(svc), dir)
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = sess
	cmd.Stderr = sess.Stderr()
	err = cmd.Run()
	if guard != nil && guard.hit {
		// Unlike HTTP there is nothing to hold back here - receive-pack has
		// been talking to the client since the ref advertisement, and its own
		// report-status ends the side-band stream. The explanation goes on the
		// session's stderr instead, which ssh puts straight on the student's
		// terminal next to git's "unpack-objects abnormal exit".
		fmt.Fprintln(sess.Stderr(), "anygrade: push rejected: "+oversizeMessage(limit))
		_ = sess.Exit(1)
		return
	}
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			_ = sess.Exit(exit.ExitCode())
			return
		}
		_ = sess.Exit(1)
		return
	}
	_ = sess.Exit(0)
}

func (s *SSHServer) identity(sess ssh.Session) (Identity, bool) {
	if s.Local != nil {
		return *s.Local, true
	}
	id, ok := sess.Context().Value(identityKey).(Identity)
	return id, ok
}

// repoDir mirrors HTTPHandler.repoDir: lazy provisioning only for the owner.
func (s *SSHServer) repoDir(ctx context.Context, id Identity, owner string) (string, error) {
	if owner == "" {
		return s.Repos.CourseDir(), nil
	}
	if owner == id.Login {
		return s.Repos.EnsureStudent(ctx, owner)
	}
	dir := s.Repos.StudentDir(owner)
	if _, err := os.Stat(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// parseGitCommand accepts exactly the two git transport commands, in both the
// dashed and spaced spellings, with a single repo-path argument
// ("/alice/course.git" from ssh:// URLs, "alice/course.git" from scp form).
func parseGitCommand(argv []string) (svc, owner string, err error) {
	switch {
	case len(argv) == 2 && (argv[0] == "git-upload-pack" || argv[0] == "git-receive-pack"):
		svc = argv[0]
	case len(argv) == 3 && argv[0] == "git" && (argv[1] == "upload-pack" || argv[1] == "receive-pack"):
		svc = "git-" + argv[1]
	default:
		return "", "", fmt.Errorf("only git-upload-pack/git-receive-pack are allowed, got %q", strings.Join(argv, " "))
	}
	p := strings.Trim(argv[len(argv)-1], "'")
	p = strings.TrimPrefix(p, "/")
	if p == "course.git" {
		return svc, "", nil
	}
	login, found := strings.CutSuffix(p, "/course.git")
	if !found || login == "" || strings.ContainsAny(login, "/\\") {
		return "", "", fmt.Errorf("unknown repository %q", p)
	}
	return svc, login, nil
}

// ensureHostKey loads the persisted host key, generating an ed25519 one on
// first serve.
func ensureHostKey(path string) (gossh.Signer, error) {
	if data, err := os.ReadFile(path); err == nil {
		return gossh.ParsePrivateKey(data)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := gossh.MarshalPrivateKey(priv, "anygrade host key")
	if err != nil {
		return nil, err
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, err
	}
	return gossh.NewSignerFromKey(priv)
}
