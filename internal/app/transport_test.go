package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHTTPServerTimeouts pins the slowloris budget. The negative half matters
// as much as the positive one: a WriteTimeout would cut SSE streams and long
// clones, a ReadTimeout a slow but legitimate push of a large pack.
func TestHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(":8080", http.NewServeMux())
	if srv.ReadHeaderTimeout == 0 {
		t.Error("no ReadHeaderTimeout: request headers can be dribbled out forever")
	}
	if srv.IdleTimeout == 0 {
		t.Error("no IdleTimeout: idle keep-alive connections are never reclaimed")
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want none: it would kill SSE streams", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want none: it would kill large git pushes", srv.ReadTimeout)
	}
}

// TestCheckTLSOptions: half a TLS configuration must not start a plaintext
// server the operator believes is encrypted.
func TestCheckTLSOptions(t *testing.T) {
	for _, tc := range []struct {
		name, cert, key, wantErr string
	}{
		{name: "neither"},
		{name: "both", cert: "c.pem", key: "k.pem"},
		{name: "cert only", cert: "c.pem", wantErr: "--tls-key"},
		{name: "key only", key: "k.pem", wantErr: "--tls-cert"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTLSOptions(tc.cert, tc.key)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestPlaintextWarning: the personal access token is both the git password and
// the web login, so a public plaintext bind has to say so out loud - and stay
// quiet when TLS is handled here or by a proxy the operator vouched for.
func TestPlaintextWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want bool
	}{
		{name: "public plaintext", opts: Options{HTTPAddr: ":8080"}, want: true},
		{name: "public with tls", opts: Options{HTTPAddr: ":8080", TLSCert: "c.pem", TLSKey: "k.pem"}},
		{name: "public behind proxy", opts: Options{HTTPAddr: ":8080", BehindProxy: true}},
		{name: "loopback plaintext", opts: Options{HTTPAddr: "127.0.0.1:8080"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := plaintextWarning(tc.opts)
			if (got != "") != tc.want {
				t.Fatalf("warning %q, want present=%v", got, tc.want)
			}
			if tc.want && !strings.Contains(got, "--tls-cert") {
				t.Errorf("the warning does not name the way out:\n%s", got)
			}
		})
	}
}

// TestLeaderboardSecretIsStableAndPrivate: aliases must survive a restart of
// one instance (SPEC §10), which means the secret is persisted, not per
// process - and it must not be world-readable next to the database.
func TestLeaderboardSecretIsStableAndPrivate(t *testing.T) {
	dir := t.TempDir()
	first, err := loadLeaderboardSecret(dir)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if len(first) != leaderboardSecretLen {
		t.Fatalf("secret is %d bytes, want %d", len(first), leaderboardSecretLen)
	}
	second, err := loadLeaderboardSecret(dir)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if string(first) != string(second) {
		t.Error("the secret changed across restarts: every alias would be reshuffled")
	}

	st, err := os.Stat(filepath.Join(dir, leaderboardKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode %o, want 600", perm)
	}

	// Two instances must not share a secret, or one board de-anonymizes another.
	other, err := loadLeaderboardSecret(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if string(other) == string(first) {
		t.Error("two data dirs produced the same secret")
	}
}
