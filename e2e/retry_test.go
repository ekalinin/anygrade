//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The retry schedule this scenario's server is started on. Sub-second, so the
// whole budget is spent inside one e2e run: on the shipped 10s/5m/8 the same
// five retries would take over five minutes, which is why nothing covered this
// path until the schedule became a `serve` flag (SPEC §11, §13).
const (
	retryBackoff    = "200ms"
	retryBackoffCap = "500ms"
	retryMax        = "5"
)

// retryMaxAttempts is the flaky task's attempt budget. It only has to be more
// than one: what the task page is read for is whether the retries were each
// charged as an attempt of their own.
const retryMaxAttempts = 3

// retryCourseYAML is the course of this scenario: open registration, the local
// runner, one task. Everything the shared fixture carries for its own
// scenarios is left out - this course exists to fail on infrastructure.
const retryCourseYAML = `name: "E2E retry course"
tasks_dir: "tasks"

registration:
  mode: open
  course_code: "e2e-retry-code"

scoring:
  policy: best

defaults:
  runner:
    type: local
    timeout: 60s
`

// retryTaskYAMLTmpl points hidden_tests at a repo that does not exist, so every
// attempt to assemble the workspace fails before a single check runs. That is
// infrastructure and not a wrong answer (SPEC §13), and it is the cheapest one
// a suite without docker can produce on demand.
const retryTaskYAMLTmpl = `name: "Flaky"
score: 10

solution_files:
  - notes.txt

limits:
  max_attempts: %d

hidden_tests:
  source: git
  url: file://%s
  ref: main
  path: flaky/

checks:
  - name: check
    weight: 1
    run: "true"
`

const flakyReadme = "# Flaky\n\nThe hidden tests for this task are unreachable.\n"
const flakyNotes = "todo\n"

// 36. infra-error retry budget: a submission whose hidden-tests overlay cannot
// be fetched is retried on the schedule `serve` was given, the retries do not
// each cost the student an attempt, and once the budget is spent the row is
// terminal with the reason recorded for both audiences (SPEC §13).
func TestRetryBudget(t *testing.T) {
	e := startRetryEnv(t)
	dir := registerAndClone(t, e, "alice", "e2e-retry-code")

	writeFile(t, filepath.Join(dir, "tasks", "flaky", "notes.txt"), "solve flaky\n")
	git(t, dir, nil, "add", "-A")
	git(t, dir, nil, "commit", "-q", "-m", "solve flaky")
	pushed := time.Now()
	out := git(t, dir, nil, "push", "origin", "main")
	id := taskSubmissionID(t, out, "flaky")

	// The first failure always arms a retry - the budget is only spent after
	// max-retries of them - so the row is `retrying`, not graded and not
	// terminal.
	pollStatus(t, e.aliceClient, e, id, "retrying")

	// Read the task page during the retry loop, then confirm the submission is
	// *still* retrying. The transition retrying -> error only goes one way, so
	// a reading taken before that confirmation was taken while retries were
	// still running, without the test having to guess at timings.
	_, taskPage := get(t, e.aliceClient, e.baseURL+"/tasks/flaky")
	_, subPage := get(t, e.aliceClient, fmt.Sprintf("%s/submissions/%d", e.baseURL, id))
	if !strings.Contains(subPage, ">retrying<") {
		t.Fatalf("submission #%d left the retry loop before its attempt count could be read; widen the schedule", id)
	}
	// One submission in flight holds exactly one attempt slot, however many
	// times it has been retried: it is the same attempt coming back.
	if want := fmt.Sprintf(">1 of %d<", retryMaxAttempts); !strings.Contains(taskPage, want) {
		t.Fatalf("task page while retrying: want attempts %q, so the retries are not charged one by one:\n%s", want, taskPage)
	}

	// Budget spent. The stored status is infra_error with retry_at cleared,
	// which the pages call `error` (SPEC §12).
	page := pollStatus(t, e.aliceClient, e, id, "error")
	// The direct proof that --retry-backoff reached the queue: on the shipped
	// schedule five retries are 10+20+40+80+160 seconds and this line would
	// never be reached.
	if elapsed := time.Since(pushed); elapsed > 30*time.Second {
		t.Errorf("the retry budget took %s to spend: the schedule from the flags did not reach the queue", elapsed)
	}

	// The reason is recorded, and it says the budget is what ran out - a bare
	// "unavailable" would read like a state still being retried. The student
	// gets the scrubbed wording (SPEC §14); a submission with no check results
	// has nothing else to explain itself with.
	const exhausted = "hidden tests temporarily unavailable (retries exhausted)"
	if !strings.Contains(page, exhausted) {
		t.Errorf("terminal submission #%d does not tell the student why it stopped:\n%s", id, page)
	}
	status, queue := get(t, e.profClient, e.baseURL+"/queue")
	if status != http.StatusOK {
		t.Fatalf("GET /queue: status %d", status)
	}
	if !strings.Contains(queue, exhausted) {
		t.Errorf("queue row for submission #%d missing the terminal reason:\n%s", id, queue)
	}

	// And the attempt is given back: a submission that never ran must not cost
	// the student one of the task's attempts (SPEC §13).
	_, taskPage = get(t, e.aliceClient, e.baseURL+"/tasks/flaky")
	if want := fmt.Sprintf(">0 of %d<", retryMaxAttempts); !strings.Contains(taskPage, want) {
		t.Fatalf("task page after the budget ran out: want attempts %q, the slot released:\n%s", want, taskPage)
	}
}

