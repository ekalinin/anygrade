// Package ident holds the shared identifier rules. A login doubles as a
// filesystem path component (students/<login>.git) and a web identity, so
// gitserver, web, and cli must agree on exactly one validation rule.
package ident

import (
	"regexp"
	"strings"
)

// loginRe: lowercase, starts alphanumeric, then [a-z0-9._-].
var loginRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// reservedLogin is "course": the hook-protocol sentinel that routes a
// post-receive to the upstream course repo instead of a student's own
// (gitserver.hookEnv, intake.Server.dispatch). A login here would collide
// with it and be silently misrouted, so it is refused like any other invalid
// login rather than left to reach either site.
const reservedLogin = "course"

// ValidLogin reports whether s is safe as a login and repo path component.
func ValidLogin(s string) bool {
	return s != reservedLogin && loginRe.MatchString(s) && !strings.Contains(s, "..") && len(s) <= 64
}
