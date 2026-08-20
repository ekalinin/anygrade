package runner

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/config"
)

// writeFiles creates a file tree from relative path -> content.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestAssembleCheckMode(t *testing.T) {
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{
		"go.mod":                 "module course.example/x\n",
		"tasks/01/task.yaml":     "id: 01\n",
		"tasks/01/main.go":       "package main\n",
		"tasks/01/sub/helper.go": "package sub\n",
		"tasks/02/main.go":       "package main // other task, must NOT be exported\n",
		"secret/hidden_test.go":  "package hidden\n",
	})

	dest := filepath.Join(t.TempDir(), "ws")
	ws, err := Assemble(t.Context(), Assembly{
		Dest:          dest,
		Task:          config.ResolvedTask{SolutionFiles: []string{"main.go"}},
		TaskRelDir:    "tasks/01",
		Include:       []string{"go.mod"},
		Authoritative: WorkingCopySource{Root: repo},
		RunAsUID:      -1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if ws.TaskDir != filepath.Join(dest, "tasks", "01") {
		t.Errorf("TaskDir: %s", ws.TaskDir)
	}
	// The workspace holds hidden tests and a student's code: owner-only, like
	// the data dir around it. Nothing is bind-mounted, so the container user is
	// unaffected (the tree is copied to it, uid and all).
	for _, dir := range []string{dest, ws.TaskDir} {
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %v, want 0700", dir, perm)
		}
	}
	// Task dir and include exported, structure mirrored.
	readFile(t, filepath.Join(dest, "go.mod"))
	readFile(t, filepath.Join(ws.TaskDir, "main.go"))
	readFile(t, filepath.Join(ws.TaskDir, "sub", "helper.go"))
	// Other tasks are NOT exported.
	if _, err := os.Stat(filepath.Join(dest, "tasks", "02")); !os.IsNotExist(err) {
		t.Error("tasks/02 must not be exported")
	}

	if err := ws.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("Close must remove the workspace")
	}
}

