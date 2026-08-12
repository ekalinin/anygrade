//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// courseYAML is the course.yaml fixture used by every scenario.
const courseYAML = `name: "E2E course"
tasks_dir: "tasks"

registration:
  mode: open
  course_code: "e2e-code"

scoring:
  policy: best

leaderboard:
  enabled: true
  anonymize: false

defaults:
  runner:
    type: local
    timeout: 60s
  deadline:
    penalty: {percent: 10, per: 24h, max_percent: 50}
  workspace:
    include:
      - common.sh
`

// commonSh is the course-root shared file pulled into every workspace through
// workspace.include (the shell course's analogue of a root go.mod).
const commonSh = "# shared helpers\ncommon_ok() { return 0; }\n"

const sumTaskYAML = `name: "Sum"
score: 100

solution_files:
  - sum.sh

checks:
  - name: common
    required: true
    weight: 0
    run: sh -n ../../common.sh
  - name: build
    required: true
    weight: 0
    run: sh -n sum.sh
  - name: basic
    weight: 60
    run: test "$(sh sum.sh 2 3)" = "5"
  - name: advanced
    weight: 40
    run: test "$(sh sum.sh 0 0)" = "0"
`

const sumReadme = "# Sum\n\nPrint the sum of two arguments.\n"

// sumBroken is the task template committed to the course repo (intentionally
// failing so scenarios can push a fix).
const sumBroken = "#!/bin/sh\necho TODO\n"

// sumSolution is the correct solution used by scenarios.
const sumSolution = "#!/bin/sh\necho $(( $1 + $2 ))\n"

const greetTaskYAMLTmpl = `name: "Greet"
score: 50

solution_files:
  - greet.sh

hidden_tests:
  source: git
  url: file://%s
  ref: main
  path: greet/

checks:
  - name: open
    weight: 1
    run: test "$(sh greet.sh)" = "hello"
  - name: hidden
    weight: 1
    run: sh hidden_check.sh
`

const greetReadme = "# Greet\n\nPrint hello.\n"
const greetBroken = "#!/bin/sh\necho TODO\n"
const greetSolution = "#!/bin/sh\necho hello\n"

const lateTaskYAML = `name: "Late"
score: 10

solution_files:
  - notes.txt

deadline:
  soft: 2020-01-01T00:00:00+03:00
  hard: 2020-01-02T00:00:00+03:00

checks:
  - name: check
    weight: 1
    run: "true"
`

const lateReadme = "# Late\n\nDeadline passed long ago.\n"
const lateNotes = "todo\n"

// hiddenCheckSh is the hidden-tests overlay for the greet task.
const hiddenCheckSh = `test "$(sh greet.sh)" = "hello"
`

// writeHiddenFixture writes and commits the hidden-tests repo, returning its
// absolute path (used to build the greet task's file:// hidden_tests URL).
func writeHiddenFixture(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "hidden")
	writeFile(t, filepath.Join(dir, "greet", "hidden_check.sh"), hiddenCheckSh)
	git(t, dir, nil, "init", "-q", "-b", "main")
	git(t, dir, nil, "add", ".")
	git(t, dir, nil, "-c", "user.name=e2e", "-c", "user.email=e2e@test", "commit", "-q", "-m", "init")
	return dir
}

// writeCourseFixture writes and commits the course repo, returning its
// absolute path. hiddenDir must already exist (it is templated into the
// greet task's hidden_tests URL).
func writeCourseFixture(t *testing.T, root, hiddenDir string) string {
	t.Helper()
	dir := filepath.Join(root, "course")
	writeFile(t, filepath.Join(dir, "course.yaml"), courseYAML)
	writeFile(t, filepath.Join(dir, "common.sh"), commonSh)

	writeFile(t, filepath.Join(dir, "tasks", "sum", "task.yaml"), sumTaskYAML)
	writeFile(t, filepath.Join(dir, "tasks", "sum", "README.md"), sumReadme)
	writeFile(t, filepath.Join(dir, "tasks", "sum", "sum.sh"), sumBroken)

	greetTaskYAML := fmt.Sprintf(greetTaskYAMLTmpl, hiddenDir)
	writeFile(t, filepath.Join(dir, "tasks", "greet", "task.yaml"), greetTaskYAML)
	writeFile(t, filepath.Join(dir, "tasks", "greet", "README.md"), greetReadme)
	writeFile(t, filepath.Join(dir, "tasks", "greet", "greet.sh"), greetBroken)

	writeFile(t, filepath.Join(dir, "tasks", "late", "task.yaml"), lateTaskYAML)
	writeFile(t, filepath.Join(dir, "tasks", "late", "README.md"), lateReadme)
	writeFile(t, filepath.Join(dir, "tasks", "late", "notes.txt"), lateNotes)

	git(t, dir, nil, "init", "-q", "-b", "main")
	git(t, dir, nil, "add", ".")
	git(t, dir, nil, "-c", "user.name=e2e", "-c", "user.email=e2e@test", "commit", "-q", "-m", "init")
	return dir
}

// writeFile creates path (and its parent directories) with the given content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
