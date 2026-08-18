package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ekalinin/anygrade/internal/config"
)

// Source yields file subtrees for workspace assembly (SPEC §6.1). M2 ships
// WorkingCopySource (checked-out repo, used by `anygrade check`); M3/M4 add a
// bare-git source reading a commit via `git archive`/`git cat-file`.
type Source interface {
	// Export copies the subtree (or single file) at srcRel (source-relative,
	// "" = source root) into dstAbs, preserving structure and file modes.
	// Existing destination files are overwritten (overlay semantics).
	// Symlinks are refused: a workspace is a plain tree.
	Export(ctx context.Context, srcRel, dstAbs string) error
	// Open returns a reader over one source-relative file; ok=false if the file
	// does not exist or is not a regular file. It is a stream, not a []byte,
	// because the content comes from an untrusted commit: the caller bounds how
	// much of it ever reaches memory or disk.
	Open(ctx context.Context, srcRel string) (rc io.ReadCloser, ok bool, err error)
}

// WorkingCopySource reads files from a directory tree on disk: a checked-out
// course repo, or a local hidden-tests directory.
type WorkingCopySource struct {
	Root string
}

// Export implements Source.
func (s WorkingCopySource) Export(ctx context.Context, srcRel, dstAbs string) error {
	src := filepath.Join(s.Root, filepath.FromSlash(srcRel))
	return copyTree(ctx, src, dstAbs, false)
}

// Open implements Source.
func (s WorkingCopySource) Open(_ context.Context, srcRel string) (io.ReadCloser, bool, error) {
	f, err := os.Open(filepath.Join(s.Root, filepath.FromSlash(srcRel)))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, false, err
	}
	if !st.Mode().IsRegular() {
		// A directory is not a solution file, matching GitSource, where
		// `cat-file blob` rejects trees.
		f.Close()
		return nil, false, nil
	}
	return f, true, nil
}

// TamperError marks a workspace that cannot be assembled because the submitted
// content breaks the workspace contract: a solution path that is or crosses a
// symlink, or an overlay larger than the configured bounds. The cause is the
// student's commit, not the environment, so the submission fails terminally
// with this text as the note instead of being retried as an infra fault.
type TamperError struct {
	Msg string
}

func (e *TamperError) Error() string { return e.Msg }

func tamperErr(format string, args ...any) *TamperError {
	return &TamperError{Msg: fmt.Sprintf(format, args...)}
}

// errOverLimit is the internal signal that a copy hit its byte budget; the
// caller turns it into the TamperError that names the limit.
var errOverLimit = errors.New("size limit exceeded")

// workspaceDirMode keeps every assembled directory owner-only, matching the
// 0700 data dir it lives in.
const workspaceDirMode = 0o700

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
	// RunAsUID/RunAsGID: chown the workspace after assembly so the container
	// user owns the files copied into its tmpfs /work. -1 = leave ownership,
	// which is what both call sites do: the docker runner runs as the uid that
	// assembled the workspace. Only an anygrade running as root would need it.
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
// then the student's solution files, then hidden tests (SPEC §6.1). Returned
// errors are *InfraError, except for *TamperError - the submitted content
// itself is at fault and retrying it cannot help.
func Assemble(ctx context.Context, a Assembly) (*Workspace, error) {
	ws, err := assemble(ctx, a)
	if err != nil {
		// A half-built workspace has no owner - nothing will ever call Close on
		// it - and a student can make assembly fail on demand (oversized
		// overlay), so it must not pile up in the data dir.
		os.RemoveAll(a.Dest)
	}
	return ws, err
}

