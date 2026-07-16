package gitserver

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/ekalinin/anygrade/internal/hookproto"
	"github.com/ekalinin/anygrade/internal/ratelimit"
)

// Identity is an authenticated git user. gitserver never touches the store:
// the composition root injects an Authenticator backed by it.
type Identity struct {
	UserID int64
	Login  string
	Role   string // student | teacher
}

// Authenticator resolves credentials to identities (SPEC §8).
type Authenticator interface {
	// ByToken checks a login + personal-access-token pair (HTTP basic auth).
	ByToken(ctx context.Context, login, token string) (Identity, bool, error)
	// ByFingerprint resolves an SSH public-key fingerprint (SHA256: form).
	ByFingerprint(ctx context.Context, fingerprint string) (Identity, bool, error)
}

// Authorize is the pure repo-access policy (SPEC §7): students read/write
// their own repo and read the course repo; teachers do everything.
// owner "" means the course repo.
func Authorize(id Identity, owner string, write bool) bool {
	if id.Role == "teacher" {
		return true
	}
	if owner == "" {
		return !write
	}
	return id.Login == owner
}

// HTTPHandler serves the git smart HTTP protocol (SPEC §7) by spawning
// `git upload-pack/receive-pack --stateless-rpc` (the Gitea approach).
// Routes: /git/course.git/... and /git/<login>/course.git/...
type HTTPHandler struct {
	Repos  *RepoManager
	Auth   Authenticator
	Socket string // intake unix socket, injected into receive-pack hooks
	// Local, when non-nil, disables authentication and acts as this identity
	// (serve --local; the caller guarantees a loopback bind).
	Local *Identity
	// Limit, when non-nil, throttles failed basic-auth attempts (shared with
	// the web login by the composition root).
	Limit *ratelimit.Limiter
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner, rest, ok := splitRepoPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	var svc string
	var write bool
	switch {
	case r.Method == http.MethodGet && rest == "info/refs":
		svc = r.URL.Query().Get("service")
		if svc != "git-upload-pack" && svc != "git-receive-pack" {
			// No dumb-HTTP fallback.
			http.Error(w, "smart HTTP only", http.StatusForbidden)
			return
		}
		write = svc == "git-receive-pack"
	case r.Method == http.MethodPost && rest == "git-upload-pack":
		svc = "git-upload-pack"
	case r.Method == http.MethodPost && rest == "git-receive-pack":
		svc, write = "git-receive-pack", true
	default:
		http.NotFound(w, r)
		return
	}

	id, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if !Authorize(id, owner, write) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	dir, err := h.repoDir(r.Context(), id, owner)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	env := append(os.Environ(), hookEnv(h.Socket, owner, id)...)
	if p := r.Header.Get("Git-Protocol"); p != "" {
		env = append(env, "GIT_PROTOCOL="+p)
	}

	if rest == "info/refs" {
		h.advertise(w, r, svc, dir, env)
		return
	}
	h.serviceRPC(w, r, svc, dir, env)
}

// advertise streams the ref advertisement: the `# service=` pkt-line header
// followed by `--advertise-refs` output.
func (h *HTTPHandler) advertise(w http.ResponseWriter, r *http.Request, svc, dir string, env []string) {
	w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")

	header := "# service=" + svc + "\n"
	fmt.Fprintf(w, "%04x%s0000", len(header)+4, header)

	cmd := exec.CommandContext(r.Context(), "git", gitSub(svc), "--stateless-rpc", "--advertise-refs", dir)
	cmd.Env = env
	cmd.Stdout = w
	cmd.Stderr = io.Discard
	_ = cmd.Run() // headers are already out; a failure just truncates the body
}

// serviceRPC pipes one stateless-rpc exchange through the git subcommand.
func (h *HTTPHandler) serviceRPC(w http.ResponseWriter, r *http.Request, svc, dir string, env []string) {
	body := io.Reader(r.Body)
	// git gzips large request bodies; small test pushes never exercise this,
	// so decode defensively (design risk #2).
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}

	w.Header().Set("Content-Type", "application/x-"+svc+"-result")
	w.Header().Set("Cache-Control", "no-cache")

	cmd := exec.CommandContext(r.Context(), "git", gitSub(svc), "--stateless-rpc", dir)
	cmd.Env = env
	cmd.Stdin = body
	cmd.Stdout = w
	cmd.Stderr = io.Discard
	_ = cmd.Run() // protocol errors are reported in-band to the git client
}

// authenticate resolves the request identity, writing the 401/500 itself
// when it fails.
func (h *HTTPHandler) authenticate(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	if h.Local != nil {
		return *h.Local, true
	}
	login, token, ok := r.BasicAuth()
	if ok {
		key := ratelimit.AuthKey(r.RemoteAddr, login)
		if h.Limit != nil && h.Limit.Blocked(key) {
			http.Error(w, "too many failed attempts, try again later", http.StatusTooManyRequests)
			return Identity{}, false
		}
		id, valid, err := h.Auth.ByToken(r.Context(), login, token)
		if err != nil {
			http.Error(w, "auth failed", http.StatusInternalServerError)
			return Identity{}, false
		}
		if valid {
			if h.Limit != nil {
				h.Limit.Clear(key)
			}
			return id, true
		}
		// Only real credential mismatches count; the credential-less probe
		// that precedes every git basic-auth exchange does not.
		if h.Limit != nil {
			h.Limit.Fail(key)
		}
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="anygrade"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
	return Identity{}, false
}

// repoDir resolves (and for the owner themselves lazily provisions) the bare
// repo directory. Others get a plain existence check: repos are created on
// the owner's first access, not by teacher browsing (SPEC §7).
func (h *HTTPHandler) repoDir(ctx context.Context, id Identity, owner string) (string, error) {
	if owner == "" {
		return h.Repos.CourseDir(), nil
	}
	if owner == id.Login {
		return h.Repos.EnsureStudent(ctx, owner)
	}
	dir := h.Repos.StudentDir(owner)
	if _, err := os.Stat(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// splitRepoPath parses "/git/course.git/<rest>" (owner "") and
// "/git/<login>/course.git/<rest>".
func splitRepoPath(p string) (owner, rest string, ok bool) {
	p, found := strings.CutPrefix(p, "/git/")
	if !found {
		return "", "", false
	}
	if after, found := strings.CutPrefix(p, "course.git/"); found {
		return "", after, true
	}
	login, tail, found := strings.Cut(p, "/")
	if !found || login == "" {
		return "", "", false
	}
	after, found := strings.CutPrefix(tail, "course.git/")
	if !found {
		return "", "", false
	}
	return login, after, true
}

// hookEnv is the receive-hook environment (verified to propagate from the
// spawned receive-pack into hooks on git 2.54).
func hookEnv(socket, owner string, id Identity) []string {
	repo := owner
	if repo == "" {
		repo = "course"
	}
	return []string{
		hookproto.EnvSocket + "=" + socket,
		hookproto.EnvRepo + "=" + repo,
		hookproto.EnvActor + "=" + id.Login,
		hookproto.EnvRole + "=" + id.Role,
	}
}

// gitSub maps the wire service name to the git subcommand.
func gitSub(svc string) string { return strings.TrimPrefix(svc, "git-") }
