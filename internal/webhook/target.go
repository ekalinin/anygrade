package webhook

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrPrivateTarget is what a refused address looks like to the caller of Do.
// It is a dial error, so it surfaces as a retryable transport failure and the
// delivery gives up after the usual cap - a misconfigured target should be
// noisy in the log, not fatal to anything.
var ErrPrivateTarget = errors.New("webhook target resolves to a loopback, link-local or private address")

// CheckURL applies the target policy shared by `anygrade validate` and the
// deliverer, so a target that validates is one the server will actually post
// to. It is deliberately silent about the value it rejects: the diagnostic is
// echoed back in the teacher's push output, and the target may carry a path
// that is itself a secret.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("must be a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must be an http or https URL")
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	// Any userinfo on an http(s) URL is a credential, and this one would reach
	// the request line of every delivery. The signature is how a receiver
	// authenticates anygrade; there is nothing a password in the URL could add
	// that would not also be readable by every student with a clone.
	if u.User != nil {
		return errors.New("must not embed credentials; the webhook is authenticated by its " +
			"signature, and the signing secret comes from the environment (" + SecretEnv + ")")
	}
	return nil
}

// PrivateHost reports whether the target names an address the deliverer
// refuses by default. Only a literal host (or "localhost") is judged: a name is
// not resolved, because validate usually runs on the course author's machine,
// where the answer says nothing about what the grading server would resolve.
// Enforcement happens at dial time either way.
func PrivateHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return !publicAddr(ip)
}

// newClient builds the delivery client. Redirects are not followed at all: a
// redirect is how a target that passes the address policy hops to one that
// would not, and the final URL belongs in course.yaml anyway. A 3xx is
// therefore returned as-is and counts as a failed delivery, which puts the
// status code in the log where the teacher's mistake is visible.
func newClient(allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		d.Control = controlAddr
	}
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			// Proxy is deliberately nil rather than ProxyFromEnvironment: a
			// proxy would be dialed instead of the target, and the address
			// policy below would then judge the proxy and never the host the
			// teacher wrote. An egress proxy has to be the target itself.
			DialContext:           d.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			ExpectContinueTimeout: time.Second,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       30 * time.Second,
		},
		// No client Timeout: the per-attempt deadline lives on the request
		// context, which is also what a shutdown cancels.
	}
}

// controlAddr runs after the name has been resolved and before the connection
// is made, which is the one place the check is not a TOCTOU: a DNS answer that
// points at a public address the first time and at 127.0.0.1 the second time is
// judged here on each attempt, on the address actually about to be dialed.
//
// The target comes from course.yaml, so a teacher can move it with a push while
// the machine may belong to somebody else. That is the whole reason to refuse
// the internal ranges by default: a POST the teacher cannot read the reply to
// is still a POST to the cloud metadata endpoint or an admin API on the same
// host. An operator who wants a local relay says so with AllowPrivateEnv.
func controlAddr(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	// Control is always handed a literal address, never a name.
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("webhook target: unexpected dial address %q", address)
	}
	if !publicAddr(ip) {
		return ErrPrivateTarget
	}
	return nil
}

// publicAddr reports whether ip is routable on the public internet, judging any
// IPv4 address an IPv6 one carries as well. Unmap folds ::ffff:a.b.c.d; the
// deprecated ::a.b.c.d and RFC 6052's 64:ff9b::a.b.c.d are handled below, so
// neither is a second way to write 127.0.0.1.
func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !routable(ip) {
		return false
	}
	if v4, ok := embedded4(ip); ok {
		return routable(v4)
	}
	return true
}

// routable judges one address. IsPrivate covers RFC 1918 and RFC 4193 (IPv6
// unique-local); link-local unicast covers 169.254.0.0/16, which is where the
// cloud metadata endpoints live.
func routable(ip netip.Addr) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	// 100.64.0.0/10 is RFC 6598 shared address space: not private by netip's
	// definition, not reachable from the public internet either, and in
	// practice where a tailnet and several container platforms put hosts.
	if ip.Is4() {
		b := ip.As4()
		return !(b[0] == 100 && b[1] >= 64 && b[1] <= 127)
	}
	return true
}

// embedded4 returns the IPv4 address an IPv6 one carries in the deprecated
// ::a.b.c.d form or under RFC 6052's well-known 64:ff9b::/96 NAT64 prefix.
func embedded4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.Is6() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	v4 := netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	if b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b &&
		b[4]|b[5]|b[6]|b[7]|b[8]|b[9]|b[10]|b[11] == 0 {
		return v4, true
	}
	for _, x := range b[:12] {
		if x != 0 {
			return netip.Addr{}, false
		}
	}
	return v4, true
}
