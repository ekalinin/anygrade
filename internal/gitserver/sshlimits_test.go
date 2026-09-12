package gitserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// TestConnGateBudgets: the handshake budget has to bound one peer without
// bounding the course. Both ceilings are checked, plus the release path, since
// a slot that never comes back would lock the transport out after N pushes.
func TestConnGateBudgets(t *testing.T) {
	g := newConnGate(3, 2)

	r1, ok := g.acquire("10.0.0.1:1")
	if !ok {
		t.Fatal("first handshake from a fresh address was refused")
	}
	if _, ok := g.acquire("10.0.0.1:2"); !ok {
		t.Fatal("second handshake from the same address was refused under the per-IP cap")
	}
	if _, ok := g.acquire("10.0.0.1:3"); ok {
		t.Error("a third handshake passed a per-IP cap of 2")
	}
	// A different address must not be collateral damage of a noisy neighbour.
	r4, ok := g.acquire("10.0.0.2:1")
	if !ok {
		t.Fatal("another address was refused because the first one was busy")
	}
	if _, ok := g.acquire("10.0.0.3:1"); ok {
		t.Error("a fourth handshake passed a global cap of 3")
	}

	r1()
	r4()
	if _, ok := g.acquire("10.0.0.3:1"); !ok {
		t.Error("released slots did not come back to the global budget")
	}
	if _, ok := g.byIP["10.0.0.2"]; ok {
		t.Error("an address with nothing in flight is still tracked: the map grows with every peer ever seen")
	}
}

// deadlineConn is a net.Conn stub that only records the deadline set on it.
type deadlineConn struct {
	net.Conn
	deadline time.Time
	closed   bool
}

func (c *deadlineConn) SetDeadline(t time.Time) error { c.deadline = t; return nil }
func (c *deadlineConn) Close() error                  { c.closed = true; return nil }

// TestHandshakeConnHoldsTheDeadline: the ssh library restamps its idle deadline
// on every read and write, so a handshake deadline only survives if the wrapper
// clamps it. Once the handshake is over the clamp must get out of the way, or
// every clone longer than the grace period would be cut.
func TestHandshakeConnHoldsTheDeadline(t *testing.T) {
	raw := &deadlineConn{}
	grace := time.Now().Add(time.Minute)
	released := 0
	c := &handshakeConn{Conn: raw, deadline: grace, release: func() { released++ }}

	if err := c.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !raw.deadline.Equal(grace) {
		t.Errorf("deadline %v, want it clamped to the handshake grace %v", raw.deadline, grace)
	}
	// "No deadline" is the value the library uses when nothing is configured;
	// pre-auth it must not mean forever either.
	if err := c.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !raw.deadline.Equal(grace) {
		t.Errorf("deadline %v after a zero deadline, want the handshake grace", raw.deadline)
	}

	c.established()
	if released != 1 {
		t.Fatalf("release called %d times at the end of the handshake, want 1", released)
	}
	later := time.Now().Add(time.Hour)
	if err := c.SetDeadline(later); err != nil {
		t.Fatal(err)
	}
	if !raw.deadline.Equal(later) {
		t.Errorf("deadline %v after authentication, want the idle deadline %v to pass through", raw.deadline, later)
	}

	// Close is the backstop for connections that never authenticate; it must
	// not hand the same slot back a second time.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Errorf("release called %d times after established+Close, want 1: the budget would drift", released)
	}
	if !raw.closed {
		t.Error("the underlying connection was not closed")
	}
}

// TestSSHServerBudgets pins the transport budgets the way the HTTP listener
// pins its own. The negative half matters as much as the positive one: a
// MaxTimeout would cut a large push or a slow clone mid-transfer.
func TestSSHServerBudgets(t *testing.T) {
	signer, err := ensureHostKey(filepath.Join(t.TempDir(), "hostkey"))
	if err != nil {
		t.Fatal(err)
	}
	srv := (&SSHServer{}).newServer(signer)
	if srv.IdleTimeout == 0 {
		t.Error("no IdleTimeout: a connection whose peer went away is never reclaimed")
	}
	if srv.MaxTimeout != 0 {
		t.Errorf("MaxTimeout = %v, want none: it would cut a legitimate long push or clone", srv.MaxTimeout)
	}
	if srv.ConnCallback == nil {
		t.Error("no ConnCallback: unauthenticated connections are unbounded again")
	}
}

// serveSSH starts s on a loopback port and returns its address.
func serveSSH(t *testing.T, s *SSHServer) string {
	t.Helper()
	s.HostKey = filepath.Join(t.TempDir(), "ssh_host_ed25519_key")
	if s.Repos == nil {
		s.Repos = &RepoManager{DataDir: t.TempDir()}
	}
	if s.Auth == nil {
		s.Auth = fakeAuth{}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Serve(ctx, l); err != nil {
			t.Error("ssh serve:", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return l.Addr().String()
}

// readBanner reads the server's version string, which is written from inside
// the handshake - so getting it back proves the connection holds a slot.
func readBanner(t *testing.T, c net.Conn) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := c.Read(buf); err != nil {
		t.Fatalf("no version string from the server: %v", err)
	}
}

// expectClosed fails unless the peer drops the connection soon.
func expectClosed(t *testing.T, c net.Conn, within time.Duration) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	for {
		_, err := c.Read(buf)
		if err == nil {
			continue // the server is still talking; keep reading until it stops
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the connection was still open after %v", within)
		}
		if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			t.Logf("closed with %v", err) // RST instead of FIN is still closed
		}
		return
	}
}

