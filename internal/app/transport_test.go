package app

import (
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/oidc"
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

// TestCheckRetryOptions: the shipped schedule passes untouched - an invocation
// that names none of the flags must keep working - and every value that would
// quietly not do what it says is refused rather than defaulted.
func TestCheckRetryOptions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		base       time.Duration
		backoffCap time.Duration
		maxRetries int
		wantErr    string
	}{
		{
			name: "the shipped schedule", base: DefaultRetryBackoff,
			backoffCap: DefaultRetryBackoffCap, maxRetries: DefaultMaxRetries,
		},
		{
			name: "a sub-second schedule is legitimate", base: 200 * time.Millisecond,
			backoffCap: time.Second, maxRetries: 3,
		},
		{
			name: "base equal to the cap is a flat schedule, not a broken one",
			base: time.Minute, backoffCap: time.Minute, maxRetries: 4,
		},
		{
			name: "negative base", base: -time.Second,
			backoffCap: time.Minute, maxRetries: 8, wantErr: "--retry-backoff must be > 0",
		},
		{
			name: "zero base", base: 0,
			backoffCap: time.Minute, maxRetries: 8, wantErr: "--retry-backoff must be > 0",
		},
		{
			name: "zero cap", base: time.Second,
			backoffCap: 0, maxRetries: 8, wantErr: "--retry-backoff-cap must be > 0",
		},
		{
			name: "cap below base", base: time.Minute,
			backoffCap: time.Second, maxRetries: 8, wantErr: "must be >= --retry-backoff",
		},
		{
			name: "zero budget", base: time.Second,
			backoffCap: time.Minute, maxRetries: 0, wantErr: "--max-retries must be > 0",
		},
		{
			name: "negative budget", base: time.Second,
			backoffCap: time.Minute, maxRetries: -1, wantErr: "--max-retries must be > 0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRetryOptions(tc.base, tc.backoffCap, tc.maxRetries)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestCheckWorkerOptions: a non-positive --workers must be refused at startup
// for the same reason a non-positive retry flag is - the queue would
// otherwise silently clamp it to its own default of 4, and an operator who
// wrote 0 believes they turned checking off.
func TestCheckWorkerOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		workers int
		wantErr string
	}{
		{name: "the shipped default", workers: 4},
		{name: "one worker is legitimate", workers: 1},
		{name: "zero", workers: 0, wantErr: "--workers must be > 0"},
		{name: "negative", workers: -3, wantErr: "--workers must be > 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkWorkerOptions(tc.workers)
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

// TestBaseURL: the derived submission-link prefix must name a scheme the
// listener actually answers (SPEC §11), so --tls-cert has to make it https.
// --behind-proxy must not: the public origin is then the proxy's, not this
// listener's, and only --base-url can name it.
func TestBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "explicit base URL wins over everything",
			opts: Options{BaseURL: "https://grades.example.edu", HTTPAddr: ":8080"},
			want: "https://grades.example.edu",
		},
		{
			name: "plaintext listener",
			opts: Options{HTTPAddr: ":8080"},
			want: "http://localhost:8080",
		},
		{
			name: "tls listener is https",
			opts: Options{HTTPAddr: ":8080", TLSCert: "c.pem", TLSKey: "k.pem"},
			want: "https://localhost:8080",
		},
		{
			name: "tls listener keeps a named host",
			opts: Options{HTTPAddr: "grades.example.edu:8443", TLSCert: "c.pem", TLSKey: "k.pem"},
			want: "https://grades.example.edu:8443",
		},
		{
			name: "wildcard v6 bind becomes localhost",
			opts: Options{HTTPAddr: "[::]:8080"},
			want: "http://localhost:8080",
		},
		{
			name: "behind a proxy the local listener is still plaintext",
			opts: Options{HTTPAddr: "127.0.0.1:8080", BehindProxy: true},
			want: "http://127.0.0.1:8080",
		},
		{
			name: "unparsable address yields no links",
			opts: Options{HTTPAddr: "8080"},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := baseURL(tc.opts); got != tc.want {
				t.Errorf("baseURL(%+v) = %q, want %q", tc.opts, got, tc.want)
			}
		})
	}
}

// TestOidcProviderRequiresExplicitBaseURL: the redirect URI is registered at
// the provider ahead of time, so oidcProvider must see the operator's own
// --base-url and nothing baseURL derives from --http-addr - a derived value is
// always non-empty (e.g. "http://localhost:8080"), which made FromEnv's own
// "public base URL is unknown" guard unreachable.
func TestOidcProviderRequiresExplicitBaseURL(t *testing.T) {
	t.Setenv(oidc.EnvIssuer, "https://idp.example.org")
	t.Setenv(oidc.EnvClientID, "anygrade")

	opts := Options{HTTPAddr: ":8080"} // baseURL(opts) would derive http://localhost:8080
	if _, err := oidcProvider(t.Context(), opts, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "--base-url") {
		t.Fatalf("oidcProvider with no --base-url = %v, want it to require the flag explicitly", err)
	}
}

// TestLoadLeaderboardSecretRejectsWrongLength: the generator always writes
// leaderboardSecretLen bytes; a file that decodes to any other length - a
// truncated backup, a key typed by hand - must not silently become a weaker
// (or wider) HMAC key.
func TestLoadLeaderboardSecretRejectsWrongLength(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, leaderboardKeyFile)
	short := "ab" // decodes to one byte, not leaderboardSecretLen
	if err := os.WriteFile(path, []byte(short+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLeaderboardSecret(dir); err == nil || !strings.Contains(err.Error(), "remove it to regenerate") {
		t.Fatalf("loadLeaderboardSecret with a 1-byte key = %v, want the regenerate instruction", err)
	}
}

// TestLoadLeaderboardSecretTightensPermissions: a key restored from a backup
// under a loose umask must end up 0600 on load, exactly like the data dir
// itself.
func TestLoadLeaderboardSecretTightensPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, leaderboardKeyFile)
	secret := make([]byte, leaderboardSecretLen)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(secret)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLeaderboardSecret(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions after load = %#o, want 0600", perm)
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
