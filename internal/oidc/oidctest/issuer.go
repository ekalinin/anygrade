// Package oidctest is a fake OpenID Connect issuer for tests: its own signing
// key, its own JWKS, and knobs for every way an ID token can be wrong. It lives
// in a package of its own rather than in each test file because the same fake
// is driven from three places - the protocol tests, the web handler tests and
// the end-to-end suite - and three copies of an issuer would drift apart
// exactly where the tests are supposed to agree.
//
// Nothing in the shipped binary imports it, so it is never linked into a
// release.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"
)

// ClientID is the client every issuer here mints tokens for unless a test
// overrides Token.Audience.
const ClientID = "anygrade-test"

// ClientSecret is the confidential-client secret the token endpoint demands.
const ClientSecret = "s3cret"

// Token is one ID token to mint. The zero value is a valid token for Subject;
// each field that is set replaces a correct value with a wrong one, which is
// how the refusal tests are written.
type Token struct {
	Subject  string
	Login    string // the preferred_username claim
	Email    string
	Name     string
	Nonce    string
	Issuer   string   // overrides the real issuer
	Audience []string // overrides ClientID
	AZP      string
	Expiry   time.Time // overrides "an hour from now"
	IssuedAt time.Time
	// EmailVerified is emitted only when set; the default token carries no
	// email_verified claim at all.
	EmailVerified *bool
	// Alg overrides the JOSE header's alg without changing how the token is
	// signed - "none" produces an unsigned token.
	Alg string
	// BadSignature flips the signature after it is computed.
	BadSignature bool
}

// Issuer is a running fake provider.
type Issuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string

	mu sync.Mutex
	// codes maps an issued authorization code to the token it redeems for.
	codes map[string]Token
	// LastAuth is the query of the most recent /authorize request.
	lastAuth url.Values
	// Redeemed counts successful code redemptions, so a test can prove a code
	// is single use.
	redeemed int
}

// New starts a fake issuer. Close it with Close.
func New() (*Issuer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	is := &Issuer{key: key, kid: "test-key-1", codes: map[string]Token{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", is.discovery)
	mux.HandleFunc("/jwks", is.jwks)
	mux.HandleFunc("/authorize", is.authorize)
	mux.HandleFunc("/token", is.token)
	is.srv = httptest.NewServer(mux)
	return is, nil
}

func (is *Issuer) Close() { is.srv.Close() }

// URL is the issuer identifier, which is also its base URL.
func (is *Issuer) URL() string { return is.srv.URL }

// LastAuth returns the query the last /authorize request carried.
func (is *Issuer) LastAuth() url.Values {
	is.mu.Lock()
	defer is.mu.Unlock()
	return is.lastAuth
}

// Redeemed is how many authorization codes have been exchanged.
func (is *Issuer) Redeemed() int {
	is.mu.Lock()
	defer is.mu.Unlock()
	return is.redeemed
}

// IssueCode registers an authorization code that redeems for tok.
func (is *Issuer) IssueCode(code string, tok Token) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.codes[code] = tok
}

func (is *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                is.srv.URL,
		"authorization_endpoint":                is.srv.URL + "/authorize",
		"token_endpoint":                        is.srv.URL + "/token",
		"jwks_uri":                              is.srv.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
	})
}

func (is *Issuer) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := is.key.Public().(*rsa.PublicKey)
	writeJSON(w, map[string]any{"keys": []map[string]any{{
		"kty": "RSA",
		"kid": is.kid,
		"alg": "RS256",
		"use": "sig",
		"n":   b64(pub.N.Bytes()),
		"e":   b64(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// authorize records the request and, when the test has already registered a
// code for it, redirects straight back - which is what lets a test drive the
// whole flow with an HTTP client that follows redirects.
func (is *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	is.mu.Lock()
	is.lastAuth = q
	is.mu.Unlock()
	redirect := q.Get("redirect_uri")
	if redirect == "" {
		http.Error(w, "no redirect_uri", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, redirect+"?code=auto&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
}

func (is *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeErr(w, "invalid_request")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok || id != url.QueryEscape(ClientID) || secret != url.QueryEscape(ClientSecret) {
		writeErr(w, "invalid_client")
		return
	}
	if r.PostFormValue("code_verifier") == "" {
		writeErr(w, "invalid_grant")
		return
	}
	is.mu.Lock()
	tok, known := is.codes[r.PostFormValue("code")]
	if known {
		delete(is.codes, r.PostFormValue("code"))
		is.redeemed++
	}
	is.mu.Unlock()
	if !known {
		writeErr(w, "invalid_grant")
		return
	}
	writeJSON(w, map[string]any{
		"access_token": "opaque",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     is.Mint(tok),
	})
}

// Mint builds an ID token from tok, filling in every field the test left at its
// zero value with a correct one.
func (is *Issuer) Mint(tok Token) string {
	now := time.Now()
	claims := map[string]any{
		"iss": or(tok.Issuer, is.srv.URL),
		"sub": or(tok.Subject, "subject-1"),
		"exp": pick(tok.Expiry, now.Add(time.Hour)).Unix(),
		"iat": pick(tok.IssuedAt, now).Unix(),
	}
	if len(tok.Audience) > 0 {
		claims["aud"] = tok.Audience
	} else {
		claims["aud"] = ClientID
	}
	if tok.AZP != "" {
		claims["azp"] = tok.AZP
	}
	if tok.Nonce != "" {
		claims["nonce"] = tok.Nonce
	}
	if tok.Login != "" {
		claims["preferred_username"] = tok.Login
	}
	if tok.Email != "" {
		claims["email"] = tok.Email
	}
	if tok.Name != "" {
		claims["name"] = tok.Name
	}
	if tok.EmailVerified != nil {
		claims["email_verified"] = *tok.EmailVerified
	}

	alg := or(tok.Alg, "RS256")
	header, _ := json.Marshal(map[string]any{"alg": alg, "typ": "JWT", "kid": is.kid})
	payload, _ := json.Marshal(claims)
	signing := b64(header) + "." + b64(payload)
	if alg == "none" {
		// The classic forgery: a well-formed JWT with an empty signature.
		return signing + "."
	}
	sig := is.sign(signing)
	if tok.BadSignature {
		sig[0] ^= 0xff
	}
	return signing + "." + b64(sig)
}

func (is *Issuer) sign(signing string) []byte {
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, is.key, crypto.SHA256, sum[:])
	if err != nil {
		panic("oidctest: sign: " + err.Error())
	}
	return sig
}

// AuthCode drives the authorization request the way a browser would: it reads
// the state and nonce out of authURL, registers a code that redeems for tok
// with that nonce, and returns the code and the state.
func (is *Issuer) AuthCode(authURL string, tok Token) (code, state string, err error) {
	u, err := url.Parse(authURL)
	if err != nil {
		return "", "", err
	}
	q := u.Query()
	if tok.Nonce == "" {
		tok.Nonce = q.Get("nonce")
	}
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	code = "code-" + b64(buf)
	is.IssueCode(code, tok)
	return code, q.Get("state"), nil
}

func or(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func pick(v, fallback time.Time) time.Time {
	if v.IsZero() {
		return fallback
	}
	return v
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
