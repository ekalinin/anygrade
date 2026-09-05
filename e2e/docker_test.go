//go:build e2e && docker

// This file is the docker half of the e2e suite: it grades real pushes in real
// containers, so it needs a reachable docker daemon (colima on macOS). It sits
// behind its own build tag on purpose - `make e2e` must keep running with git
// alone - and is built by `make e2e-docker` (`-tags 'e2e docker'`).
package e2e

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// dockerImage is the runner image of the fixture course: the smallest image
// with a POSIX shell, the same one the runner unit tests use.
const dockerImage = "alpine:3"

// dockerCourseYAMLTmpl is the course.yaml of the docker suite. Everything the
// default fixture needs for its own scenarios (hidden tests, penalties,
// workspace includes) is left out: this course exists to prove that the
// submission flow ends inside a container, not to re-cover grading policy.
const dockerCourseYAMLTmpl = `name: "E2E docker course"
tasks_dir: "tasks"

registration:
  mode: open
  course_code: "e2e-docker-code"

scoring:
  policy: best

defaults:
  runner:
    type: docker
    image: "%s"
    timeout: 60s
`

// dockerSumTaskYAML grades the same sum.sh the local suite does, in front of a
// required gate that can only pass inside the sandbox: /work is a tmpfs and the
// checks do not run as root (SPEC §14). A run that quietly fell back to the
// host would fail that gate and score nothing, so the score alone is proof.
const dockerSumTaskYAML = `name: "Sum (docker)"
score: 100

solution_files:
  - sum.sh

checks:
  - name: sandbox
    required: true
    weight: 0
    run: grep -E "^[^ ]+ /work tmpfs " /proc/mounts && echo "uid=$(id -u)" && test "$(id -u)" != 0
  - name: basic
    weight: 60
    run: test "$(sh sum.sh 2 3)" = "5"
  - name: advanced
    weight: 40
    run: test "$(sh sum.sh 0 0)" = "0"
`

// dockerTimeoutTaskYAML times one check out inside the container. On the host a
// timeout only kills a process; here it destroys the container, because killing
// the `docker exec` client leaves the command running inside it. The checks
// that follow therefore need a rebuilt container seeded from the workspace the
// dead one carried - which is what `after` proves by reading a file `hang`
// wrote into the tmpfs before it hung.
const dockerTimeoutTaskYAML = `name: "Timeout (docker)"
score: 20

solution_files:
  - notes.txt

runner:
  timeout: 5s

checks:
  - name: hang
    weight: 1
    run: echo survived > carried.txt && sleep 300
  - name: after
    weight: 1
    run: test "$(cat carried.txt)" = "survived"
`

const dockerTimeoutReadme = "# Timeout\n\nOne check hangs inside the container.\n"
const dockerTimeoutNotes = "todo\n"

// dockerGreetTaskYAMLTmpl is the two-phase task of SPEC §6.1 with the docker
// runner under it. The boundary is applied differently here than on the host:
// the workspace the run phase sees is the container's tmpfs, so the hidden
// sources are removed by taking the container down and seeding a fresh one from
// what is left. The `hidden` check asserts both halves of that from the inside -
// the build artifact came across, the hidden source did not.
const dockerGreetTaskYAMLTmpl = `name: "Greet (docker)"
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
    build: sh hidden_check.sh && echo ok > "$ANYGRADE_ARTIFACTS/greet.ok"
    run: test -f "$ANYGRADE_ARTIFACTS/greet.ok" && test ! -e hidden_check.sh
`

