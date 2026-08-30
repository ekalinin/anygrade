// Package hidden is the git-backed hidden-tests cache (SPEC §6.1 step 3):
// one bare mirror per hidden-repo URL under <data dir>/hidden/<sha256(url)>,
// fetched at prepare time with an offline fallback to the last successful
// commit. Errors crossing the package boundary are generalized - they end up
// in submissions.worker_note, which students see (SPEC §14); full detail
// (URL, git stderr) goes only to the server-side log.
package hidden

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/gitserver"
	"github.com/ekalinin/anygrade/internal/runner"
)

// ErrConfig marks a permanent, non-retryable hidden-tests configuration
// fault (e.g. the configured path: does not exist at the resolved commit).
// intake maps it to a terminal submission; every other error the cache
// returns is a retryable infra failure. Messages are already student-safe.
var ErrConfig = errors.New("hidden tests configuration error")

// errUnavailable is the single student-visible wording every hidden-tests
// failure collapses to: nothing about the URL, the cache directory, or the
// configured path may reach submissions.worker_note (SPEC §14).
var errUnavailable = errors.New("hidden tests temporarily unavailable")

// Cache resolves `hidden_tests: source: git` specs into runner.Source
// overlays. Fetches to one URL are serialized and TTL-coalesced; different
// URLs proceed in parallel. It never touches the DB and is safe for
// concurrent use by the worker pool. `anygrade check` never constructs one:
// the student self-check tool must not fetch hidden tests (SPEC §11).
type Cache struct {
	Dir   string        // cache root, e.g. <data dir>/hidden
	Token string        // ANYGRADE_HIDDEN_GIT_TOKEN; "" = ssh agent / no auth
	User  string        // credential username; "" = "x-access-token"
	TTL   time.Duration // per-(url,ref) refetch window; 0 = 60s
	Log   *slog.Logger  // full-detail sink (URLs, git stderr); nil = default
	// Env holds extra git environment (protocol.file.allow for file://
	// remotes in tests), mirroring gitserver.GitSource.Env.
	Env []string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	fresh map[string]freshEntry
}

// freshEntry remembers the last resolved commit of one (url, ref) pair so
// rapid successive submissions skip the network entirely.
type freshEntry struct {
	at  time.Time
	sha string
}

// Source syncs the hidden repo described by spec and returns an overlay
// rooted at spec.Path, so Assemble's Hidden.Export(ctx, "", taskDst) drops
// the subdir contents onto the task dir. On an unreachable remote it serves
// the last successfully fetched commit; with no warm cache it returns a
// retryable (already scrubbed) error.
func (c *Cache) Source(ctx context.Context, spec config.HiddenTests) (runner.Source, error) {
	dir := filepath.Join(c.Dir, hashHex(spec.URL))
	lk := c.urlLock(spec.URL)
	lk.Lock()
	defer lk.Unlock()

	sha, err := c.resolve(ctx, dir, spec)
	if err != nil {
		return nil, err
	}

	sub := path.Clean(strings.Trim(spec.Path, "/"))
	if sub == "." {
		sub = ""
	}
	if sub != "" {
		if _, _, err := c.git(ctx, dir, nil, "cat-file", "-e", sha+":"+sub); err != nil {
			c.logger().Error("hidden path missing at resolved commit",
				"url", spec.URL, "ref", spec.Ref, "path", spec.Path, "commit", sha)
			return nil, fmt.Errorf("%w: configured path is missing in the hidden repo", ErrConfig)
		}
	}
	// Wrapped: a failed export names the cache directory and echoes git's
	// stderr, and that error is what ends up in worker_note (SPEC §14).
	return scrubbed{
		inner: subdirSource{
			inner: gitserver.GitSource{Dir: dir, Commit: sha, Env: c.Env},
			sub:   sub,
		},
		log: c.logger(),
	}, nil
}

// resolve returns the commit to grade against: the TTL-fresh cached one, a
// freshly fetched one, or (remote down) the pinned last-good one.
func (c *Cache) resolve(ctx context.Context, dir string, spec config.HiddenTests) (string, error) {
	ref := spec.Ref
	if ref == "" {
		ref = "HEAD"
	}
	key := spec.URL + "\x00" + ref
	pin := "refs/anygrade/hidden/" + hashHex(ref)

	c.mu.Lock()
	e, fresh := c.fresh[key]
	c.mu.Unlock()
	if fresh && time.Since(e.at) < c.ttl() {
		return e.sha, nil
	}

	// Unconditional: the cache holds the hidden tests themselves, and a cache
	// created by an older install is 0755 until something narrows it.
	if err := c.ensureCacheDir(dir); err != nil {
		return "", c.infra("create hidden cache", spec, err, "")
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if _, stderr, err := c.git(ctx, dir, nil, "init", "--bare", "--quiet"); err != nil {
			return "", c.infra("init hidden cache", spec, err, stderr)
		}
	}

	// Fetch the one ref into FETCH_HEAD; branch, tag, and (where the server
	// allows it) raw SHA all resolve through the same path.
	_, stderr, fetchErr := c.git(ctx, dir, c.credArgs(spec.URL),
		"fetch", "--no-tags", "--quiet", spec.URL, ref)
	if fetchErr == nil {
		out, stderr, err := c.git(ctx, dir, nil, "rev-parse", "FETCH_HEAD")
		if err != nil {
			return "", c.infra("resolve FETCH_HEAD", spec, err, stderr)
		}
		sha := strings.TrimSpace(out)
		// Pin the commit: it survives gc and backs the offline fallback.
		if _, stderr, err := c.git(ctx, dir, nil, "update-ref", pin, sha); err != nil {
			return "", c.infra("pin hidden commit", spec, err, stderr)
		}
		c.markFresh(key, sha)
		return sha, nil
	}

	// Remote unreachable (or ref vanished): fall back to the last good
	// commit if we ever fetched one (SPEC §6.1 step 3).
	if out, _, err := c.git(ctx, dir, nil, "rev-parse", "--verify", pin+"^{commit}"); err == nil {
		sha := strings.TrimSpace(out)
		c.logger().Warn("hidden fetch failed; serving last cached commit",
			"url", spec.URL, "ref", ref, "commit", sha, "err", fetchErr, "stderr", stderr)
		c.markFresh(key, sha) // avoid stalling every Prepare on a dead remote
		return sha, nil
	}
	return "", c.infra("fetch hidden tests", spec, fetchErr, stderr)
}