func assemble(ctx context.Context, a Assembly) (*Workspace, error) {
	// The whole workspace is owner-only: it holds hidden tests and a student's
	// code, and only anygrade (or the container user it hands the tree to)
	// needs to reach it. Nothing is bind-mounted - the docker runner copies the
	// tree into the container - so the modes here bind host accounts only.
	if err := os.MkdirAll(a.Dest, workspaceDirMode); err != nil {
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
		if err := overlayStudent(ctx, a); err != nil {
			return nil, err
		}
	}

	// 3. Hidden tests overlay the task dir.
	if a.Hidden != nil {
		if err := overlayHidden(ctx, a, taskDst); err != nil {
			return nil, err
		}
	}

	if a.RunAsUID >= 0 {
		if err := chownTree(a.Dest, a.RunAsUID, a.RunAsGID); err != nil {
			return nil, infraErr("workspace", err)
		}
	}
	return &Workspace{Root: a.Dest, TaskDir: taskDst}, nil
}

// overlayStudent copies the declared solution_files from the student's commit
// over the authoritative task dir (SPEC §6.1 step 2).
//
// Two properties matter here, because both inputs - the path layout and the
// blob - are attacker-controlled. Every write goes through an os.Root anchored
// at the workspace and every path component is checked for symlinks, so a link
// planted anywhere along a solution path is refused instead of followed (which
// would write student bytes outside the workspace, as the anygrade process,
// before docker is even started). And every blob is streamed under a per-file
// and a whole-overlay budget: the push limit bounds the compressed pack only,
// so a highly compressible object passes it and expands afterwards.
func overlayStudent(ctx context.Context, a Assembly) error {
	root, err := os.OpenRoot(a.Dest)
	if err != nil {
		return infraErr("workspace", err)
	}
	defer root.Close()

	maxFile, maxTotal := overlayLimits(a.Task.Workspace)
	var total int64
	for _, sf := range a.Task.SolutionFiles {
		rel := path.Join(a.TaskRelDir, sf)
		rc, ok, err := a.Student.Open(ctx, rel)
		if err != nil {
			return infraErr("workspace", fmt.Errorf("read solution file %q: %w", sf, err))
		}
		if !ok {
			continue // deleted by the student; authoritative version stays
		}
		budget, overTotal := maxFile, false
		if rem := maxTotal - total; rem < budget {
			budget, overTotal = rem, true
		}
		n, err := writeNoFollow(root, rel, rc, budget)
		closeErr := rc.Close()
		total += n
		switch {
		case errors.Is(err, errOverLimit) && overTotal:
			return tamperErr("solution files exceed the %d byte workspace overlay limit", maxTotal)
		case errors.Is(err, errOverLimit):
			return tamperErr("solution file %q exceeds the %d byte limit", sf, maxFile)
		case err != nil:
			if _, ok := errors.AsType[*TamperError](err); ok {
				return err
			}
			return infraErr("workspace", fmt.Errorf("write solution file %q: %w", sf, err))
		case closeErr != nil:
			// Never grade a half-read blob as if it were the student's file.
			return infraErr("workspace", fmt.Errorf("read solution file %q: %w", sf, closeErr))
		}
	}
	return nil
}

// overlayLimits falls back to the built-in bounds, so a workspace assembled
// without a resolved config (tests, older callers) is still bounded.
func overlayLimits(w config.ResolvedWorkspace) (maxFile, maxTotal int64) {
	maxFile, maxTotal = w.MaxFileSize, w.MaxTotalSize
	if maxFile <= 0 {
		maxFile = config.DefaultOverlayFile
	}
	if maxTotal <= 0 {
		maxTotal = config.DefaultOverlayTotal
	}
	return maxFile, maxTotal
}

// writeNoFollow writes at most limit bytes of r to the root-relative path rel,
// creating parent directories. It returns errOverLimit if the source had more
// to give, and a *TamperError if any component of rel is a symlink or is not
// the kind of file it has to be. os.Root already refuses to leave the
// workspace; the explicit component checks turn a symlink inside it into a
// clear refusal instead of a write through it.
func writeNoFollow(root *os.Root, rel string, r io.Reader, limit int64) (int64, error) {
	name := filepath.FromSlash(rel)
	if !filepath.IsLocal(name) {
		return 0, tamperErr("solution path %q escapes the workspace", rel)
	}
	if err := mkdirAllNoFollow(root, filepath.Dir(name)); err != nil {
		return 0, err
	}
	switch fi, err := root.Lstat(name); {
	case err == nil && fi.Mode()&fs.ModeSymlink != 0:
		return 0, tamperErr("solution path %q is a symlink", rel)
	case err == nil && !fi.Mode().IsRegular():
		return 0, tamperErr("solution path %q is not a regular file", rel)
	case err != nil && !os.IsNotExist(err):
		return 0, err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return 0, err
	}
	// One byte over the budget is enough to tell "fits" from "too big" without
	// ever holding, or storing, the rest of an oversized blob.
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, errOverLimit
	}
	return n, nil
}