// expectRefused fails unless the peer was dropped before the key exchange: a
// connection over budget never gets a version string, while one the server
// admitted answers immediately - the distinction expectClosed cannot make,
// since an admitted connection is closed too, at the handshake deadline.
func expectRefused(t *testing.T, c net.Conn) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	switch n, err := c.Read(buf); {
	case err == nil:
		t.Fatalf("the connection was admitted; the server answered %q", buf[:n])
	case errors.Is(err, os.ErrDeadlineExceeded):
		t.Fatal("the connection was neither answered nor closed")
	}
}

// TestSSHHandshakeTimeout: a peer that connects and then says nothing must not
// hold a goroutine and a slot for as long as it pleases.
func TestSSHHandshakeTimeout(t *testing.T) {
	addr := serveSSH(t, &SSHServer{HandshakeTimeout: 200 * time.Millisecond})

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readBanner(t, c)
	expectClosed(t, c, 5*time.Second)
}

// TestSSHHandshakeBudget: one peer cannot open unauthenticated connections
// without limit. The refusal is a close before the key exchange, so it costs
// the server nothing and charges no credential budget.
func TestSSHHandshakeBudget(t *testing.T) {
	addr := serveSSH(t, &SSHServer{
		MaxHandshakesPerIP: 1,
		HandshakeTimeout:   10 * time.Second,
	})

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	readBanner(t, first) // the budget is now spent

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	expectClosed(t, second, 5*time.Second)

	// Closing the first one gives the slot back: the cap is on connections in
	// flight, not on connections ever made, so a student's next push works.
	first.Close()
	var third net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		third, err = net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if err := third.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		if _, err := third.Read(buf); err == nil {
			third.Close()
			return
		}
		third.Close()
		if time.Now().After(deadline) {
			t.Fatal("the slot never came back after the first connection closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// blockingSigner is a client key that stops between the public-key query and
// the signature: x/crypto asks the server whether the key is acceptable, and
// signs only once the test lets it. It embeds gossh.Signer and nothing else, so
// the client cannot route around Sign through the AlgorithmSigner interface.
type blockingSigner struct {
	gossh.Signer
	asked   chan struct{} // closed when the client asks for a signature
	release chan struct{} // closed by the test to let Sign finish
	once    sync.Once
}

func (s *blockingSigner) Sign(r io.Reader, data []byte) (*gossh.Signature, error) {
	s.once.Do(func() { close(s.asked) })
	<-s.release
	return s.Signer.Sign(r, data)
}

// closeNotifyConn reports the moment the peer drops the connection. x/crypto
// reads in a goroutine of its own, so this stays observable while the goroutine
// driving the handshake is parked inside Sign.
type closeNotifyConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *closeNotifyConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil {
		c.once.Do(func() { close(c.closed) })
	}
	return n, err
}

// registeredKey generates a client key and the Authenticator that knows it.
func registeredKey(t *testing.T) (gossh.Signer, Authenticator) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, fakeAuth{ids: map[string]Identity{
		gossh.FingerprintSHA256(key): {UserID: 1, Login: "alice", Role: "student"},
	}}
}

// waitFor fails the test unless ch is closed within the deadline.
func waitFor(t *testing.T, ch <-chan struct{}, within time.Duration, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatal(msg)
	}
}

// TestSSHUnsignedQueryKeepsHandshakeSlot: x/crypto calls the publickey callback
// on the client's query packet - an offer carrying no signature - and caches
// the answer, so the callback proves nothing about the peer. A public key is
// public data: were the slot and the grace period released there, one known
// registered key blob would let an unauthenticated peer park connections past
// both budgets (SPEC §14).
func TestSSHUnsignedQueryKeepsHandshakeSlot(t *testing.T) {
	base, auth := registeredKey(t)
	signer := &blockingSigner{Signer: base, asked: make(chan struct{}), release: make(chan struct{})}
	addr := serveSSH(t, &SSHServer{
		Auth:               auth,
		MaxHandshakesPerIP: 1,
		HandshakeTimeout:   2 * time.Second,
	})

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	watched := &closeNotifyConn{Conn: raw, closed: make(chan struct{})}
	dialed := make(chan struct{})
	// Registered before the unblock below, so it runs after it: the client
	// goroutine returns only once Sign is free to fail on the dead socket.
	t.Cleanup(func() { <-dialed })
	t.Cleanup(func() { close(signer.release) })
	go func() {
		defer close(dialed)
		c, _, _, err := gossh.NewClientConn(watched, addr, &gossh.ClientConfig{
			User:            "git",
			Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
			HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		})
		if err == nil {
			c.Close()
		}
	}()
	waitFor(t, signer.asked, 10*time.Second,
		"the client never got as far as signing: the server did not answer the key query")

	// The peer has offered a public key and nothing else, so it is still
	// unauthenticated and must still hold the one slot this address gets: the
	// second connection is refused before the key exchange.
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	expectRefused(t, second)

	// And it must still be on the handshake clock, not on the idle one.
	waitFor(t, watched.closed, 10*time.Second,
		"the parked connection outlived the handshake deadline")
}

// TestSSHSessionReleasesHandshakeSlot: the slot still has to come back, or the
// per-IP cap would bound real pushes instead of churn. The session is the place
// for it - it opens only after a signature has been verified, and before the
// transfer, which is the long part, starts.
func TestSSHSessionReleasesHandshakeSlot(t *testing.T) {
	signer, auth := registeredKey(t)
	addr := serveSSH(t, &SSHServer{
		Auth:               auth,
		MaxHandshakesPerIP: 1,
		HandshakeTimeout:   10 * time.Second,
	})

	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "git",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// The handler hands the slot back before it looks at the command, so a
	// rejected one is enough here: what matters is that a session was reached.
	if err := sess.Run("whoami"); err == nil {
		t.Fatal("a command other than git-upload-pack/git-receive-pack was accepted")
	}

	// The first connection is still open, but it no longer counts against the
	// handshake budget.
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	readBanner(t, second)
}
