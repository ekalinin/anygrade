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

	"github.com/gliderlabs/ssh"
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
	addr     net.Addr
	deadline time.Time
	closed   bool
}

func (c *deadlineConn) SetDeadline(t time.Time) error { c.deadline = t; return nil }
func (c *deadlineConn) Close() error                  { c.closed = true; return nil }
func (c *deadlineConn) RemoteAddr() net.Addr          { return c.addr }

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

// fakeSSHContext is the connection-scoped context the library builds per
// connection, reduced to the two things the budget code uses: the value store
// and the lock.
type fakeSSHContext struct {
	context.Context
	sync.Mutex
	values map[any]any
}

func newFakeSSHContext(t *testing.T) *fakeSSHContext {
	return &fakeSSHContext{Context: t.Context(), values: map[any]any{}}
}

func (c *fakeSSHContext) SetValue(key, value any) { c.values[key] = value }
func (c *fakeSSHContext) Value(key any) any {
	if v, ok := c.values[key]; ok {
		return v
	}
	return c.Context.Value(key)
}
func (c *fakeSSHContext) User() string          { return "" }
func (c *fakeSSHContext) SessionID() string     { return "" }
func (c *fakeSSHContext) ClientVersion() string { return "" }
func (c *fakeSSHContext) ServerVersion() string { return "" }
func (c *fakeSSHContext) RemoteAddr() net.Addr  { return nil }
func (c *fakeSSHContext) LocalAddr() net.Addr   { return nil }
func (c *fakeSSHContext) Permissions() *ssh.Permissions {
	return &ssh.Permissions{Permissions: &gossh.Permissions{}}
}

// TestSSHAuthReleasesHandshakeSlot: the budget is on unauthenticated churn, so
// a registered key has to hand its slot back before the transfer starts. If the
// slot were held for the whole connection, a per-IP cap tight enough to be a
// defence would also cap concurrent pushes from one classroom NAT address.
func TestSSHAuthReleasesHandshakeSlot(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ensureHostKey(filepath.Join(t.TempDir(), "hostkey"))
	if err != nil {
		t.Fatal(err)
	}
	srv := (&SSHServer{
		Auth: fakeAuth{ids: map[string]Identity{
			gossh.FingerprintSHA256(key): {UserID: 1, Login: "alice", Role: "student"},
		}},
		MaxHandshakesPerIP: 1,
	}).newServer(signer)

	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 4242}
	accept := func() net.Conn {
		return srv.ConnCallback(newFakeSSHContext(t), &deadlineConn{addr: addr})
	}

	ctx := newFakeSSHContext(t)
	if got := srv.ConnCallback(ctx, &deadlineConn{addr: addr}); got == nil {
		t.Fatal("the first connection from a fresh address was refused")
	}
	if accept() != nil {
		t.Fatal("the per-IP handshake cap of 1 did not hold")
	}

	// An unregistered key is not an authentication: the slot stays taken, and
	// nothing else about the peer changes.
	other, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := gossh.NewPublicKey(other)
	if err != nil {
		t.Fatal(err)
	}
	if srv.PublicKeyHandler(ctx, unknown) {
		t.Fatal("an unregistered fingerprint authenticated")
	}
	if accept() != nil {
		t.Error("a rejected key released the handshake slot")
	}

	if !srv.PublicKeyHandler(ctx, key) {
		t.Fatal("the registered fingerprint did not authenticate")
	}
	if accept() == nil {
		t.Error("a registered key did not release its handshake slot; the cap would apply to pushes, not to churn")
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
