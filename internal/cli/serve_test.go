package cli

import (
	"io"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/app"
)

// TestServeLocalAddrDefaults: --local refuses a non-loopback bind, and the
// shipped defaults have an empty host, which is every interface. Without the
// substitution the documented `anygrade serve --local` cannot start at all.
// An address the user typed is never touched, so a public one still reaches
// the gate and is still refused there.
func TestServeLocalAddrDefaults(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantHTTP string
		wantSSH  string
	}{
		{
			name:     "no local mode keeps the shipped defaults",
			args:     nil,
			wantHTTP: ":8080",
			wantSSH:  ":2222",
		},
		{
			name:     "local mode moves both unset addresses to loopback",
			args:     []string{"--local"},
			wantHTTP: "127.0.0.1:8080",
			wantSSH:  "127.0.0.1:2222",
		},
		{
			name:     "an address the user typed is left alone, default-valued or not",
			args:     []string{"--local", "--http-addr", ":8080", "--ssh-addr", "0.0.0.0:2200"},
			wantHTTP: ":8080",
			wantSSH:  "0.0.0.0:2200",
		},
		{
			name:     "only the unset one is substituted",
			args:     []string{"--local", "--http-addr", "192.168.0.5:9000"},
			wantHTTP: "192.168.0.5:9000",
			wantSSH:  "127.0.0.1:2222",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServeFlags()
			f.fs.SetOutput(io.Discard)
			if err := f.parse(tc.args); err != nil {
				t.Fatalf("parse(%v): %v", tc.args, err)
			}
			if *f.httpAddr != tc.wantHTTP {
				t.Errorf("http-addr = %q, want %q", *f.httpAddr, tc.wantHTTP)
			}
			if *f.sshAddr != tc.wantSSH {
				t.Errorf("ssh-addr = %q, want %q", *f.sshAddr, tc.wantSSH)
			}
		})
	}
}

// TestServeTLSFlags: --tls-cert/--tls-key and --behind-proxy reach Options, and
// they default off so an unchanged invocation keeps behaving as before.
func TestServeTLSFlags(t *testing.T) {
	f := newServeFlags()
	f.fs.SetOutput(io.Discard)
	if err := f.parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *f.tlsCert != "" || *f.tlsKey != "" || *f.behindProxy {
		t.Errorf("TLS defaults are not off: cert=%q key=%q behindProxy=%v",
			*f.tlsCert, *f.tlsKey, *f.behindProxy)
	}

	f = newServeFlags()
	f.fs.SetOutput(io.Discard)
	if err := f.parse([]string{"--tls-cert", "c.pem", "--tls-key", "k.pem", "--behind-proxy"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *f.tlsCert != "c.pem" || *f.tlsKey != "k.pem" || !*f.behindProxy {
		t.Errorf("flags not parsed: cert=%q key=%q behindProxy=%v",
			*f.tlsCert, *f.tlsKey, *f.behindProxy)
	}
}

// TestServeRetryFlags: the retry schedule reaches the flag set, and an
// invocation that names none of the three carries the shipped values - the
// whole point of making it settable is that nobody's existing command line
// starts behaving differently.
func TestServeRetryFlags(t *testing.T) {
	f := newServeFlags()
	f.fs.SetOutput(io.Discard)
	if err := f.parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *f.retryBackoff != app.DefaultRetryBackoff ||
		*f.retryBackoffCap != app.DefaultRetryBackoffCap ||
		*f.maxRetries != app.DefaultMaxRetries {
		t.Errorf("the shipped retry schedule changed: backoff=%s cap=%s max=%d",
			*f.retryBackoff, *f.retryBackoffCap, *f.maxRetries)
	}

	f = newServeFlags()
	f.fs.SetOutput(io.Discard)
	args := []string{"--retry-backoff", "200ms", "--retry-backoff-cap", "1s", "--max-retries", "3"}
	if err := f.parse(args); err != nil {
		t.Fatalf("parse(%v): %v", args, err)
	}
	if *f.retryBackoff != 200*time.Millisecond || *f.retryBackoffCap != time.Second || *f.maxRetries != 3 {
		t.Errorf("flags not parsed: backoff=%s cap=%s max=%d",
			*f.retryBackoff, *f.retryBackoffCap, *f.maxRetries)
	}

	// A malformed duration must fail the parse rather than reach the queue as
	// a zero schedule.
	f = newServeFlags()
	f.fs.SetOutput(io.Discard)
	if err := f.parse([]string{"--retry-backoff", "soon"}); err == nil {
		t.Error("--retry-backoff soon parsed successfully")
	}
}
