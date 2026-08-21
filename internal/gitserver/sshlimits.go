package gitserver

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ekalinin/anygrade/internal/ratelimit"
)

// SSH transport budgets (SPEC §14).
//
// These are deliberately not credential budgets. SSH authenticates a public-key
// fingerprint, which nobody guesses, and a client legitimately offers every key
// in its agent until one matches - so counting "failed" keys the way the web
// login and git basic auth count failed tokens would throttle honest students
// and buy nothing against an attacker. x/crypto already caps the offers at six
// per connection, and each one is a single indexed lookup.
//
// What is genuinely unbounded is churn: every accepted connection costs a
// goroutine, a key exchange and a store lookup, and nothing capped how many an
// unauthenticated peer could open or how long it could hold them. That is a
// resource question, so the budgets below are on concurrency and time.
const (
	// defaultMaxHandshakes bounds the connections sitting in the handshake at
	// once, across all peers. Together with defaultHandshakeTimeout it is the
	// ceiling on the server's whole unauthenticated footprint: past the
	// handshake a connection has proved a registered key, so it belongs to a
	// student doing real work and is no longer counted.
	defaultMaxHandshakes = 512
	// defaultMaxHandshakesPerIP stops one peer from taking the global budget on
	// its own. A handshake is milliseconds of work, so a whole lab section
	// pushing in the same second - thirty students behind one NAT address at a
	// deadline - stays well under it, while an attacker needs eight distinct
	// addresses before it can even reach the global ceiling.
	defaultMaxHandshakesPerIP = 64
	// defaultHandshakeTimeout matches sshd's own LoginGraceTime default, and for
	// the same reason: the client may prompt for the key's passphrase inside the
	// handshake, right after the server accepts the offered key, so a tight
	// deadline here would cut off a student who is typing.
	defaultHandshakeTimeout = 2 * time.Minute
	// defaultIdleTimeout reclaims an established connection whose peer went away
	// without closing it. It is sized above the longest silence a real operation
	// produces - receive-pack unpacking a pack up to max_push_size and running
	// the intake hook - because there is deliberately no absolute connection
	// deadline: that would cut a legitimate slow clone or push mid-transfer, the
	// same reason the HTTP listener sets no read or write timeout (§14).
	defaultIdleTimeout = 10 * time.Minute
)

// connGate counts the connections currently inside the SSH handshake, in total
// and per client address.
type connGate struct {
	max, perIP int

	mu    sync.Mutex
	total int
	byIP  map[string]int
}

func newConnGate(max, perIP int) *connGate {
	return &connGate{max: max, perIP: perIP, byIP: map[string]int{}}
}

// acquire takes a handshake slot for the peer at addr. ok=false means the peer
// is over budget and the connection must be dropped; release gives the slot
// back and is safe to call more than once only through handshakeConn, which
// guards it with a sync.Once.
func (g *connGate) acquire(addr string) (release func(), ok bool) {
	// No forwarded header applies: SSH is spoken straight to the listener, so
	// the peer address is the only address there is.
	host := ratelimit.ClientAddr(addr, "", false)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.total >= g.max || g.byIP[host] >= g.perIP {
		return nil, false
	}
	g.total++
	g.byIP[host]++
	return func() { g.release(host) }, true
}

func (g *connGate) release(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.total--
	// Drop the entry at zero: the map is keyed by addresses the peer chooses,
	// so it must not grow with the number of peers ever seen.
	if n := g.byIP[host] - 1; n > 0 {
		g.byIP[host] = n
	} else {
		delete(g.byIP, host)
	}
}

// handshakeConn is an accepted connection for as long as it is unauthenticated.
// It owns two things until the handshake ends: the gate slot, and a deadline
// the peer cannot push out.
//
// Clamping SetDeadline is what makes that deadline stick. The ssh library wraps
// this conn again and stamps its own idle deadline on every read and write, so
// a deadline merely set at accept time would be overwritten by the very first
// read of the client's version string.
type handshakeConn struct {
	net.Conn
	deadline time.Time
	release  func()

	done atomic.Bool
	once sync.Once
}

func (c *handshakeConn) SetDeadline(t time.Time) error {
	if !c.done.Load() && (t.IsZero() || t.After(c.deadline)) {
		t = c.deadline
	}
	return c.Conn.SetDeadline(t)
}

// established marks the end of the handshake: the slot goes back to the gate
// and the peer stops being held to the handshake deadline, so a long clone or
// push is bounded only by the idle timeout.
func (c *handshakeConn) established() {
	if c.done.Swap(true) {
		return
	}
	c.free()
	// The library restamps the idle deadline on the next read or write; until
	// then the connection is better off with none than with a stale one.
	_ = c.Conn.SetDeadline(time.Time{})
}

// Close is the backstop that makes the slot accounting leak-proof: the library
// closes every connection it accepted, on every path, handshake failures and
// dropped peers included.
func (c *handshakeConn) Close() error {
	c.free()
	return c.Conn.Close()
}

func (c *handshakeConn) free() { c.once.Do(c.release) }
