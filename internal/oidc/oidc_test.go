package oidc

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/oidc/oidctest"
)

// newProvider starts a fake issuer and points a relying party at it.
func newProvider(t *testing.T, mutate func(*Config)) (*Provider, *oidctest.Issuer) {
	t.Helper()
	is, err := oidctest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(is.Close)
	cfg := Config{
		Issuer:       is.URL(),
		ClientID:     oidctest.ClientID,
		ClientSecret: oidctest.ClientSecret,
		RedirectURL:  "https://grade.example.org" + CallbackPath,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, is
}

// TestExchangeHappyPath drives the whole flow the way a browser would: the
// authorization URL carries the PKCE challenge, the code redeems once, and the
// ID token that comes back yields the identity the account is matched by.
func TestExchangeHappyPath(t *testing.T) {
	p, is := newProvider(t, nil)
	flow, err := NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	authURL, err := p.AuthCodeURL(t.Context(), flow)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	q, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	got := q.Query()
	if got.Get("code_challenge_method") != "S256" || got.Get("code_challenge") == "" {
		t.Errorf("no PKCE challenge on the authorization url: %v", got)
	}
	if got.Get("state") != flow.State || got.Get("nonce") != flow.Nonce {
		t.Errorf("state/nonce not carried: %v", got)
	}
	if got.Get("code_challenge") == flow.Verifier {
		t.Error("the raw verifier was sent as the challenge")
	}

	code, _, err := is.AuthCode(authURL, oidctest.Token{
		Subject: "sub-42", Login: "Alice", Email: "alice@uni.example", Name: "Alice A",
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := p.Exchange(t.Context(), code, flow)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Subject != "sub-42" || id.Issuer != is.URL() {
		t.Errorf("identity = %+v, want subject sub-42 at %s", id, is.URL())
	}
	// Logins are lowercase everywhere else in the system; the claim is folded
	// to match rather than silently failing the lookup.
	if id.Login != "alice" {
		t.Errorf("login = %q, want %q", id.Login, "alice")
	}

	// The code is single use at the provider, so a replay gets nothing.
	if _, err := p.Exchange(t.Context(), code, flow); err == nil {
		t.Error("a replayed authorization code was accepted")
	}
	if is.Redeemed() != 1 {
		t.Errorf("codes redeemed = %d, want 1", is.Redeemed())
	}
}

// TestIDTokenRefusals is the core of the feature: every way an ID token can be
// wrong has to leave the caller with no identity. Each of these accepted would
// be a way to sign in as somebody else.
func TestIDTokenRefusals(t *testing.T) {
	tests := []struct {
		name string
		tok  func(nonce string) oidctest.Token
		want string // substring of the error, so the check is not just "some error"
	}{
		{
			// A token minted by a different issuer for the same client id.
			name: "wrong issuer",
			tok:  func(n string) oidctest.Token { return oidctest.Token{Nonce: n, Issuer: "https://evil.example"} },
			want: "iss",
		},
		{
			// A real token from the right issuer, minted for another relying
			// party. Without the aud check, every client of a shared university
			// IdP could log people into this one.
			name: "wrong audience",
			tok:  func(n string) oidctest.Token { return oidctest.Token{Nonce: n, Audience: []string{"other-app"}} },
			want: "aud",
		},
		{
			// Several audiences and an azp naming somebody else: the token is
			// for that party, not for us.
			name: "azp names another client",
			tok: func(n string) oidctest.Token {
				return oidctest.Token{Nonce: n, Audience: []string{oidctest.ClientID, "other-app"}, AZP: "other-app"}
			},
			want: "azp",
		},
		{
			name: "expired",
			tok: func(n string) oidctest.Token {
				return oidctest.Token{Nonce: n, Expiry: time.Now().Add(-time.Hour)}
			},
			want: "expired",
		},
		{
			name: "issued in the future",
			tok: func(n string) oidctest.Token {
				return oidctest.Token{Nonce: n, IssuedAt: time.Now().Add(time.Hour)}
			},
			want: "iat",
		},
		{
			name: "bad signature",
			tok:  func(n string) oidctest.Token { return oidctest.Token{Nonce: n, BadSignature: true} },
			want: "signature",
		},
		{
			// The classic JWT forgery: a well-formed token with alg none and no
			// signature at all. The algorithm allowlist has no entry for it, so
			// it never reaches a verification step that could pass.
			name: "alg none",
			tok:  func(n string) oidctest.Token { return oidctest.Token{Nonce: n, Alg: "none"} },
			want: "not accepted",
		},
		{
			// A symmetric algorithm: the "key" would be the client secret, which
			// is exactly what an attacker who read the config already has.
			name: "hmac algorithm",
			tok:  func(n string) oidctest.Token { return oidctest.Token{Nonce: n, Alg: "HS256"} },
			want: "not accepted",
		},
		{
			// A perfectly valid token from an earlier login of the same student.
			name: "nonce from another login",
			tok:  func(string) oidctest.Token { return oidctest.Token{Nonce: "some-other-nonce"} },
			want: "nonce",
		},
		{
			name: "no nonce at all",
			tok:  func(string) oidctest.Token { return oidctest.Token{} },
			want: "nonce",
		},
		{
			name: "no login claim",
			tok:  func(n string) oidctest.Token { return oidctest.Token{Nonce: n, Login: ""} },
			want: "preferred_username",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, is := newProvider(t, nil)
			const nonce = "the-nonce"
			tok := tc.tok(nonce)
			if tok.Login == "" && tc.name != "no login claim" {
				tok.Login = "alice"
			}
			id, err := p.verify(t.Context(), is.Mint(tok), nonce)
			if err == nil {
				t.Fatalf("accepted a bad id token: %+v", id)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestIDTokenAcceptsMultipleAudiencesWithMatchingAZP: several audiences are
// legal as long as azp says the token is for us.
func TestIDTokenAcceptsMultipleAudiencesWithMatchingAZP(t *testing.T) {
	p, is := newProvider(t, nil)
	id, err := p.verify(t.Context(), is.Mint(oidctest.Token{
		Nonce: "n", Login: "alice", Audience: []string{oidctest.ClientID, "other"}, AZP: oidctest.ClientID,
	}), "n")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.Login != "alice" {
		t.Errorf("login = %q", id.Login)
	}
}

// TestEmailClaimNeedsVerification: matching accounts on `email` is only safe
// when the issuer says it verified the address. A provider that lets a user
// type any address would otherwise hand out a claim on any account.
func TestEmailClaimNeedsVerification(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name     string
		verified *bool
		ok       bool
	}{
		{name: "verified", verified: &yes, ok: true},
		{name: "explicitly unverified", verified: &no},
		{name: "claim absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, is := newProvider(t, func(c *Config) { c.LoginClaim = "email" })
			_, err := p.verify(t.Context(), is.Mint(oidctest.Token{
				Nonce: "n", Email: "alice@uni.example", EmailVerified: tc.verified,
			}), "n")
			if tc.ok && err != nil {
				t.Fatalf("verify: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("an unverified email was accepted as an identifier")
			}
		})
	}
}

// TestDiscoveryIssuerMustMatch: the document names the issuer its tokens will
// carry. If that is not the string we compare `iss` against, every later check
// is against the wrong value.
func TestDiscoveryIssuerMustMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":"https://somebody.else","authorization_endpoint":"https://a","token_endpoint":"https://t","jwks_uri":"https://j"}`))
	}))
	t.Cleanup(srv.Close)
	p, err := New(Config{
		Issuer: srv.URL, ClientID: "id", RedirectURL: "https://grade.example.org" + CallbackPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Probe(t.Context()); err == nil {
		t.Fatal("a discovery document for another issuer was accepted")
	}
}

// TestNewRejectsBadConfig: the settings that can only be wrong are refused when
// the provider is built, not when the first student tries to sign in.
func TestNewRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no issuer", Config{ClientID: "id", RedirectURL: "https://g" + CallbackPath}},
		{"no client id", Config{Issuer: "https://idp", RedirectURL: "https://g" + CallbackPath}},
		{"plaintext issuer", Config{Issuer: "http://idp.example", ClientID: "id", RedirectURL: "https://g" + CallbackPath}},
		{"relative issuer", Config{Issuer: "idp.example", ClientID: "id", RedirectURL: "https://g" + CallbackPath}},
		{"redirect is not the callback", Config{Issuer: "https://idp", ClientID: "id", RedirectURL: "https://g/elsewhere"}},
		{"relative redirect", Config{Issuer: "https://idp", ClientID: "id", RedirectURL: CallbackPath}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("accepted a configuration that cannot work")
			}
		})
	}
	// Loopback over plain http is the one exception, and only so a test issuer
	// (and a laptop trying the feature out) can exist at all.
	if _, err := New(Config{
		Issuer: "http://127.0.0.1:9999", ClientID: "id", RedirectURL: "http://localhost:8080" + CallbackPath,
	}); err != nil {
		t.Errorf("a loopback issuer was refused: %v", err)
	}
}