// 34. docker runner end to end: a course whose runner is docker grades a real
// push in an ephemeral container - the whole submission flow of the local
// suite, with the sandbox of SPEC §14 in the middle instead of `sh -c` on the
// host.
func TestDockerGrading(t *testing.T) {
	requireDockerDaemon(t)
	requireDockerImage(t)

	e := startDockerEnv(t)
	dir := registerAndClone(t, e, "alice", "e2e-docker-code")

	writeFile(t, filepath.Join(dir, "tasks", "sum", "sum.sh"), sumSolution)
	git(t, dir, nil, "add", "-A")
	git(t, dir, nil, "commit", "-q", "-m", "solve sum")
	out := git(t, dir, nil, "push", "origin", "main")
	id := taskSubmissionID(t, out, "sum")

	pollSubmission(t, e.aliceClient, e, id)
	if got := fetchScores(t, e)["alice"]["sum"]; got != "100" {
		t.Fatalf("alice/sum graded in docker: got %q, want 100", got)
	}

	// The gate's own log is where the sandbox becomes visible rather than
	// inferred: /work is a tmpfs there and nowhere on the host, and the uid is
	// not root's. Read as the teacher - log downloads are teacher-only, whatever
	// phase they belong to (SPEC §14).
	status, body := get(t, e.profClient, fmt.Sprintf("%s/submissions/%d/logs/sandbox", e.baseURL, id))
	if status != http.StatusOK {
		t.Fatalf("GET the sandbox check log: status %d", status)
	}
	if !strings.Contains(body, "/work tmpfs") {
		t.Fatalf("the sandbox log does not show the tmpfs workspace:\n%s", body)
	}
	if !strings.Contains(body, "uid=") || strings.Contains(body, "uid=0") {
		t.Fatalf("student code must not run as root (SPEC §14):\n%s", body)
	}
}

// 35. docker daemon unreachable: with DOCKER_HOST pointed at a socket nothing
// listens on, no check can run at all. That is infrastructure, not a failed
// solution (SPEC §13): the submission is retried instead of graded, nothing
// lands in the gradebook, and the daemon's error reaches the teacher.
func TestDockerDaemonUnreachable(t *testing.T) {
	// A daemon must be reachable for the test process too, otherwise the dead
	// socket would not be the reason the run failed.
	requireDockerDaemon(t)

	dead := filepath.Join(shortTempDir(t), "dead.sock")
	e := startDockerEnv(t, "DOCKER_HOST=unix://"+dead)
	dir := registerAndClone(t, e, "alice", "e2e-docker-code")

	writeFile(t, filepath.Join(dir, "tasks", "sum", "sum.sh"), sumSolution)
	git(t, dir, nil, "add", "-A")
	git(t, dir, nil, "commit", "-q", "-m", "solve sum with no daemon")
	out := git(t, dir, nil, "push", "origin", "main")
	id := taskSubmissionID(t, out, "sum")

	// The stored status is infra_error; the page shows `retrying` while a retry
	// is still armed (subDisplayStatus), and the first failure always is - the
	// row only turns terminal after MaxRetries.
	page := pollStatus(t, e.aliceClient, e, id, "retrying")
	// The daemon error is an operator's business: the student's page carries
	// neither it nor the socket it names (today it carries no explanation at
	// all - a submission with no check results renders no worker_note).
	if strings.Contains(page, dead) || strings.Contains(page, "image_pull") {
		t.Fatalf("the student page leaks the daemon failure:\n%s", page)
	}
	if got := fetchScores(t, e)["alice"]["sum"]; got != "" && got != "0" {
		t.Fatalf("alice/sum with no daemon: got %q, want no recorded score", got)
	}

	// The reason reaches the teacher's queue row - the surface that renders
	// worker_note without check results. The op is `image_pull` because the
	// image lookup is the runner's first contact with the daemon.
	status, queue := get(t, e.profClient, e.baseURL+"/queue")
	if status != http.StatusOK {
		t.Fatalf("GET /queue: status %d", status)
	}
	if !strings.Contains(queue, "infra error (image_pull)") {
		t.Fatalf("queue row for submission #%d does not name the daemon failure:\n%s", id, queue)
	}
}

