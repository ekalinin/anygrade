package gitserver

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/runner"
)

// newCourseFixture builds a working repo with a task subtree (including an
// executable file) and returns the work dir, a bare clone, and the head SHA.
func newCourseFixture(t *testing.T) (work, bare, head string) {
	t.Helper()
	work = t.TempDir()
	runSrc(t, work, "init", "-b", "main")
	files := map[string]string{
		"go.mod":                      "module course.example/fixture\n",
		"lib/util.sh":                 "util() { echo util; }\n",
		"tasks/01-intro/task.yaml":    "name: Intro\n",
		"tasks/01-intro/main.go":      "package main\n",
		"tasks/01-intro/main_test.go": "package main // tests\n",
		"tasks/01-intro/run.sh":       "#!/bin/sh\necho ok\n",
	}
	for p, content := range files {
		abs := filepath.Join(work, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(work, "tasks", "01-intro", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSrc(t, work, "add", ".")
	runSrc(t, work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "init")
	head = runSrc(t, work, "rev-parse", "HEAD")

	bare = filepath.Join(t.TempDir(), "course.git")
	runSrc(t, work, "clone", "--bare", work, bare)
	return work, bare, head
}

// treeSnapshot maps rel path -> "content|exec" for tree comparison.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		exec := "noexec"
		if info.Mode()&0o100 != 0 {
			exec = "exec"
		}
		snap[filepath.ToSlash(rel)] = string(data) + "|" + exec
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// TestGitSourceExportMatchesWorkingCopy: GitSource and WorkingCopySource must
// produce identical trees for the same commit (design risk #1: tar prefix
// re-rooting).
func TestGitSourceExportMatchesWorkingCopy(t *testing.T) {
	requireGit(t)
	work, bare, head := newCourseFixture(t)
	gs := GitSource{Dir: bare, Commit: head}
	wc := runner.WorkingCopySource{Root: work}

	for _, srcRel := range []string{"tasks/01-intro", "go.mod"} {
		gitDst := filepath.Join(t.TempDir(), "git")
		wcDst := filepath.Join(t.TempDir(), "wc")
		if err := gs.Export(t.Context(), srcRel, gitDst); err != nil {
			t.Fatalf("GitSource.Export(%q): %v", srcRel, err)
		}
		if err := wc.Export(t.Context(), srcRel, wcDst); err != nil {
			t.Fatalf("WorkingCopySource.Export(%q): %v", srcRel, err)
		}
		got, want := treeSnapshot(t, gitDst), treeSnapshot(t, wcDst)
		if len(got) != len(want) {
			t.Fatalf("%q: %d files, want %d (got %v)", srcRel, len(got), len(want), got)
		}
		for p, v := range want {
			if got[p] != v {
				t.Errorf("%q: file %s = %q, want %q", srcRel, p, got[p], v)
			}
		}
	}
}

func TestGitSourceExportMissingPath(t *testing.T) {
	requireGit(t)
	_, bare, head := newCourseFixture(t)
	gs := GitSource{Dir: bare, Commit: head}
	err := gs.Export(t.Context(), "no/such/dir", t.TempDir())
	if err == nil {
		t.Fatal("export of a missing path must fail")
	}
}

func TestGitSourceFile(t *testing.T) {
	requireGit(t)
	_, bare, head := newCourseFixture(t)
	gs := GitSource{Dir: bare, Commit: head}

	data, ok, err := gs.File(t.Context(), "tasks/01-intro/main.go")
	if err != nil || !ok || string(data) != "package main\n" {
		t.Fatalf("File: data=%q ok=%v err=%v", data, ok, err)
	}
	// Missing path: ok=false, no error.
	if _, ok, err := gs.File(t.Context(), "tasks/01-intro/ghost.go"); ok || err != nil {
		t.Fatalf("missing file: ok=%v err=%v", ok, err)
	}
	// A directory is not a solution file: reads as absent.
	if _, ok, err := gs.File(t.Context(), "tasks/01-intro"); ok || err != nil {
		t.Fatalf("dir as file: ok=%v err=%v", ok, err)
	}
	// Broken commit must be an error, not a silent miss.
	bad := GitSource{Dir: bare, Commit: strings.Repeat("0", 40)}
	if _, ok, err := bad.File(t.Context(), "go.mod"); ok || err == nil {
		t.Fatalf("bad commit: ok=%v err=%v", ok, err)
	}
}

// newStudentWork clones the course fixture, applies edit, and commits the
// result, returning the clone and its head SHA.
func newStudentWork(t *testing.T, work string, edit func(dir string)) (dir, head string) {
	t.Helper()
	dir = t.TempDir()
	runSrc(t, work, "clone", work, dir)
	edit(dir)
	runSrc(t, dir, "add", "-A")
	runSrc(t, dir, "-c", "user.name=s", "-c", "user.email=s@s", "commit", "-m", "solve")
	return dir, runSrc(t, dir, "rev-parse", "HEAD")
}

// writeIn writes content to a repo-relative path, creating parent directories.
func writeIn(t *testing.T, dir, rel, content string) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTamperNotes: student edits outside solution_files are reported; edits
// to solution files are not.
func TestTamperNotes(t *testing.T) {
	requireGit(t)
	work, bare, head := newCourseFixture(t)

	student, studentHead := newStudentWork(t, work, func(dir string) {
		writeIn(t, dir, "tasks/01-intro/main.go", "package main // solution\n") // allowed
		writeIn(t, dir, "tasks/01-intro/main_test.go", "package main // hacked\n")
		writeIn(t, dir, "tasks/01-intro/extra.txt", "cheat sheet\n")
		if err := os.Remove(filepath.Join(dir, "tasks", "01-intro", "run.sh")); err != nil {
			t.Fatal(err)
		}
	})

	notes, err := TamperNotes(t.Context(),
		GitSource{Dir: bare, Commit: head},
		GitSource{Dir: student, Commit: studentHead},
		"tasks/01-intro", []string{"main.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"added outside solution_files (ignored): tasks/01-intro/extra.txt",
		"deleted outside solution_files (restored): tasks/01-intro/run.sh",
		"modified outside solution_files (restored): tasks/01-intro/main_test.go",
	}
	if !slices.Equal(notes, want) {
		t.Errorf("notes = %v, want %v", notes, want)
	}

	// An untouched student tree yields no notes.
	clean, err := TamperNotes(t.Context(),
		GitSource{Dir: bare, Commit: head},
		GitSource{Dir: bare, Commit: head},
		"tasks/01-intro", []string{"main.go"}, []string{"go.mod", "lib"})
	if err != nil {
		t.Fatal(err)
	}
	if len(clean) != 0 {
		t.Errorf("clean tree notes = %v, want none", clean)
	}
}

// TestTamperNotesIncludePaths: workspace.include paths live outside the task
// dir, yet assembly restores them from the course repo exactly like task-dir
// files - so tampering with them must be reported too (SPEC §6.1).
func TestTamperNotesIncludePaths(t *testing.T) {
	requireGit(t)
	work, bare, head := newCourseFixture(t)

	student, studentHead := newStudentWork(t, work, func(dir string) {
		writeIn(t, dir, "go.mod", "module course.example/hacked\n")
		writeIn(t, dir, "lib/extra.sh", "echo cheat\n")
		if err := os.Remove(filepath.Join(dir, "lib", "util.sh")); err != nil {
			t.Fatal(err)
		}
	})

	notes, err := TamperNotes(t.Context(),
		GitSource{Dir: bare, Commit: head},
		GitSource{Dir: student, Commit: studentHead},
		"tasks/01-intro", []string{"main.go"}, []string{"go.mod", "lib"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"added outside solution_files (ignored): lib/extra.sh",
		"deleted outside solution_files (restored): lib/util.sh",
		"modified outside solution_files (restored): go.mod",
	}
	if !slices.Equal(notes, want) {
		t.Errorf("notes = %v, want %v", notes, want)
	}
}

// TestTamperNotesMissingSolutionFile: a solution file the student deleted is
// silently replaced by the authoritative template (SPEC §6.1 allows it), so
// the teacher has to be told the graded code was not the student's.
func TestTamperNotesMissingSolutionFile(t *testing.T) {
	requireGit(t)
	work, bare, head := newCourseFixture(t)

	student, studentHead := newStudentWork(t, work, func(dir string) {
		if err := os.Remove(filepath.Join(dir, "tasks", "01-intro", "main.go")); err != nil {
			t.Fatal(err)
		}
	})

	notes, err := TamperNotes(t.Context(),
		GitSource{Dir: bare, Commit: head},
		GitSource{Dir: student, Commit: studentHead},
		"tasks/01-intro", []string{"main.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"solution file missing in the submitted commit (template used): tasks/01-intro/main.go",
	}
	if !slices.Equal(notes, want) {
		t.Errorf("notes = %v, want %v", notes, want)
	}
}