// mkdirAllNoFollow creates dir inside root, refusing to descend through a
// symlink even though os.Root would keep the result inside the workspace.
func mkdirAllNoFollow(root *os.Root, dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	var walked string
	for part := range strings.SplitSeq(dir, string(filepath.Separator)) {
		walked = filepath.Join(walked, part)
		fi, err := root.Lstat(walked)
		switch {
		case os.IsNotExist(err):
			if err := root.Mkdir(walked, workspaceDirMode); err != nil {
				return err
			}
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink != 0:
			return tamperErr("solution path component %q is a symlink", filepath.ToSlash(walked))
		case !fi.IsDir():
			return tamperErr("solution path component %q is not a directory", filepath.ToSlash(walked))
		}
	}
	return nil
}

// overlayHidden stages the hidden tests next to the workspace and then copies
// them into the task dir read-only (SPEC §6.1 step 3). The staging step is what
// makes the mode possible: it is the only way to tell hidden files apart from
// the task files they land among.
//
// Read-only is not an isolation boundary - the checks run as the owner of these
// files and can chmod them back - but it does stop a check from rewriting the
// tests the next check runs against without going out of its way.
func overlayHidden(ctx context.Context, a Assembly, taskDst string) error {
	stage := a.Dest + ".hidden"
	defer os.RemoveAll(stage)
	if err := os.MkdirAll(stage, workspaceDirMode); err != nil {
		return infraErr("workspace", err)
	}
	if err := a.Hidden.Export(ctx, "", stage); err != nil {
		return infraErr("workspace", fmt.Errorf("overlay hidden tests: %w", err))
	}
	if err := copyTree(ctx, stage, taskDst, true); err != nil {
		return infraErr("workspace", fmt.Errorf("overlay hidden tests: %w", err))
	}
	return nil
}

// copyTree copies a file or directory tree, overwriting existing destination
// files. readOnly strips the write bits from every copied file (execute and
// read are preserved, so a hidden-test script still runs).
//
// Symlinks are skipped, never followed and never recreated, exactly like the
// git export in gitserver - the two sources have to produce the same tree. A
// workspace is a plain tree: a link in it is what a later write resolves
// through, and following one here would copy in whatever the host has at the
// other end.
func copyTree(ctx context.Context, src, dst string, readOnly bool) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		skippedSymlink(src)
		return nil
	}
	if !info.IsDir() {
		return copyFile(src, dst, fileMode(info.Mode(), readOnly))
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
			return os.MkdirAll(target, workspaceDirMode)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			skippedSymlink(p)
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(p, target, fileMode(fi.Mode(), readOnly))
	})
}

// skippedSymlink records what assembly left out, so a course that relies on a
// link finds out from the server log rather than from a mystery build failure.
func skippedSymlink(p string) {
	slog.Warn("skipped a symlink during workspace assembly: the workspace must be a plain tree",
		"path", filepath.Base(p))
}

// fileMode drops the write bits when the destination must not be writable.
func fileMode(src fs.FileMode, readOnly bool) fs.FileMode {
	if readOnly {
		return src &^ 0o222
	}
	return src
}

func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), workspaceDirMode); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// O_NOFOLLOW: a destination that is already a symlink must fail the copy,
	// not redirect it. Nothing in an assembled workspace creates one, so this
	// only ever fires on a bug or a race.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// The create mode only applies to a file that did not exist yet; an overlay
	// lands on one that does.
	return os.Chmod(dst, mode.Perm())
}

func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, uid, gid)
	})
}
