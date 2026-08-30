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
  opens: 2020-01-01T00:00:00+00:00
  closes: 2100-01-01T00:00:00+00:00
  max_accounts: 1

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
  # Two phases (SPEC §6.1): the build phase is the only one that sees the
  # hidden sources, the run phase executes what it left behind. Shell is
  # interpreted, so a real shell course gains nothing from this - it is here
  # because it is the smallest thing that exercises the boundary end to end.
  - name: hidden
    weight: 1
    build: sh hidden_check.sh && echo ok > "$ANYGRADE_ARTIFACTS/greet.ok"
    run: test -f "$ANYGRADE_ARTIFACTS/greet.ok" && test ! -e hidden_check.sh
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

// slowTaskYAML is a task whose check runs long enough for a scenario to catch
// the submission in `running` and act on it (restart, cancel).
const slowTaskYAML = `name: "Slow"
score: 10

solution_files:
  - notes.txt

checks:
  - name: slow
    weight: 1
    run: sleep 3
`

const slowReadme = "# Slow\n\nA check that takes a few seconds.\n"
const slowNotes = "todo\n"

// timeoutTaskYAML has a non-gate check that outlives the task's runner timeout,
// plus a following check that must still run (SPEC §13).
const timeoutTaskYAML = `name: "Timeout"
score: 20

solution_files:
  - notes.txt

runner:
  timeout: 2s

checks:
  - name: hang
    weight: 1
    run: sleep 30
  - name: after
    weight: 1
    run: "true"
`

const timeoutReadme = "# Timeout\n\nOne check hangs.\n"
const timeoutNotes = "todo\n"

// softTaskYAML is late by years with the hard deadline still ahead, so it is
// always accepted and always carries the capped penalty from the course
// defaults (10% per 24h, capped at 50%) - a fixed expectation no matter when
// the suite runs (SPEC §9).
const softTaskYAML = `name: "Soft"
score: 100

solution_files:
  - notes.txt

deadline:
  soft: 2020-01-01T00:00:00+03:00
  hard: 2999-01-01T00:00:00+03:00

checks:
  - name: check
    weight: 1
    run: "true"
`

const softReadme = "# Soft\n\nSoft deadline long past, hard deadline far ahead.\n"
const softNotes = "todo\n"

// limitedTaskYAML caps attempts at two so a third push is rejected without
// running (SPEC §4.3).
const limitedTaskYAML = `name: "Limited"
score: 10

solution_files:
  - notes.txt

limits:
  max_attempts: 2

checks:
  - name: check
    weight: 1
    run: "true"
`

const limitedReadme = "# Limited\n\nTwo attempts.\n"
const limitedNotes = "todo\n"

// hiddenSecret is printed by the hidden test and by nothing else, so finding
// it in a response is proof that hidden-test output reached that reader.
const hiddenSecret = "hidden-marker-e2e-secret"

// hiddenCheckSh is the hidden-tests overlay for the greet task.
const hiddenCheckSh = `echo "` + hiddenSecret + `"
test "$(sh greet.sh)" = "hello"
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

	writeFile(t, filepath.Join(dir, "tasks", "slow", "task.yaml"), slowTaskYAML)
	writeFile(t, filepath.Join(dir, "tasks", "slow", "README.md"), slowReadme)
	writeFile(t, filepath.Join(dir, "tasks", "slow", "notes.txt"), slowNotes)

	writeFile(t, filepath.Join(dir, "tasks", "timeout", "task.yaml"), timeoutTaskYAML)
	writeFile(t, filepath.Join(dir, "tasks", "timeout", "README.md"), timeoutReadme)
	writeFile(t, filepath.Join(dir, "tasks", "timeout", "notes.txt"), timeoutNotes)

	writeFile(t, filepath.Join(dir, "tasks", "soft", "task.yaml"), softTaskYAML)
	writeFile(t, filepath.Join(dir, "tasks", "soft", "README.md"), softReadme)
	writeFile(t, filepath.Join(dir, "tasks", "soft", "notes.txt"), softNotes)

	writeFile(t, filepath.Join(dir, "tasks", "limited", "task.yaml"), limitedTaskYAML)
	writeFile(t, filepath.Join(dir, "tasks", "limited", "README.md"), limitedReadme)
	writeFile(t, filepath.Join(dir, "tasks", "limited", "notes.txt"), limitedNotes)

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
