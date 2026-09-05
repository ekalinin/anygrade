// Package oidc is a minimal OpenID Connect relying party: issuer discovery,
// the authorization code flow with PKCE, and ID token verification against the
// issuer's JWKS (SPEC §8).
//
// It is generic on purpose. One correct implementation of the protocol covers
// a university IdP, Google and every Keycloak/Okta/Entra deployment at once,
// where a per-vendor integration would be a new code path - and a new set of
// mistakes - for each. Note that GitHub's OAuth is *not* OpenID Connect (it
// issues no ID token), so it is not covered by this and would need exactly the
// per-vendor integration this package exists to avoid.
//
// The package is a leaf: stdlib only, and it knows nothing about accounts,
// sessions or handlers. Everything it returns is a verified claim about the
// person at the browser; deciding what that means for an account is the web
// layer's job.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CallbackPath is the redirect URI this package expects to be reached at,
// relative to the site's base URL. It is fixed rather than configurable: the
// operator has to register the exact URI at the provider anyway, and one less
// setting is one less way to point the provider at somebody else's host.
const CallbackPath = "/oidc/callback"

// DefaultLoginClaim is the ID token claim matched against an account login
// when the operator names none. It is the claim an IdP is expected to carry a
// short local username in; `email` is the usual alternative, and it is the one
// case where a verification flag is also demanded (see Exchange).
const DefaultLoginClaim = "preferred_username"

// defaultScopes is the smallest set that yields a subject plus a usable login
// claim. `openid` alone gives only `sub`.
var defaultScopes = []string{"openid", "profile", "email"}

const (
	// discoveryTTL bounds how long a discovery document is reused. Endpoints
	// move rarely, but a provider that rotates them must not need a restart.
	discoveryTTL = time.Hour
	// jwksMinRefresh is the shortest interval between two JWKS fetches. An ID
	// token names its signing key, so an unknown kid is the signal to refetch -
	// and, without a floor, also a way for anyone who can reach the callback to
	// make the server hammer the provider.
	jwksMinRefresh = time.Minute
	// httpTimeout bounds every outbound call to the provider. All three are
	// made while a student's browser waits on the callback.
	httpTimeout = 10 * time.Second
	// maxBody caps a provider response. Discovery, JWKS and the token response
	// are all small; the limit is what keeps a hostile or broken endpoint from
	// being read into memory without bound.
	maxBody = 1 << 20
)

// Config is the relying party's whole configuration. It is credentials plus
// endpoints, which is why it comes from the environment and never from
// course.yaml - that file is cloned by every student (SPEC §11).
type Config struct {
	Issuer       string // exactly as the provider spells it in `iss`
	ClientID     string
	ClientSecret string // empty for a public client (PKCE only)
	RedirectURL  string // absolute; must end in CallbackPath
	Scopes       []string
	LoginClaim   string // claim matched against the account login
	ProviderName string // shown on the login button

	// HTTPClient and Now are test seams; the zero value is production.
	HTTPClient *http.Client
	Now        func() time.Time
}

// Provider is a configured relying party. It is safe for concurrent use and
// caches what it discovers.
type Provider struct {
	cfg  Config
	http *http.Client
	now  func() time.Time

	mu       sync.Mutex
	meta     *metadata
	metaAt   time.Time
	keys     []jwk
	keysAt   time.Time
	keysOnce bool
}

// metadata is the subset of the discovery document a relying party needs.
type metadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
}

// New validates the configuration and returns a provider. It performs no I/O:
// a provider that could not be built is an operator error, while a provider
// that cannot reach its issuer right now is an outage, and the two must fail
// at different times (see Probe).
func New(cfg Config) (*Provider, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" {
		return nil, errors.New("oidc: issuer and client id are required")
	}
	if err := checkIssuer(cfg.Issuer); err != nil {
		return nil, err
	}
	if !strings.HasSuffix(cfg.RedirectURL, CallbackPath) {
		return nil, fmt.Errorf("oidc: redirect url %q must end in %s", cfg.RedirectURL, CallbackPath)
	}
	if u, err := url.Parse(cfg.RedirectURL); err != nil || !u.IsAbs() {
		return nil, fmt.Errorf("oidc: redirect url %q is not absolute", cfg.RedirectURL)
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = defaultScopes
	}
	if cfg.LoginClaim == "" {
		cfg.LoginClaim = DefaultLoginClaim
	}
	if cfg.ProviderName == "" {
		cfg.ProviderName = providerName(cfg.Issuer)
	}
	p := &Provider{cfg: cfg, http: cfg.HTTPClient, now: cfg.Now}
	if p.http == nil {
		p.http = &http.Client{Timeout: httpTimeout}
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p, nil
}

