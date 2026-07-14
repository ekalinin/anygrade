package gitserver

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

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
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
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
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}

// File implements runner.Source. `cat-file blob` also rejects trees, so a
// solution-file path that is a directory in the student repo reads as absent
// and the authoritative version stays.
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

// lsTree maps repo-relative paths under dir to blob SHAs at the pinned commit.
func (s GitSource) lsTree(ctx context.Context, dir string) (map[string]string, error) {
	out, err := s.output(ctx, "ls-tree", "-r", s.Commit, "--", dir)
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

// TamperNotes lists task-dir files the student changed outside solution_files
// (SPEC §6.1): workspace assembly silently restores the authoritative
// versions, and the differences are surfaced to the teacher. Both sides are
// read from their own repo, so no cross-repo git plumbing is needed.
func TamperNotes(ctx context.Context, authoritative, student GitSource, taskRelDir string, solutionFiles []string) ([]string, error) {
	course, err := authoritative.lsTree(ctx, taskRelDir)
	if err != nil {
		return nil, err
	}
	mine, err := student.lsTree(ctx, taskRelDir)
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
		if !solution[p] && mine[p] == "" {
			notes = append(notes, fmt.Sprintf("deleted outside solution_files (restored): %s", p))
		}
	}
	slices.Sort(notes)
	return notes, nil
}
