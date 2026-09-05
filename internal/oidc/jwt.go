package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"strings"
)

// clockSkew absorbs the difference between this server's clock and the
// provider's, in the seconds JWT timestamps are counted in. It is deliberately
// small: every second of it is a second an expired ID token is still accepted.
const clockSkew = 30

// maxClaimLen bounds the two claims that leave this package and get stored -
// the subject, which goes on the users row, and the login claim, which lands
// in the audit log. Both are far below the limit at every real provider, and
// the limit is what keeps unbounded provider-controlled text out of the
// database when one is not real.
const maxClaimLen = 255

// sigAlg describes one accepted JOSE signature algorithm. The table *is* the
// allowlist: `none` and the HMAC family have no entry, so an unsigned token and
// a token signed with a symmetric key the issuer never shared are refused
// before anything else is looked at. `alg: none` in particular is the classic
// JWT forgery, and the only defence that works is never to reach a code path
// that treats a missing signature as valid.
type sigAlg struct {
	kty  string // required JWK key type
	hash crypto.Hash
	pss  bool // RSA-PSS instead of PKCS#1 v1.5
	// size is the fixed length of each half of an ECDSA signature.
	size int
}

var sigAlgs = map[string]sigAlg{
	"RS256": {kty: "RSA", hash: crypto.SHA256},
	"RS384": {kty: "RSA", hash: crypto.SHA384},
	"RS512": {kty: "RSA", hash: crypto.SHA512},
	"PS256": {kty: "RSA", hash: crypto.SHA256, pss: true},
	"PS384": {kty: "RSA", hash: crypto.SHA384, pss: true},
	"PS512": {kty: "RSA", hash: crypto.SHA512, pss: true},
	"ES256": {kty: "EC", hash: crypto.SHA256, size: 32},
	"ES384": {kty: "EC", hash: crypto.SHA384, size: 48},
	"ES512": {kty: "EC", hash: crypto.SHA512, size: 66},
}

type joseHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// audience is `aud`, which OIDC allows to be either a string or an array.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("aud is neither a string nor an array of strings")
	}
	*a = many
	return nil
}

func (a audience) has(s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

// idClaims are the claims a relying party must check. Everything else is read
// out of the raw map alongside it.
type idClaims struct {
	Issuer          string   `json:"iss"`
	Subject         string   `json:"sub"`
	Audience        audience `json:"aud"`
	AuthorizedParty string   `json:"azp"`
	Expiry          int64    `json:"exp"`
	IssuedAt        int64    `json:"iat"`
	NotBefore       int64    `json:"nbf"`
	Nonce           string   `json:"nonce"`
}

// verify checks the ID token's signature and every claim, then maps it onto an
// Identity. Order matters: nothing is trusted until the signature is verified
// against a key the issuer published.
func (p *Provider) verify(ctx context.Context, token, nonce string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("oidc: id_token is not a compact JWS")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Identity{}, fmt.Errorf("oidc: id_token header: %w", err)
	}
	var hdr joseHeader
	if err := json.Unmarshal(headerRaw, &hdr); err != nil {
		return Identity{}, fmt.Errorf("oidc: id_token header: %w", err)
	}
	alg, ok := sigAlgs[hdr.Alg]
	if !ok {
		return Identity{}, fmt.Errorf("oidc: id_token alg %q is not accepted", hdr.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, fmt.Errorf("oidc: id_token signature: %w", err)
	}
	if len(sig) == 0 {
		return Identity{}, errors.New("oidc: id_token carries no signature")
	}
	signed := []byte(parts[0] + "." + parts[1])
	if err := p.checkSignature(ctx, hdr.Kid, alg, signed, sig); err != nil {
		return Identity{}, err
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}, fmt.Errorf("oidc: id_token payload: %w", err)
	}
	var c idClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Identity{}, fmt.Errorf("oidc: id_token payload: %w", err)
	}
	if c.Issuer != p.cfg.Issuer {
		return Identity{}, fmt.Errorf("oidc: id_token iss %q, want %q", c.Issuer, p.cfg.Issuer)
	}
	if !c.Audience.has(p.cfg.ClientID) {
		return Identity{}, fmt.Errorf("oidc: id_token aud %v does not contain the client id", []string(c.Audience))
	}
	// With more than one audience the token was minted for somebody else too,
	// and only `azp` says which party it is actually for (OIDC Core 3.1.3.7).
	if len(c.Audience) > 1 && c.AuthorizedParty != p.cfg.ClientID {
		return Identity{}, fmt.Errorf("oidc: id_token azp %q, want %q", c.AuthorizedParty, p.cfg.ClientID)
	}
	now := p.now().Unix()
	if c.Expiry == 0 || now-clockSkew >= c.Expiry {
		return Identity{}, fmt.Errorf("oidc: id_token expired (exp %d, now %d)", c.Expiry, now)
	}
	if c.IssuedAt == 0 || c.IssuedAt-clockSkew > now {
		return Identity{}, fmt.Errorf("oidc: id_token iat %d is in the future (now %d)", c.IssuedAt, now)
	}
	if c.NotBefore != 0 && c.NotBefore-clockSkew > now {
		return Identity{}, fmt.Errorf("oidc: id_token not valid yet (nbf %d, now %d)", c.NotBefore, now)
	}
	// The nonce is what makes this token *this* login: without the check, an ID
	// token captured from another session of the same student replays here.
	if nonce == "" || c.Nonce != nonce {
		return Identity{}, errors.New("oidc: id_token nonce does not match the login it answers")
	}
	if c.Subject == "" || len(c.Subject) > maxClaimLen {
		return Identity{}, errors.New("oidc: id_token has no usable sub")
	}

	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Identity{}, fmt.Errorf("oidc: id_token payload: %w", err)
	}
	login, _ := raw[p.cfg.LoginClaim].(string)
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" || len(login) > maxClaimLen {
		return Identity{}, fmt.Errorf("oidc: id_token carries no usable %q claim to match an account by", p.cfg.LoginClaim)
	}
	// An email is only an identifier if the provider says it verified it.
	// Providers that let a user type any address would otherwise hand out a
	// claim on any account whose login happens to be that address.
	if p.cfg.LoginClaim == "email" {
		if verified, _ := raw["email_verified"].(bool); !verified {
			return Identity{}, errors.New("oidc: email is not marked verified by the issuer")
		}
	}
	name, _ := raw["name"].(string)
	email, _ := raw["email"].(string)
	return Identity{
		Issuer:  c.Issuer,
		Subject: c.Subject,
		Login:   login,
		Name:    strings.TrimSpace(name),
		Email:   strings.TrimSpace(email),
	}, nil
}