// startRetryEnv is startEnv for this scenario: its own course, data dir, ports
// and server process, started on a sub-second retry schedule. It cannot share
// the suite's server - that one runs on the shipped defaults on purpose, and
// every ordered subtest there depends on it doing so.
func startRetryEnv(t *testing.T) *env {
	t.Helper()
	root := shortTempDir(t)
	courseDir := writeRetryCourseFixture(t, root)
	dataDir := filepath.Join(root, "data")

	out := runBin(t, "", "user", "add", "--login", "prof", "--role", "teacher", "--data-dir", dataDir)
	profToken := reToken.FindString(out)
	if profToken == "" {
		t.Fatalf("no token in `user add` output:\n%s", out)
	}

	httpPort := freePort(t)
	sshPort := freePort(t)
	logPath := filepath.Join(root, "serve.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create serve.log: %v", err)
	}

	e := &env{
		root:       root,
		baseURL:    fmt.Sprintf("http://127.0.0.1:%d", httpPort),
		httpPort:   httpPort,
		sshPort:    sshPort,
		courseDir:  courseDir,
		dataDir:    dataDir,
		profToken:  profToken,
		serverLog:  logFile,
		profClient: newClient(t),
		serveArgs: []string{
			"--retry-backoff", retryBackoff,
			"--retry-backoff-cap", retryBackoffCap,
			"--max-retries", retryMax,
		},
	}
	t.Cleanup(func() {
		stopServer(t, e, syscall.SIGTERM)
		logFile.Close()
		if t.Failed() {
			if b, err := os.ReadFile(logPath); err == nil {
				t.Logf("serve.log:\n%s", b)
			}
		}
	})

	startServer(t, e)
	login(t, e, e.profClient, "prof", profToken)
	return e
}

// writeRetryCourseFixture writes and commits this scenario's course repo,
// returning its absolute path. The hidden-tests URL names a sibling directory
// that is never created, which is what makes every submission fail on
// infrastructure.
func writeRetryCourseFixture(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "course")
	gone := filepath.Join(root, "hidden-never-created")

	writeFile(t, filepath.Join(dir, "course.yaml"), retryCourseYAML)
	writeFile(t, filepath.Join(dir, "tasks", "flaky", "task.yaml"),
		fmt.Sprintf(retryTaskYAMLTmpl, retryMaxAttempts, gone))
	writeFile(t, filepath.Join(dir, "tasks", "flaky", "README.md"), flakyReadme)
	writeFile(t, filepath.Join(dir, "tasks", "flaky", "notes.txt"), flakyNotes)

	git(t, dir, nil, "init", "-q", "-b", "main")
	git(t, dir, nil, "add", ".")
	git(t, dir, nil, "-c", "user.name=e2e", "-c", "user.email=e2e@test", "commit", "-q", "-m", "init")
	return dir
}