// infra logs the full failure server-side and returns the generalized,
// student-safe error that lands in worker_note.
func (c *Cache) infra(op string, spec config.HiddenTests, err error, stderr string) error {
	c.logger().Error("hidden tests: "+op,
		"url", spec.URL, "ref", spec.Ref, "err", err, "stderr", stderr)
	return errUnavailable
}

// ensureCacheDir creates the per-URL mirror directory and narrows it, and the
// cache root above it, to owner-only. Hidden tests must not be readable by
// other accounts on the host any more than the students' repos are; what git
// then creates inside the mirror keeps git's own modes, which the 0700 root
// makes unreachable anyway.
func (c *Cache) ensureCacheDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, p := range []string{c.Dir, dir} {
		if err := os.Chmod(p, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cache) markFresh(key, sha string) {
	c.mu.Lock()
	if c.fresh == nil {
		c.fresh = map[string]freshEntry{}
	}
	c.fresh[key] = freshEntry{at: time.Now(), sha: sha}
	c.mu.Unlock()
}

func (c *Cache) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return time.Minute
}

// urlLock serializes fetch/resolve per URL: concurrent fetches into one bare
// repo are safe on git 2.54, but FETCH_HEAD is shared state and coalescing
// spares the remote.
func (c *Cache) urlLock(url string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks == nil {
		c.locks = map[string]*sync.Mutex{}
	}
	if c.locks[url] == nil {
		c.locks[url] = &sync.Mutex{}
	}
	return c.locks[url]
}

// credArgs injects the token for http(s) remotes via an inline credential
// helper that references the secret by environment variable NAME: the value
// never appears in argv (ps), .git/config, or git's stderr. The leading
// empty helper clears any system helper so nothing else is consulted or
// persisted. ssh:// remotes rely on the host agent/config instead.
func (c *Cache) credArgs(url string) []string {
	if c.Token == "" || !strings.HasPrefix(url, "http") {
		return nil
	}
	const helper = `!f() { test "$1" = get && printf "username=%s\npassword=%s\n" ` +
		`"${ANYGRADE_HIDDEN_GIT_USER:-x-access-token}" "$ANYGRADE_HIDDEN_GIT_TOKEN"; }; f`
	return []string{"-c", "credential.helper=", "-c", "credential.helper=" + helper}
}

// git runs one git command against the cache repo, prompts disabled.
//
// gc.autoDetach=false for the same reason the bare repos carry it in their own
// config: a fetch would otherwise return while a detached repack keeps writing
// into the cache dir, outliving both the context and the process that started
// it. Nothing but this method ever runs git here, so the flag rides along
// instead of being persisted.
func (c *Cache) git(ctx context.Context, dir string, cfg []string, args ...string) (stdout, stderr string, err error) {
	full := append([]string{"-C", dir, "-c", "gc.autoDetach=false"}, cfg...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes",
	)
	// The token travels only in the child environment (owner-readable),
	// where the credential helper expands it at run time.
	if c.Token != "" {
		cmd.Env = append(cmd.Env, "ANYGRADE_HIDDEN_GIT_TOKEN="+c.Token)
	}
	if c.User != "" {
		cmd.Env = append(cmd.Env, "ANYGRADE_HIDDEN_GIT_USER="+c.User)
	}
	cmd.Env = append(cmd.Env, c.Env...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	return out.String(), strings.TrimSpace(errb.String()), err
}

func (c *Cache) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// subdirSource re-roots a GitSource at a subdirectory of the hidden repo:
// srcRel "" means the subdir itself, matching how Assemble overlays
// Hidden.Export(ctx, "", taskDst).
type subdirSource struct {
	inner gitserver.GitSource
	sub   string // slash path inside the hidden repo; "" = repo root
}

func (s subdirSource) Export(ctx context.Context, srcRel, dstAbs string) error {
	return s.inner.Export(ctx, path.Join(s.sub, srcRel), dstAbs)
}

func (s subdirSource) Open(ctx context.Context, srcRel string) (io.ReadCloser, bool, error) {
	return s.inner.Open(ctx, path.Join(s.sub, srcRel))
}