// TestFromEnvDisabledByDefault: with no issuer in the environment the whole
// feature is off, which is what makes it optional.
func TestFromEnvDisabledByDefault(t *testing.T) {
	t.Setenv(EnvIssuer, "")
	cfg, enabled, err := FromEnv("https://grade.example.org")
	if err != nil || enabled {
		t.Fatalf("FromEnv = %+v, enabled=%v, err=%v; want disabled", cfg, enabled, err)
	}
}

// TestFromEnvHalfConfigured: an issuer without a client id is an operator
// mistake, and a silent fallback would leave a login button that cannot work.
func TestFromEnvHalfConfigured(t *testing.T) {
	t.Setenv(EnvIssuer, "https://idp.example")
	t.Setenv(EnvClientID, "")
	if _, _, err := FromEnv("https://grade.example.org"); err == nil {
		t.Fatal("a half-configured provider was accepted")
	}
	t.Setenv(EnvClientID, "id")
	if _, _, err := FromEnv(""); err == nil {
		t.Fatal("a provider with no public base URL was accepted: the redirect uri would be wrong")
	}
}

// TestFromEnvDefaults: the redirect URI is derived from the public base URL and
// the claim defaults to the one an IdP usually carries a short username in.
func TestFromEnvDefaults(t *testing.T) {
	t.Setenv(EnvIssuer, "https://idp.example/realms/uni/")
	t.Setenv(EnvClientID, "anygrade")
	t.Setenv(EnvClientSecret, "shh")
	cfg, enabled, err := FromEnv("https://grade.example.org/")
	if err != nil || !enabled {
		t.Fatalf("FromEnv: enabled=%v err=%v", enabled, err)
	}
	if cfg.RedirectURL != "https://grade.example.org"+CallbackPath {
		t.Errorf("redirect url = %q", cfg.RedirectURL)
	}
	// A trailing slash is part of the issuer identifier for some providers, so
	// it survives verbatim: `iss` is compared byte for byte.
	if cfg.Issuer != "https://idp.example/realms/uni/" {
		t.Errorf("issuer = %q, want it kept verbatim", cfg.Issuer)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.LoginClaim != DefaultLoginClaim {
		t.Errorf("login claim = %q, want %q", p.cfg.LoginClaim, DefaultLoginClaim)
	}
	if p.Name() != "idp.example" {
		t.Errorf("provider name = %q, want the issuer host", p.Name())
	}
}
