package cli

import (
	"io"
	"testing"
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
