package gitserver

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// exportDirMode keeps an exported tree owner-only: it lands in the data dir
// (check workspaces) or in a temp dir, and nothing outside anygrade reads it.
const exportDirMode = 0o700

// GitSource implements runner.Source over a bare repo pinned to one commit:
// the server-side counterpart of runner.WorkingCopySource (SPEC §6.1).
type GitSource struct {
	Dir    string // bare repo path
	Commit string // pinned commit SHA (or ref)
	// Env holds extra environment (GIT_OBJECT_DIRECTORY etc.) so quarantined
	// objects of an in-flight push are readable during course validation.
	Env []string
}

// Export implements runner.Source: it streams `git archive` and re-roots the
// subtree at srcRel under dstAbs. Archive entry names are prefixed with
// srcRel, so the prefix is stripped to match WorkingCopySource semantics
// ("copy the contents of srcRel into dstAbs").
func (s GitSource) Export(ctx context.Context, srcRel, dstAbs string) error {
	args := []string{"-C", s.Dir, "archive", "--format=tar", s.Commit}
	if srcRel != "" {
		args = append(args, "--", srcRel)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), s.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	untarErr := untarTo(tar.NewReader(stdout), srcRel, dstAbs)
	if untarErr != nil {
		// Drain so git does not block on a full pipe before Wait.
		_, _ = io.Copy(io.Discard, stdout)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive %s -- %q: %w: %s",
			s.Commit, srcRel, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return untarErr
}

// untarTo extracts entries, stripping the srcRel prefix. A lone entry equal
// to srcRel is a single-file export: it lands at dstAbs itself.
func untarTo(tr *tar.Reader, srcRel, dstAbs string) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		name := path.Clean(hdr.Name)
		var rel string
		switch {
		case srcRel == "":
			rel = name
		case name == srcRel:
			rel = "."
		case strings.HasPrefix(name, srcRel+"/"):
			rel = name[len(srcRel)+1:]
		default:
			continue // pax headers and other out-of-subtree noise
		}
		// Tree entries come from pushed (untrusted) objects; git does not
		// fsck-reject ".." components by default, so refuse them here.
		if rel != "." && !filepath.IsLocal(filepath.FromSlash(rel)) {
			return fmt.Errorf("archive entry escapes destination: %q", hdr.Name)
		}
		target := dstAbs
		if rel != "." {
			target = filepath.Join(dstAbs, filepath.FromSlash(rel))
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, exportDirMode); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), exportDirMode); err != nil {
				return err
			}
			// O_NOFOLLOW because no entry may ever redirect a write through a
			// link: nothing here creates one, so this can only fire on a race.
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW,
				hdr.FileInfo().Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Never restored: a link in the exported tree is what every later
			// write resolves through - the solution-file overlay above all - and
			// an absolute one points straight out of the workspace. Dropping the
			// entry rather than failing the export keeps a stray link in some
			// unrelated corner of the course repo from taking the whole course
			// down; where it matters (a solution_file or a workspace.include
			// path) it shows up as a missing file, which validate reports.
			slog.Warn("skipped a symlink in a git export: the workspace must be a plain tree",
				"entry", hdr.Name)
		}
	}
}

// Open implements runner.Source: it streams `git cat-file blob`, so a blob is
// never buffered whole. The pushed pack is bounded compressed, which says
// nothing about the size it unpacks to; the caller stops reading at its own
// limit and closes, which kills git.
//
// The type probe runs first, and it is what keeps the semantics of File: a
// missing path and a directory both read as absent, a broken repo or commit is
// an error. Probing before the stream also means a missing blob can never
// truncate the authoritative file the overlay is about to replace.
func (s GitSource) Open(ctx context.Context, srcRel string) (io.ReadCloser, bool, error) {
	typ, err := s.output(ctx, "cat-file", "-t", s.Commit+":"+srcRel)
	if err != nil {
		if _, probeErr := s.output(ctx, "cat-file", "-e", s.Commit+"^{commit}"); probeErr != nil {
			return nil, false, probeErr
		}
		return nil, false, nil
	}
	if string(bytes.TrimSpace(typ)) != "blob" {
		return nil, false, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, "git", "-C", s.Dir, "cat-file", "blob", s.Commit+":"+srcRel)
	cmd.Env = append(os.Environ(), s.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, false, err
	}
	return &blobReader{cmd: cmd, stdout: stdout, stderr: &stderr, cancel: cancel}, true, nil
}

