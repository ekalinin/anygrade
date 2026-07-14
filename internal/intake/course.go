// Package intake is the server side of the receive-hook protocol: it turns
// pushes into submissions (SPEC §6) and validates teacher course updates
// (SPEC §7). It may import store/queue/config; gitserver stays free of them.
package intake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/gitserver"
)

// Course is one loaded snapshot of the course metadata, pinned to the mirror
// commit it was read from. ResolvedTask.Dir points into a checkout that is
// deleted after loading; use relDirs instead of Dir.
type Course struct {
	Resolved *config.Resolved
	Head     string            // mirror commit the metadata was loaded from
	relDirs  map[string]string // task id -> repo-relative dir (slash-separated)
}

// Task returns a task and its repo-relative dir by id.
func (c *Course) Task(id string) (config.ResolvedTask, string, bool) {
	for _, t := range c.Resolved.Tasks {
		if t.ID == id {
			return t, c.relDirs[id], true
		}
	}
	return config.ResolvedTask{}, "", false
}

// DetectTasks maps changed repo-relative paths to task ids, in course order.
func (c *Course) DetectTasks(paths []string) []string {
	var ids []string
	for _, t := range c.Resolved.Tasks {
		rel := c.relDirs[t.ID]
		for _, p := range paths {
			if p == rel || len(p) > len(rel) && p[:len(rel)] == rel && p[len(rel)] == '/' {
				ids = append(ids, t.ID)
				break
			}
		}
	}
	return ids
}

// Holder is the atomically swappable current course: workers read it while a
// teacher push replaces it.
type Holder struct {
	p atomic.Pointer[Course]
}

func (h *Holder) Get() *Course  { return h.p.Load() }
func (h *Holder) Set(c *Course) { h.p.Store(c) }

// LoadCourse loads and validates metadata from the mirror's current head
// (SPEC §12: the course repo head is the single source of truth).
func LoadCourse(ctx context.Context, courseDir string) (*Course, []config.Diagnostic, error) {
	head, err := gitserver.Git(ctx, courseDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, nil, err
	}
	return LoadCourseAt(ctx, courseDir, head, nil)
}

// LoadCourseAt loads metadata from an arbitrary commit; env may carry the
// quarantine object dirs so a not-yet-accepted teacher push is readable.
func LoadCourseAt(ctx context.Context, courseDir, commit string, env []string) (*Course, []config.Diagnostic, error) {
	tmp, err := os.MkdirTemp("", "anygrade-course-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)

	src := gitserver.GitSource{Dir: courseDir, Commit: commit, Env: env}
	if err := src.Export(ctx, "", tmp); err != nil {
		return nil, nil, fmt.Errorf("export course tree: %w", err)
	}
	resolved, diags, err := config.LoadAll(tmp)
	if err != nil {
		return nil, nil, err
	}
	diags = append(diags, config.Validate(resolved)...)
	if config.HasErrors(diags) {
		return nil, diags, nil
	}

	relDirs := make(map[string]string, len(resolved.Tasks))
	for _, t := range resolved.Tasks {
		rel, err := filepath.Rel(tmp, t.Dir)
		if err != nil {
			return nil, diags, err
		}
		relDirs[t.ID] = filepath.ToSlash(rel)
	}
	return &Course{Resolved: resolved, Head: commit, relDirs: relDirs}, diags, nil
}
