package app

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/gitserver"
	"github.com/ekalinin/anygrade/internal/web"
)

// isLoopbackAddr reports whether a listen address can only be reached from
// this host. An empty host (":8080") binds every interface and is therefore
// not loopback; unresolved hostnames are treated conservatively as public.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newHTTPServer builds the public listener with a slowloris budget: a client
// gets readHeaderTimeout to finish its request line and headers, and
// idleTimeout of keep-alive between requests.
//
// Deliberately no WriteTimeout and no ReadTimeout. WriteTimeout is wall-clock
// from the start of the response, so it would cut every SSE stream and every
// long `git clone`; ReadTimeout would do the same to a slow but legitimate
// `git push` of a large pack. Header timeouts bound the attack without
// bounding the legitimate long-lived traffic this server exists to carry.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

const (
	readHeaderTimeout = 20 * time.Second
	idleTimeout       = 2 * time.Minute
)

// plaintextWarning is the startup banner for a public listener that carries
// credentials in the clear. The personal access token is both the git basic-auth
// password and the web login, and the session cookie can only be marked Secure
// when the connection actually is - so an operator who has not arranged TLS,
// here or in a proxy, needs to be told in as many words.
func plaintextWarning(opts Options) string {
	if opts.TLSCert != "" || opts.BehindProxy || isLoopbackAddr(opts.HTTPAddr) {
		return ""
	}
	return fmt.Sprintf("anygrade: WARNING: serving %s without TLS.\n"+
		"anygrade: WARNING: access tokens are sent in the clear on every git push and every login.\n"+
		"anygrade: WARNING: pass --tls-cert/--tls-key, or --behind-proxy when a reverse proxy terminates TLS.\n",
		opts.HTTPAddr)
}

// checkTLSOptions rejects half a TLS configuration: one flag without the other
// would otherwise start a plaintext server the operator believes is encrypted.
func checkTLSOptions(cert, key string) error {
	switch {
	case cert != "" && key == "":
		return errors.New("--tls-cert requires --tls-key")
	case key != "" && cert == "":
		return errors.New("--tls-key requires --tls-cert")
	}
	return nil
}

// checkRetryOptions rejects a retry schedule that cannot do what SPEC §13
// promises. The flags carry the shipped values, so nothing here can fire on an
// invocation that did not name them - and a value that was named must not be
// silently replaced by the default the way a non-positive `max_push_size` used
// to be: an operator who wrote 0 believes retries are off, and the queue would
// give them eight.
//
// A cap below the base is the one non-obvious case. It does not shorten the
// schedule, it flattens it: min(base<<n, cap) is the cap from the very first
// retry, so the exponential growth the operator was tuning never happens.
func checkRetryOptions(base, backoffCap time.Duration, maxRetries int) error {
	switch {
	case base <= 0:
		return fmt.Errorf("--retry-backoff must be > 0, got %s", base)
	case backoffCap <= 0:
		return fmt.Errorf("--retry-backoff-cap must be > 0, got %s", backoffCap)
	case backoffCap < base:
		return fmt.Errorf("--retry-backoff-cap (%s) must be >= --retry-backoff (%s)", backoffCap, base)
	case maxRetries <= 0:
		return fmt.Errorf("--max-retries must be > 0, got %d", maxRetries)
	}
	return nil
}

// checkServeSafety enforces SPEC §14 at startup: --local never binds a
// non-loopback address, and a public bind with any task resolved to the
// local runner requires the explicit --allow-local-runner opt-in.
func checkServeSafety(res *config.Resolved, httpAddr, sshAddr string, localMode, allowLocalRunner bool) error {
	public := ""
	switch {
	case !isLoopbackAddr(httpAddr):
		public = httpAddr
	case !isLoopbackAddr(sshAddr):
		public = sshAddr
	}
	if public == "" {
		return nil
	}
	if localMode {
		return fmt.Errorf("serve --local refuses to bind non-loopback address %q", public)
	}
	if allowLocalRunner {
		return nil
	}
	var local []string
	for _, t := range res.Tasks {
		if t.Runner.Type == "local" {
			local = append(local, t.ID)
		}
	}
	if len(local) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to serve on %q: task(s) %s use the local runner, "+
		"which executes untrusted student code on the host; switch them to docker "+
		"or pass --allow-local-runner if you accept the risk",
		public, strings.Join(local, ", "))
}

// webFile adapts a GitSource read to the contract internal/web expects. web
// stays git-free, so the size limit gitserver enforces has to be renamed here,
// in the one place that already knows both sides.
func webFile(data []byte, found bool, err error) ([]byte, bool, error) {
	if errors.Is(err, gitserver.ErrBlobTooLarge) {
		return nil, true, web.ErrFileTooLarge
	}
	return data, found, err
}