// blobReader is the live `git cat-file blob` of GitSource.Open.
type blobReader struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr *bytes.Buffer
	cancel context.CancelFunc
	eof    bool
}

func (b *blobReader) Read(p []byte) (int, error) {
	n, err := b.stdout.Read(p)
	if err == io.EOF {
		b.eof = true
	}
	return n, err
}

// Close reaps git. A caller that stopped early (size limit) leaves git blocked
// on a pipe nobody drains, so it is killed and its status ignored; only a
// stream read to the end can say whether git actually produced the blob.
func (b *blobReader) Close() error {
	if !b.eof {
		b.cancel()
	}
	err := b.cmd.Wait()
	b.cancel()
	if !b.eof || err == nil {
		return nil
	}
	return fmt.Errorf("git cat-file blob: %w: %s", err, bytes.TrimSpace(b.stderr.Bytes()))
}

// File reads one blob whole. `cat-file blob` also rejects trees, so a path
// that is a directory reads as absent.
func (s GitSource) File(ctx context.Context, srcRel string) ([]byte, bool, error) {
	data, err := s.output(ctx, "cat-file", "blob", s.Commit+":"+srcRel)
	if err == nil {
		return data, true, nil
	}
	// Missing path and broken repo/commit both exit 128: probe the commit to
	// tell them apart, otherwise a repo-level failure would silently grade
	// the template instead of the student's code.
	if _, probeErr := s.output(ctx, "cat-file", "-e", s.Commit+"^{commit}"); probeErr != nil {
		return nil, false, probeErr
	}
	return nil, false, nil
}

// output runs one git command in the source repo and returns raw stdout.
func (s GitSource) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", s.Dir}, args...)...)
	cmd.Env = append(os.Environ(), s.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

// List returns the sorted repo-relative file paths under dir at the pinned
// commit (the teacher code view's allowlist).
func (s GitSource) List(ctx context.Context, dir string) ([]string, error) {
	blobs, err := s.lsTree(ctx, dir)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(blobs))
	for p := range blobs {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths, nil
}

// lsTree maps repo-relative paths under the given dirs (files are allowed too)
// to blob SHAs at the pinned commit. Paths absent from the commit contribute
// nothing, they are not an error.
func (s GitSource) lsTree(ctx context.Context, dirs ...string) (map[string]string, error) {
	out, err := s.output(ctx, append([]string{"ls-tree", "-r", s.Commit, "--"}, dirs...)...)
	if err != nil {
		return nil, err
	}
	blobs := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		// "<mode> <type> <sha>\t<path>"
		meta, p, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" {
			continue
		}
		blobs[p] = fields[2]
	}
	return blobs, nil
}

// TamperNotes lists the files the student changed outside solution_files
// (SPEC §6.1): workspace assembly silently restores the authoritative
// versions, and the differences are surfaced to the teacher. The scan covers
// the task dir and the workspace.include paths, which are restored the same
// way even though they live outside the task dir. A solution file missing
// from the submitted commit gets its own note: assembly keeps the
// authoritative template, so the graded code is not the student's. Both sides
// are read from their own repo, so no cross-repo git plumbing is needed.
func TamperNotes(ctx context.Context, authoritative, student GitSource, taskRelDir string, solutionFiles, include []string) ([]string, error) {
	scan := append([]string{taskRelDir}, include...)
	course, err := authoritative.lsTree(ctx, scan...)
	if err != nil {
		return nil, err
	}
	mine, err := student.lsTree(ctx, scan...)
	if err != nil {
		return nil, err
	}
	solution := map[string]bool{}
	for _, sf := range solutionFiles {
		solution[path.Join(taskRelDir, sf)] = true
	}

	var notes []string
	for p, sha := range mine {
		if solution[p] {
			continue
		}
		want, exists := course[p]
		switch {
		case !exists:
			notes = append(notes, fmt.Sprintf("added outside solution_files (ignored): %s", p))
		case want != sha:
			notes = append(notes, fmt.Sprintf("modified outside solution_files (restored): %s", p))
		}
	}
	for p := range course {
		if mine[p] != "" {
			continue
		}
		if solution[p] {
			notes = append(notes, fmt.Sprintf(
				"solution file missing in the submitted commit (template used): %s", p))
			continue
		}
		notes = append(notes, fmt.Sprintf("deleted outside solution_files (restored): %s", p))
	}
	slices.Sort(notes)
	return notes, nil
}