// checkSignature verifies sig over signed with the issuer's published key for
// kid, refetching the key set once when the kid is unknown (a rotation).
func (p *Provider) checkSignature(ctx context.Context, kid string, alg sigAlg, signed, sig []byte) error {
	keys, err := p.jwks(ctx, false)
	if err != nil {
		return err
	}
	if !hasKid(keys, kid) {
		// A kid nobody published is either a rotation or a forgery. Refetching
		// tells the two apart; jwksMinRefresh is what keeps the second case from
		// becoming a way to make the server poll the provider.
		if refreshed, rerr := p.jwks(ctx, true); rerr == nil {
			keys = refreshed
		}
	}
	var last error
	for _, k := range keys {
		if kid != "" && k.Kid != "" && k.Kid != kid {
			continue
		}
		if k.Kty != alg.kty {
			continue
		}
		pub, perr := k.publicKey()
		if perr != nil {
			last = perr
			continue
		}
		verr := verifyWith(pub, alg, signed, sig)
		if verr == nil {
			return nil
		}
		last = verr
	}
	if last == nil {
		last = errors.New("no published key matches")
	}
	return fmt.Errorf("oidc: id_token signature: %w", last)
}

func hasKid(keys []jwk, kid string) bool {
	if kid == "" {
		return len(keys) > 0
	}
	for _, k := range keys {
		if k.Kid == kid {
			return true
		}
	}
	return false
}

func verifyWith(pub crypto.PublicKey, alg sigAlg, signed, sig []byte) error {
	sum := digest(alg.hash, signed)
	switch key := pub.(type) {
	case *rsa.PublicKey:
		if alg.pss {
			return rsa.VerifyPSS(key, alg.hash, sum, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		}
		return rsa.VerifyPKCS1v15(key, alg.hash, sum, sig)
	case *ecdsa.PublicKey:
		if len(sig) != 2*alg.size {
			return fmt.Errorf("ecdsa signature is %d bytes, want %d", len(sig), 2*alg.size)
		}
		r := new(big.Int).SetBytes(sig[:alg.size])
		s := new(big.Int).SetBytes(sig[alg.size:])
		if !ecdsa.Verify(key, sum, r, s) {
			return errors.New("ecdsa signature does not verify")
		}
		return nil
	}
	return errors.New("unsupported key type")
}

func digest(h crypto.Hash, b []byte) []byte {
	var d hash.Hash
	switch h {
	case crypto.SHA256:
		d = sha256.New()
	case crypto.SHA384:
		d = sha512.New384()
	default:
		d = sha512.New()
	}
	d.Write(b)
	return d.Sum(nil)
}

// jwk is one published verification key. Only the fields needed to rebuild an
// RSA or EC public key are read.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("jwk n: %w", err)
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("jwk e: %w", err)
		}
		exp := new(big.Int).SetBytes(e)
		if !exp.IsInt64() || exp.Int64() < 3 {
			return nil, errors.New("jwk e is out of range")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("jwk crv %q is not supported", k.Crv)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, fmt.Errorf("jwk x: %w", err)
		}
		y, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			return nil, fmt.Errorf("jwk y: %w", err)
		}
		key := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !curve.IsOnCurve(key.X, key.Y) {
			return nil, errors.New("jwk point is not on the curve")
		}
		return key, nil
	}
	return nil, fmt.Errorf("jwk kty %q is not supported", k.Kty)
}

// jwks returns the issuer's key set, fetching it when it has never been read or
// when force is set and the last fetch is older than jwksMinRefresh.
func (p *Provider) jwks(ctx context.Context, force bool) ([]jwk, error) {
	p.mu.Lock()
	fresh := p.keysOnce && (!force || p.now().Sub(p.keysAt) < jwksMinRefresh)
	if fresh {
		keys := p.keys
		p.mu.Unlock()
		return keys, nil
	}
	p.mu.Unlock()

	m, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := p.getJSON(ctx, m.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("oidc jwks: %w", err)
	}
	if len(set.Keys) == 0 {
		return nil, errors.New("oidc jwks: the issuer published no keys")
	}
	p.mu.Lock()
	p.keys, p.keysAt, p.keysOnce = set.Keys, p.now(), true
	p.mu.Unlock()
	return set.Keys, nil
}
