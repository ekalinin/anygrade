package hidden

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ekalinin/anygrade/internal/runner"
)

// LocalRootsEnv names the operator-controlled allowlist of directory trees
// that `hidden_tests: source: local` may read from - a colon-separated list of
// absolute paths, in the same spirit as ANYGRADE_HIDDEN_GIT_TOKEN: the course
// repo says what it wants, the environment says what this process is allowed
// to hand over.
//
// Unset keeps the pre-existing behaviour and accepts any path. Denying by
// default would break every course already shipping the SPEC §4.3 example
// (`path: /srv/hidden/01-intro`) on upgrade, and would do it silently -
// `validate` runs where the variable need not be set at all. Setting it is
// what an operator does when the teachers pushing course.yaml are not the
// administrators of the machine; on the common single-course install, where
// the two are the same person, there is no boundary to enforce.
const LocalRootsEnv = "ANYGRADE_HIDDEN_LOCAL_ROOTS"

// LocalSource resolves `hidden_tests: source: local` into an overlay rooted at
// path. ErrConfig marks what retrying cannot fix - a path outside the
// allowlist, a missing one, one that is not a directory; every other stat
// failure (a network mount blinking, a permission being repaired) comes back
// as a plain retryable error, because terminally failing every in-flight
// submission over one EIO leaves the teacher re-running them by hand.
//
// Both kinds are already student-safe: the configured path is a server-side
// detail and reaches the log only (SPEC §14).
func LocalSource(path string, log *slog.Logger) (runner.Source, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := checkLocalRoots(path, os.Getenv(LocalRootsEnv)); err != nil {
		log.Error("hidden tests: local path rejected", "path", path, "err", err)
		return nil, fmt.Errorf("%w: configured path is not allowed", ErrConfig)
	}
	st, err := os.Stat(path)
	switch {
	case err == nil && st.IsDir():
		return scrubbed{inner: runner.WorkingCopySource{Root: path}, log: log}, nil
	case err == nil:
		log.Error("hidden tests: local path is not a directory", "path", path)
		return nil, fmt.Errorf("%w: configured path is not a directory", ErrConfig)
	case errors.Is(err, fs.ErrNotExist):
		log.Error("hidden tests: local path is missing", "path", path)
		return nil, fmt.Errorf("%w: configured path is missing", ErrConfig)
	default:
		log.Error("hidden tests: local path unreadable", "path", path, "err", err)
		return nil, errUnavailable
	}
}

// checkLocalRoots reports whether path resolves inside one of the colon-
// separated roots. An empty list allows everything (see LocalRootsEnv).
func checkLocalRoots(path, roots string) error {
	if strings.TrimSpace(roots) == "" {
		return nil
	}
	abs := resolve(path)
	for root := range strings.SplitSeq(roots, ":") {
		if root = strings.TrimSpace(root); root == "" {
			continue
		}
		rel, err := filepath.Rel(resolve(root), abs)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return nil
	}
	return fmt.Errorf("path is outside %s", LocalRootsEnv)
}

// resolve makes a path absolute and follows symlinks where it can, so that
// neither `..` nor a link planted inside an allowed root can widen it. A path
// that does not exist is compared as written.
func resolve(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// scrubbed keeps a source's own error text - which carries absolute host paths
// - out of submissions.worker_note, which students read (SPEC §14). The detail
// goes to the server log instead.
type scrubbed struct {
	inner runner.Source
	log   *slog.Logger
}

func (s scrubbed) Export(ctx context.Context, srcRel, dstAbs string) error {
	if err := s.inner.Export(ctx, srcRel, dstAbs); err != nil {
		s.log.Error("hidden tests: export failed", "path", srcRel, "err", err)
		return errUnavailable
	}
	return nil
}

// Open scrubs the whole stream, not just its opening: a source backed by git
// reports the failure that matters - a bad object, an unreadable cache - from
// Read or Close, long after Open returned nil, and that error travels the same
// route into worker_note.
func (s scrubbed) Open(ctx context.Context, srcRel string) (io.ReadCloser, bool, error) {
	rc, ok, err := s.inner.Open(ctx, srcRel)
	if err != nil {
		s.log.Error("hidden tests: read failed", "path", srcRel, "err", err)
		return nil, false, errUnavailable
	}
	if !ok {
		return nil, false, nil
	}
	return scrubbedReader{inner: rc, path: srcRel, log: s.log}, true, nil
}

// scrubbedReader applies the same substitution to a live stream. io.EOF passes
// through untouched: it is the normal end of a read, not a failure.
type scrubbedReader struct {
	inner io.ReadCloser
	path  string
	log   *slog.Logger
}

func (r scrubbedReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.log.Error("hidden tests: read failed", "path", r.path, "err", err)
		return n, errUnavailable
	}
	return n, err
}

func (r scrubbedReader) Close() error {
	if err := r.inner.Close(); err != nil {
		r.log.Error("hidden tests: read failed", "path", r.path, "err", err)
		return errUnavailable
	}
	return nil
}