// 36. check timeout in a container: the timeout of SPEC §13 costs the container
// rather than a process, so the checks after it run in a rebuilt one. The task
// still scores what it earned, and the workspace the dead container held is
// what the new one is seeded from.
func TestDockerCheckTimeout(t *testing.T) {
	requireDockerDaemon(t)
	requireDockerImage(t)

	e := startDockerEnv(t)
	dir := registerAndClone(t, e, "alice", "e2e-docker-code")

	writeFile(t, filepath.Join(dir, "tasks", "timeout", "notes.txt"), "go\n")
	git(t, dir, nil, "add", "-A")
	git(t, dir, nil, "commit", "-q", "-m", "trigger the timeout task")
	out := git(t, dir, nil, "push", "origin", "main")
	id := taskSubmissionID(t, out, "timeout")

	body := pollSubmission(t, e.aliceClient, e, id)
	if !strings.Contains(body, "timed out after 5s") {
		t.Errorf("submission page missing the timeout note:\n%s", body)
	}
	// A non-gate timeout stops nothing: `after` ran, in a container that did not
	// exist when `hang` started.
	if strings.Contains(body, "st-skipped") {
		t.Errorf("the check after a non-gate timeout was skipped:\n%s", body)
	}
	// hang failed, after passed: half the weight, half the score. `after` can
	// only pass if the file hang wrote into the tmpfs came out with the dying
	// container and went back into the new one.
	if got := fetchScores(t, e)["alice"]["timeout"]; got != "10" {
		t.Fatalf("alice/timeout: got %q, want 10 (the check after the timeout passed)", got)
	}
}

// 37. two-phase check in a container: the build/run boundary of SPEC §6.1 with
// the docker runner under it. The container is replaced between the phases, so
// this is where "the hidden sources left the workspace" and "the build
// artifacts survived" are two different claims about two different containers.
func TestDockerBuildPhase(t *testing.T) {
	requireDockerDaemon(t)
	requireDockerImage(t)

	e := startDockerEnv(t)
	dir := registerAndClone(t, e, "alice", "e2e-docker-code")

	writeFile(t, filepath.Join(dir, "tasks", "greet", "greet.sh"), greetSolution)
	git(t, dir, nil, "add", "-A")
	git(t, dir, nil, "commit", "-q", "-m", "solve greet")
	out := git(t, dir, nil, "push", "origin", "main")
	id := taskSubmissionID(t, out, "greet")

	body := pollSubmission(t, e.aliceClient, e, id)
	// The `hidden` check passes only when both halves held inside the second
	// container: $ANYGRADE_ARTIFACTS/greet.ok came across, hidden_check.sh did
	// not. A full score is that assertion, made from inside the sandbox.
	if got := fetchScores(t, e)["alice"]["greet"]; got != "50" {
		t.Fatalf("alice/greet graded in docker: got %q, want 50", got)
	}
	if strings.Contains(body, hiddenSecret) || strings.Contains(body, "phase=build") {
		t.Fatalf("the student page leaks the build phase:\n%s", body)
	}

	// The build phase is the one that read the hidden tests, and its log is
	// teacher-only whatever the runner was (SPEC §14).
	logURL := fmt.Sprintf("%s/submissions/%d/logs/hidden", e.baseURL, id)
	status, log := get(t, e.profClient, logURL+"?phase=build")
	if status != http.StatusOK {
		t.Fatalf("teacher GET build log: status %d, body:\n%s", status, log)
	}
	if !strings.Contains(log, hiddenSecret) {
		t.Fatalf("build log does not carry the hidden test output:\n%s", log)
	}
	if status, _ := get(t, e.aliceClient, logURL+"?phase=build"); status != http.StatusNotFound {
		t.Fatalf("the student read the build log: status %d", status)
	}
	if status, log = get(t, e.profClient, logURL); status != http.StatusOK {
		t.Fatalf("teacher GET run log: status %d", status)
	}
	if strings.Contains(log, hiddenSecret) {
		t.Fatalf("the run phase saw the hidden tests:\n%s", log)
	}
}