// checkIssuer refuses an issuer that is not an absolute https URL. The whole
// trust chain hangs off the discovery document fetched from it, so plaintext
// would put the signing keys themselves in the hands of anyone on the path.
// Loopback is the one exception, and only so a test issuer can exist.
func checkIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("oidc: issuer %q is not an absolute URL", issuer)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopback(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("oidc: issuer %q must be https", issuer)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// providerName is the fallback button label: the issuer's host, which is what
// a student would recognize anyway ("sign in with sso.uni.example").
func providerName(issuer string) string {
	if u, err := url.Parse(issuer); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return issuer
}

// Name is the label the login button carries.
func (p *Provider) Name() string { return p.cfg.ProviderName }

// Probe fetches the discovery document once, so a misspelled issuer shows up
// at startup instead of at the first student's login. A failure is
// deliberately not fatal to the caller: the provider being unreachable must
// not stop a course server from starting, and the token login keeps working.
func (p *Provider) Probe(ctx context.Context) error {
	_, err := p.discover(ctx)
	return err
}

// discover returns the cached discovery document, fetching it when it is
// missing or stale.
func (p *Provider) discover(ctx context.Context) (*metadata, error) {
	p.mu.Lock()
	if p.meta != nil && p.now().Sub(p.metaAt) < discoveryTTL {
		m := p.meta
		p.mu.Unlock()
		return m, nil
	}
	p.mu.Unlock()

	target := strings.TrimSuffix(p.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	var m metadata
	if err := p.getJSON(ctx, target, &m); err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	// The document says who it is for. A mismatch means the issuer URL and the
	// `iss` the tokens will carry are not the same string, and every later
	// comparison would be against the wrong one.
	if m.Issuer != p.cfg.Issuer {
		return nil, fmt.Errorf("oidc discovery: document issuer %q does not match configured %q", m.Issuer, p.cfg.Issuer)
	}
	if m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" || m.JWKSURI == "" {
		return nil, errors.New("oidc discovery: document is missing an endpoint")
	}
	p.mu.Lock()
	p.meta, p.metaAt = &m, p.now()
	p.mu.Unlock()
	return &m, nil
}

// Flow is the per-login state a relying party keeps between the redirect out
// and the callback back. All three fields are credentials: State is what ties
// the callback to the browser that started it, Nonce is what ties the ID token
// to this one login, and Verifier is the PKCE secret that makes a stolen
// authorization code useless.
type Flow struct {
	State    string
	Nonce    string
	Verifier string
}

// NewFlow draws a fresh state, nonce and PKCE verifier.
func NewFlow() (Flow, error) {
	var f Flow
	for _, dst := range []*string{&f.State, &f.Nonce, &f.Verifier} {
		v, err := randomString()
		if err != nil {
			return Flow{}, err
		}
		*dst = v
	}
	return f, nil
}

// randomString returns 32 bytes of entropy as base64url text, which is also a
// legal PKCE code verifier (43 unreserved characters).
func randomString() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// AuthCodeURL is where the browser is sent to authenticate.
func (p *Provider) AuthCodeURL(ctx context.Context, f Flow) (string, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(f.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.cfg.ClientID},
		"redirect_uri":          {p.cfg.RedirectURL},
		"scope":                 {strings.Join(p.cfg.Scopes, " ")},
		"state":                 {f.State},
		"nonce":                 {f.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(m.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return m.AuthorizationEndpoint + sep + q.Encode(), nil
}

// Identity is what a verified ID token says about the person at the browser.
// Subject is the only field that identifies them durably: a login or an email
// can be renamed on the provider's side, `iss`+`sub` cannot.
type Identity struct {
	Issuer  string
	Subject string
	// Login is the configured claim, trimmed and lowercased - the value an
	// account login is matched against.
	Login string
	Name  string
	Email string
}

// tokenResponse is the token endpoint's reply; only the ID token matters here.
// The access token is deliberately unused: every claim needed is in the ID
// token, and a userinfo round trip would be a third network call on the path a
// student waits on.
type tokenResponse struct {
	IDToken string `json:"id_token"`
	Error   string `json:"error"`
	Desc    string `json:"error_description"`
}

// Exchange redeems the authorization code and verifies the ID token that comes
// back. Every failure is an error the caller must treat as "not authenticated";
// none of them is safe to show to the browser verbatim.
func (p *Provider) Exchange(ctx context.Context, code string, f Flow) (Identity, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return Identity{}, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.cfg.RedirectURL},
		"client_id":     {p.cfg.ClientID},
		"code_verifier": {f.Verifier},
	}
	// client_secret_basic is what OIDC Core requires every provider to accept,
	// so it is the default; the secret moves into the body only for a provider
	// that advertises client_secret_post and not basic. A public client sends
	// neither and is authenticated by PKCE alone.
	basic := p.cfg.ClientSecret != "" && !usesPostAuth(m.AuthMethods)
	if p.cfg.ClientSecret != "" && !basic {
		form.Set("client_secret", p.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		// RFC 6749 §2.3.1: both halves are form-urlencoded before the base64.
		req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("oidc token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Identity{}, fmt.Errorf("oidc token response: %w", err)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return Identity{}, fmt.Errorf("oidc token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || tr.Error != "" {
		return Identity{}, fmt.Errorf("oidc token endpoint: status %d, error %q %q", resp.StatusCode, tr.Error, tr.Desc)
	}
	if tr.IDToken == "" {
		return Identity{}, errors.New("oidc token response: no id_token")
	}
	return p.verify(ctx, tr.IDToken, f.Nonce)
}

func usesPostAuth(methods []string) bool {
	var post bool
	for _, m := range methods {
		if m == "client_secret_basic" {
			return false
		}
		if m == "client_secret_post" {
			post = true
		}
	}
	return post
}

// getJSON fetches and decodes one provider document under the body cap.
func (p *Provider) getJSON(ctx context.Context, target string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", target, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}
