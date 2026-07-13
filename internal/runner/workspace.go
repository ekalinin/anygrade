package runner

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/ekalinin/anygrade/internal/config"
)

// Source yields file subtrees for workspace assembly (SPEC §6.1). M2 ships
// WorkingCopySource (checked-out repo, used by `anygrade check`); M3/M4 add a
// bare-git source reading a commit via `git archive`/`git cat-file`.
type Source interface {
	// Export copies the subtree (or single file) at srcRel (source-relative,
	// "" = source root) into dstAbs, preserving structure and file modes.
	// Existing destination files are overwritten (overlay semantics).
	Export(ctx context.Context, srcRel, dstAbs string) error
	// File returns the content of one source-relative file; ok=false if the
	// file does not exist.
	File(ctx context.Context, srcRel string) (data []byte, ok bool, err error)
}

// WorkingCopySource reads files from a directory tree on disk: a checked-out
// course repo, or a local hidden-tests directory.
type WorkingCopySource struct {
	Root string
}

// Export implements Source.
func (s WorkingCopySource) Export(ctx context.Context, srcRel, dstAbs string) error {
	src := filepath.Join(s.Root, filepath.FromSlash(srcRel))
	return copyTree(ctx, src, dstAbs)
}

// File implements Source.
func (s WorkingCopySource) File(_ context.Context, srcRel string) ([]byte, bool, error) {
	data, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(srcRel)))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Assembly describes how to build one check workspace (SPEC §6.1).
type Assembly struct {
	Dest       string              // workspace root; created if absent
	Task       config.ResolvedTask //
	TaskRelDir string              // task dir relative to the repo root (slash-separated)
	// Include lists extra repo-relative paths exported alongside the task dir
	// (usually Task.Workspace.Include; e.g. a course-root go.mod).
	Include []string
	// Authoritative provides the task dir, includes, and every non-solution
	// file: the course repo (SPEC §6.1 step 1).
	Authoritative Source
	// Student provides solution_files overlaid on top (step 2). nil = check
	// mode: the authoritative source already is the student's working copy.
	Student Source
	// Hidden provides hidden tests overlaid onto the task dir (step 3).
	// nil = not configured or not available (check mode never fetches).
	Hidden Source
	// RunAsUID/RunAsGID: chown the workspace after assembly so a non-root
	// container can write to it (Linux server path). -1 = leave ownership
	// (check mode; colima's virtiofs forces root:root anyway).
	RunAsUID int
	RunAsGID int
}

// Workspace is an assembled check workspace.
type Workspace struct {
	Root    string // == Assembly.Dest
	TaskDir string // absolute path of the task dir inside the workspace (check cwd)
	// TamperNotes will list student modifications outside solution_files once
	// the server-side git source lands (M4); always empty in check mode.
	TamperNotes []string
}

// Close removes the workspace from disk.
func (w *Workspace) Close() error { return os.RemoveAll(w.Root) }

// Assemble builds a clean workspace: authoritative task dir + includes,
// then the student's solution files, then hidden tests (SPEC §6.1). All
// returned errors are *InfraError.
func Assemble(ctx context.Context, a Assembly) (*Workspace, error) {
	if err := os.MkdirAll(a.Dest, 0o755); err != nil {
		return nil, infraErr("workspace", err)
	}
	taskDst := filepath.Join(a.Dest, filepath.FromSlash(a.TaskRelDir))

	// 1. Authoritative task dir + shared includes.
	if err := a.Authoritative.Export(ctx, a.TaskRelDir, taskDst); err != nil {
		return nil, infraErr("workspace", fmt.Errorf("export task dir: %w", err))
	}
	for _, inc := range a.Include {
		dst := filepath.Join(a.Dest, filepath.FromSlash(inc))
		if err := a.Authoritative.Export(ctx, inc, dst); err != nil {
			return nil, infraErr("workspace", fmt.Errorf("export include %q: %w", inc, err))
		}
	}

	// 2. Student solution files on top.
	if a.Student != nil {
		for _, sf := range a.Task.SolutionFiles {
			data, ok, err := a.Student.File(ctx, path.Join(a.TaskRelDir, sf))
			if err != nil {
				return nil, infraErr("workspace", fmt.Errorf("read solution file %q: %w", sf, err))
			}
			if !ok {
				continue // deleted by the student; authoritative version stays
			}
			dst := filepath.Join(taskDst, filepath.FromSlash(sf))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return nil, infraErr("workspace", err)
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return nil, infraErr("workspace", err)
			}
		}
	}

	// 3. Hidden tests overlay the task dir.
	if a.Hidden != nil {
		if err := a.Hidden.Export(ctx, "", taskDst); err != nil {
			return nil, infraErr("workspace", fmt.Errorf("overlay hidden tests: %w", err))
		}
	}

	if a.RunAsUID >= 0 {
		if err := chownTree(a.Dest, a.RunAsUID, a.RunAsGID); err != nil {
			return nil, infraErr("workspace", err)
		}
	}
	return &Workspace{Root: a.Dest, TaskDir: taskDst}, nil
}

// copyTree copies a file or directory tree, overwriting existing destination
// files. Symlinks are followed (course repos should not rely on them).
func copyTree(ctx context.Context, src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode())
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(p, target, fi.Mode())
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, uid, gid)
	})
}