// requireDockerDaemon fails - rather than skips - when no daemon is reachable.
// The unit tests skip because they run inside `go test ./...`, where a daemon
// is optional; here the `docker` build tag is the operator's own statement that
// one is running, so its absence is the finding.
func requireDockerDaemon(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		t.Fatalf("no docker daemon reachable (macOS: colima start): %v", err)
	}
}

// requireDockerImage puts the runner image on the host before any submission
// starts. A cold pull can outlast a submission poll deadline, and what these
// scenarios are about is grading in a container, not the pull - which the
// runner's own tests cover. Only fetched when missing, so an offline host with
// the image already cached still runs the suite.
func requireDockerImage(t *testing.T) {
	t.Helper()
	if exec.Command("docker", "image", "inspect", dockerImage).Run() == nil {
		return
	}
	if out, err := exec.Command("docker", "pull", dockerImage).CombinedOutput(); err != nil {
		t.Fatalf("docker pull %s: %v\n%s", dockerImage, err, out)
	}
}

// startDockerEnv is startEnv for this suite: its own course (docker runner, one
// task), its own data dir, ports and server process, so it shares nothing with
// the default suite's server. serverEnv is appended to the server's
// environment - the unreachable-daemon scenario points DOCKER_HOST at a socket
// nothing listens on, and that has to reach the worker, not the test.
//
// It starts the server itself rather than through startServer: the default
// suite's version passes no environment, and giving it one would change the
// process every docker-free scenario runs against.
func startDockerEnv(t *testing.T, serverEnv ...string) *env {
	t.Helper()
	root := shortTempDir(t)
	courseDir := writeDockerCourseFixture(t, root)
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

	cmd := exec.Command(bin, "serve",
		"--repo", courseDir,
		"--data-dir", dataDir,
		"--http-addr", fmt.Sprintf("127.0.0.1:%d", httpPort),
		"--ssh-addr", fmt.Sprintf("127.0.0.1:%d", sshPort),
		"--workers", "2",
	)
	cmd.Env = append(os.Environ(), serverEnv...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	e.serverCmd = cmd

	waitReady(t, e.baseURL)
	login(t, e, e.profClient, "prof", profToken)
	return e
}

// writeDockerCourseFixture writes and commits the docker suite's course repo,
// returning its absolute path. The task templates and solutions are the local
// suite's fixtures; only the course and task metadata differ. Each scenario
// pushes its own task, so the three share nothing but the server.
func writeDockerCourseFixture(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "course")
	writeFile(t, filepath.Join(dir, "course.yaml"), fmt.Sprintf(dockerCourseYAMLTmpl, dockerImage))
	writeFile(t, filepath.Join(dir, "tasks", "sum", "task.yaml"), dockerSumTaskYAML)
	writeFile(t, filepath.Join(dir, "tasks", "sum", "README.md"), sumReadme)
	writeFile(t, filepath.Join(dir, "tasks", "sum", "sum.sh"), sumBroken)

	writeFile(t, filepath.Join(dir, "tasks", "timeout", "task.yaml"), dockerTimeoutTaskYAML)
	writeFile(t, filepath.Join(dir, "tasks", "timeout", "README.md"), dockerTimeoutReadme)
	writeFile(t, filepath.Join(dir, "tasks", "timeout", "notes.txt"), dockerTimeoutNotes)

	greetTaskYAML := fmt.Sprintf(dockerGreetTaskYAMLTmpl, writeHiddenFixture(t, root))
	writeFile(t, filepath.Join(dir, "tasks", "greet", "task.yaml"), greetTaskYAML)
	writeFile(t, filepath.Join(dir, "tasks", "greet", "README.md"), greetReadme)
	writeFile(t, filepath.Join(dir, "tasks", "greet", "greet.sh"), greetBroken)

	git(t, dir, nil, "init", "-q", "-b", "main")
	git(t, dir, nil, "add", ".")
	git(t, dir, nil, "-c", "user.name=e2e", "-c", "user.email=e2e@test", "commit", "-q", "-m", "init")
	return dir
}
