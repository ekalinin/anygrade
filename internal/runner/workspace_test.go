package runner

import (
	"os"
	"path/filepath"
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