func TestAssembleStudentOverlayAndHidden(t *testing.T) {
	course := t.TempDir()
	writeFiles(t, course, map[string]string{
		"tasks/01/main.go":      "package main // template\n",
		"tasks/01/main_test.go": "package main // authoritative test\n",
		"tasks/01/task.yaml":    "id: 01\n",
	})
	student := t.TempDir()
	writeFiles(t, student, map[string]string{
		"tasks/01/main.go":      "package main // student solution\n",
		"tasks/01/main_test.go": "package main // student tampered with the test\n",
	})
	hidden := t.TempDir()
	writeFiles(t, hidden, map[string]string{
		"hidden_test.go": "package main // hidden\n",
	})

	dest := filepath.Join(t.TempDir(), "ws")
	ws, err := Assemble(t.Context(), Assembly{
		Dest:          dest,
		Task:          config.ResolvedTask{SolutionFiles: []string{"main.go"}},
		TaskRelDir:    "tasks/01",
		Authoritative: WorkingCopySource{Root: course},
		Student:       WorkingCopySource{Root: student},
		Hidden:        WorkingCopySource{Root: hidden},
		RunAsUID:      -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	// Solution file comes from the student.
	if got := readFile(t, filepath.Join(ws.TaskDir, "main.go")); got != "package main // student solution\n" {
		t.Errorf("main.go not overlaid from student: %q", got)
	}
	// Non-solution file stays authoritative (anti-cheat, SPEC §6.1).
	if got := readFile(t, filepath.Join(ws.TaskDir, "main_test.go")); got != "package main // authoritative test\n" {
		t.Errorf("main_test.go must stay authoritative: %q", got)
	}
	// Hidden tests overlaid into the task dir.
	readFile(t, filepath.Join(ws.TaskDir, "hidden_test.go"))
}

// TestAssembleHiddenTestsAreReadOnly: hidden tests share the workspace and the
// uid with the student's code, so the modes are all the runner can arrange -
// enough to stop a check from rewriting the tests the next check runs against.
// The executable bit survives, hidden checks are often scripts.
func TestAssembleHiddenTestsAreReadOnly(t *testing.T) {
	course := t.TempDir()
	writeFiles(t, course, map[string]string{"tasks/01/main.go": "package main\n"})
	hidden := t.TempDir()
	writeFiles(t, hidden, map[string]string{
		"hidden_test.go": "package main // hidden\n",
		"run.sh":         "#!/bin/sh\necho ok\n",
	})
	if err := os.Chmod(filepath.Join(hidden, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "ws")
	ws, err := Assemble(t.Context(), Assembly{
		Dest:          dest,
		Task:          config.ResolvedTask{SolutionFiles: []string{"main.go"}},
		TaskRelDir:    "tasks/01",
		Authoritative: WorkingCopySource{Root: course},
		Hidden:        WorkingCopySource{Root: hidden},
		RunAsUID:      -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	for name, wantExec := range map[string]bool{"hidden_test.go": false, "run.sh": true} {
		st, err := os.Stat(filepath.Join(ws.TaskDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s is writable: %v", name, st.Mode())
		}
		if got := st.Mode().Perm()&0o100 != 0; got != wantExec {
			t.Errorf("%s exec bit = %v, want %v", name, got, wantExec)
		}
	}
	// The student's own file stays writable: checks build in the task dir.
	st, err := os.Stat(filepath.Join(ws.TaskDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("task files must stay writable: %v", st.Mode())
	}
	// The staging copy of the hidden tests does not outlive assembly.
	if _, err := os.Stat(dest + ".hidden"); !os.IsNotExist(err) {
		t.Errorf("hidden staging dir left behind (err=%v)", err)
	}
}

// TestAssembleRefusesSymlinkedSolutionPath: writing a solution file through a
// symlink would put student-controlled bytes outside the workspace, as the
// anygrade process, before docker is ever started. Both shapes are covered:
// the solution file itself is a link, and a directory above it is.
func TestAssembleRefusesSymlinkedSolutionPath(t *testing.T) {
	student := t.TempDir()
	writeFiles(t, student, map[string]string{
		"tasks/01/main.go":     "package main // student\n",
		"tasks/01/sub/main.go": "package main // student\n",
	})

	for _, tc := range []struct {
		name     string
		solution string
		plant    func(t *testing.T, dest, outside string)
	}{
		{
			name:     "solution file is a symlink",
			solution: "main.go",
			plant: func(t *testing.T, dest, outside string) {
				link := filepath.Join(dest, "tasks", "01", "main.go")
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "victim.txt"), link); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "parent directory is a symlink",
			solution: "sub/main.go",
			plant: func(t *testing.T, dest, outside string) {
				link := filepath.Join(dest, "tasks", "01", "sub")
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, link); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			course := t.TempDir()
			writeFiles(t, course, map[string]string{"tasks/01/task.yaml": "id: 01\n"})
			outside := t.TempDir()
			writeFiles(t, outside, map[string]string{"victim.txt": "host file\n"})

			dest := filepath.Join(t.TempDir(), "ws")
			tc.plant(t, dest, outside)

			_, err := Assemble(t.Context(), Assembly{
				Dest:          dest,
				Task:          config.ResolvedTask{SolutionFiles: []string{tc.solution}},
				TaskRelDir:    "tasks/01",
				Authoritative: WorkingCopySource{Root: course},
				Student:       WorkingCopySource{Root: student},
				RunAsUID:      -1,
			})
			if _, ok := errors.AsType[*TamperError](err); !ok {
				t.Fatalf("want a TamperError, got %v", err)
			}
			if got := readFile(t, filepath.Join(outside, "victim.txt")); got != "host file\n" {
				t.Errorf("a file outside the workspace was written: %q", got)
			}
		})
	}
}

// TestAssembleSkipsSymlinkInSource: a link in the source tree is neither
// followed nor recreated - the workspace must be a plain tree, so that no
// later write can resolve through it - while the rest of the task still
// assembles.
func TestAssembleSkipsSymlinkInSource(t *testing.T) {
	course := t.TempDir()
	writeFiles(t, course, map[string]string{"tasks/01/main.go": "package main\n"})
	if err := os.Symlink("/etc/passwd", filepath.Join(course, "tasks", "01", "link")); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "ws")
	ws, err := Assemble(t.Context(), Assembly{
		Dest:          dest,
		Task:          config.ResolvedTask{SolutionFiles: []string{"main.go"}},
		TaskRelDir:    "tasks/01",
		Authoritative: WorkingCopySource{Root: course},
		RunAsUID:      -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if _, err := os.Lstat(filepath.Join(ws.TaskDir, "link")); !os.IsNotExist(err) {
		t.Errorf("the symlink was materialized (err=%v)", err)
	}
	readFile(t, filepath.Join(ws.TaskDir, "main.go"))
}

// TestAssembleFailureRemovesWorkspace: a rejected assembly must not leave its
// half-built tree in the data dir - nothing would ever clean it up, and a
// student can trigger the failure at will.
func TestAssembleFailureRemovesWorkspace(t *testing.T) {
	course := t.TempDir()
	writeFiles(t, course, map[string]string{"tasks/01/main.go": "package main\n"})
	student := t.TempDir()
	writeFiles(t, student, map[string]string{"tasks/01/main.go": strings.Repeat("x", 100)})

	dest := filepath.Join(t.TempDir(), "ws")
	_, err := Assemble(t.Context(), Assembly{
		Dest: dest,
		Task: config.ResolvedTask{
			SolutionFiles: []string{"main.go"},
			Workspace:     config.ResolvedWorkspace{MaxFileSize: 10, MaxTotalSize: 10},
		},
		TaskRelDir:    "tasks/01",
		Authoritative: WorkingCopySource{Root: course},
		Student:       WorkingCopySource{Root: student},
		RunAsUID:      -1,
	})
	if _, ok := errors.AsType[*TamperError](err); !ok {
		t.Fatalf("want a TamperError, got %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("workspace left behind (err=%v)", err)
	}
}

// TestAssembleOverlayLimits: the push limit bounds the compressed pack only, so
// the decompressed overlay is bounded here - per file and in total. Both are
// the student's doing, hence a TamperError (terminal), not an infra failure.
func TestAssembleOverlayLimits(t *testing.T) {
	course := t.TempDir()
	writeFiles(t, course, map[string]string{
		"tasks/01/a.txt": "template\n",
		"tasks/01/b.txt": "template\n",
	})
	student := t.TempDir()
	writeFiles(t, student, map[string]string{
		"tasks/01/a.txt": strings.Repeat("a", 100),
		"tasks/01/b.txt": strings.Repeat("b", 100),
	})

	assemble := func(t *testing.T, w config.ResolvedWorkspace) (*Workspace, error) {
		t.Helper()
		return Assemble(t.Context(), Assembly{
			Dest: filepath.Join(t.TempDir(), "ws"),
			Task: config.ResolvedTask{
				SolutionFiles: []string{"a.txt", "b.txt"},
				Workspace:     w,
			},
			TaskRelDir:    "tasks/01",
			Authoritative: WorkingCopySource{Root: course},
			Student:       WorkingCopySource{Root: student},
			RunAsUID:      -1,
		})
	}

	if _, err := assemble(t, config.ResolvedWorkspace{MaxFileSize: 50, MaxTotalSize: 1000}); err == nil ||
		!strings.Contains(err.Error(), `solution file "a.txt" exceeds`) {
		t.Fatalf("per-file limit: %v", err)
	}
	if _, err := assemble(t, config.ResolvedWorkspace{MaxFileSize: 150, MaxTotalSize: 150}); err == nil ||
		!strings.Contains(err.Error(), "overlay limit") {
		t.Fatalf("total limit: %v", err)
	}
	ws, err := assemble(t, config.ResolvedWorkspace{MaxFileSize: 100, MaxTotalSize: 200})
	if err != nil {
		t.Fatalf("an overlay exactly at the limit must be accepted: %v", err)
	}
	defer ws.Close()
	if got := readFile(t, filepath.Join(ws.TaskDir, "b.txt")); got != strings.Repeat("b", 100) {
		t.Errorf("b.txt = %q", got)
	}
}

// TestAssembleRecordsHiddenPaths: the boundary removes what the hidden overlay
// wrote, so assembly has to report it exactly - by path, never by a glob over
// the task dir, which cannot tell a hidden test from an open one.
func TestAssembleRecordsHiddenPaths(t *testing.T) {
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{
		"tasks/01/main.go":      "package main\n",
		"tasks/01/main_test.go": "package main // open test, stays\n",
	})
	hidden := t.TempDir()
	writeFiles(t, hidden, map[string]string{
		"hidden_test.go":         "package main\n",
		"cases/extra_test.go":    "package main\n",
		"cases/data/fixture.txt": "1\n",
	})

	ws, err := Assemble(t.Context(), Assembly{
		Dest:          filepath.Join(t.TempDir(), "ws"),
		Task:          config.ResolvedTask{SolutionFiles: []string{"main.go"}},
		TaskRelDir:    "tasks/01",
		Authoritative: WorkingCopySource{Root: repo},
		Hidden:        WorkingCopySource{Root: hidden},
		RunAsUID:      -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	wantFiles := []string{
		"tasks/01/cases/data/fixture.txt",
		"tasks/01/cases/extra_test.go",
		"tasks/01/hidden_test.go",
	}
	got := slices.Clone(ws.HiddenPaths)
	slices.Sort(got)
	if !slices.Equal(got, wantFiles) {
		t.Errorf("HiddenPaths: got %v, want %v", got, wantFiles)
	}
	wantDirs := []string{"tasks/01/cases", "tasks/01/cases/data"}
	gotDirs := slices.Clone(ws.HiddenDirs)
	slices.Sort(gotDirs)
	if !slices.Equal(gotDirs, wantDirs) {
		t.Errorf("HiddenDirs: got %v, want %v", gotDirs, wantDirs)
	}

	// And removing them leaves the task's own files alone - including the open
	// test, which a glob for "*_test.go" would have taken with it.
	job := Job{WorkspaceDir: ws.Root, HiddenPaths: ws.HiddenPaths, HiddenDirs: ws.HiddenDirs}
	if err := dropHiddenTests(job); err != nil {
		t.Fatal(err)
	}
	for _, rel := range append(wantFiles, wantDirs...) {
		if _, err := os.Lstat(filepath.Join(ws.Root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s survived the boundary: %v", rel, err)
		}
	}
	for _, rel := range []string{"tasks/01/main.go", "tasks/01/main_test.go"} {
		if _, err := os.Stat(filepath.Join(ws.Root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s must not be removed: %v", rel, err)
		}
	}
}

// TestDropHiddenTestsRefusesSymlink: the boundary writes into a tree a build
// phase has already touched. A symlinked path component would make the removal
// delete somewhere else on the host, as the anygrade process, so it is refused
// - and refusing fails the run, which is the safe direction: the alternative is
// executing student code with the hidden sources still in place.
func TestDropHiddenTestsRefusesSymlink(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"hidden_test.txt": "secret\n"})
	if err := os.Symlink(outside, filepath.Join(ws, "cases")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := dropHiddenTests(Job{WorkspaceDir: ws, HiddenPaths: []string{"cases/hidden_test.txt"}})
	if _, ok := errors.AsType[*InfraError](err); !ok {
		t.Fatalf("want an InfraError, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "hidden_test.txt")); err != nil {
		t.Errorf("the removal followed the symlink out of the workspace: %v", err)
	}
}

// TestAssembleArtifactsDirIsNotOverwritable: $ANYGRADE_ARTIFACTS is created
// before the student's overlay, so a solution file claiming that exact path is
// a terminal tamper error naming it - not an infrastructure failure the queue
// would keep retrying.
func TestAssembleArtifactsDirIsNotOverwritable(t *testing.T) {
	repo := t.TempDir()
	writeFiles(t, repo, map[string]string{"main.sh": "echo main\n"})
	student := t.TempDir()
	writeFiles(t, student, map[string]string{artifactsDir: "student content\n"})

	_, err := Assemble(t.Context(), Assembly{
		Dest:          filepath.Join(t.TempDir(), "ws"),
		Task:          config.ResolvedTask{SolutionFiles: []string{artifactsDir}},
		TaskRelDir:    "",
		Authoritative: WorkingCopySource{Root: repo},
		Student:       WorkingCopySource{Root: student},
		RunAsUID:      -1,
	})
	if _, ok := errors.AsType[*TamperError](err); !ok {
		t.Fatalf("want a TamperError, got %v", err)
	}
}
