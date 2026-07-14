package app

import (
	"fmt"
	"net"
	"strings"

	"github.com/ekalinin/anygrade/internal/config"
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
