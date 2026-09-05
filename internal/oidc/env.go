package oidc

import (
	"fmt"
	"os"
	"strings"
)

// Environment variables that configure the relying party. They are the whole
// configuration surface, and it is the environment rather than course.yaml for
// the reason SPEC §11 already applies to the hidden-tests token: the client id
// and secret are credentials, and course.yaml is cloned by every student. They
// are not flags either - a secret on the command line is in every `ps` listing
// on the machine.
const (
	// EnvIssuer is also the switch: unset means no identity provider, and the
	// server behaves exactly as it did before this existed.
	EnvIssuer       = "ANYGRADE_OIDC_ISSUER"
	EnvClientID     = "ANYGRADE_OIDC_CLIENT_ID"
	EnvClientSecret = "ANYGRADE_OIDC_CLIENT_SECRET"
	EnvScopes       = "ANYGRADE_OIDC_SCOPES"      // space separated; default "openid profile email"
	EnvLoginClaim   = "ANYGRADE_OIDC_LOGIN_CLAIM" // default "preferred_username"
	EnvProviderName = "ANYGRADE_OIDC_NAME"        // login button label; default the issuer host
)

// FromEnv reads the relying party configuration. enabled is false when
// EnvIssuer is unset, which is the documented way to run without an identity
// provider; an issuer set with the rest missing is an error rather than a
// silent fallback, because a half-configured provider is a login button that
// cannot work.
//
// redirectURL is the site's public base URL with CallbackPath appended - the
// caller knows it (`--base-url`), and deriving it here would need the serve
// options this package must not see.
func FromEnv(redirectURL string) (cfg Config, enabled bool, err error) {
	issuer := strings.TrimSpace(os.Getenv(EnvIssuer))
	if issuer == "" {
		return Config{}, false, nil
	}
	clientID := strings.TrimSpace(os.Getenv(EnvClientID))
	if clientID == "" {
		return Config{}, false, fmt.Errorf("%s is set but %s is not", EnvIssuer, EnvClientID)
	}
	if redirectURL == "" {
		return Config{}, false, fmt.Errorf("%s is set but the public base URL is unknown; pass --base-url", EnvIssuer)
	}
	return Config{
		// Verbatim, trailing slash included: the issuer string is compared
		// byte for byte against the `iss` claim, and some providers really do
		// spell themselves with one.
		Issuer:       issuer,
		ClientID:     clientID,
		ClientSecret: os.Getenv(EnvClientSecret),
		RedirectURL:  strings.TrimSuffix(redirectURL, "/") + CallbackPath,
		Scopes:       strings.Fields(os.Getenv(EnvScopes)),
		LoginClaim:   strings.TrimSpace(os.Getenv(EnvLoginClaim)),
		ProviderName: strings.TrimSpace(os.Getenv(EnvProviderName)),
	}, true, nil
}
