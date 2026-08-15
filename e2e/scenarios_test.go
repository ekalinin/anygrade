//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 1. validate: `anygrade validate` accepts the fixture course.
func testValidate(t *testing.T, e *env) {
	out, err := runBinErr("", "validate", "--repo", e.courseDir)
	if err != nil {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK: course is valid") {
		t.Fatalf("validate: missing success line:\n%s", out)
	}
}

// 2. teacher login: the web login form accepts the token from `user add`.
func testTeacherLogin(t *testing.T, e *env) {
	login(t, e, e.profClient, "prof", e.profToken)
}

// 3. open registration: wrong course code is rejected, the right one issues
// a token and prints the SSH clone URL.
func testOpenRegistration(t *testing.T, e *env) {
	e.aliceClient = newClient(t)

	resp, body := postForm(t, e.aliceClient, e.baseURL+"/register", url.Values{
		"login": {"alice"}, "name": {"Alice"}, "course_code": {"wrong"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("register with wrong code: status %d, body:\n%s", resp.StatusCode, body)
	}

	resp, body = postForm(t, e.aliceClient, e.baseURL+"/register", url.Values{
		"login": {"alice"}, "name": {"Alice"}, "course_code": {"e2e-code"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register with correct code: status %d, body:\n%s", resp.StatusCode, body)
	}
	tok := reToken.FindString(body)
	if tok == "" {
		t.Fatalf("register: no token in body:\n%s", body)
	}
	e.aliceToken = tok

	wantSSH := fmt.Sprintf("ssh://git@127.0.0.1:%d/alice/course.git", e.sshPort)
	if !strings.Contains(body, wantSSH) {
		t.Fatalf("register: body missing SSH clone URL %q:\n%s", wantSSH, body)
	}
}

// 4. clone and push over http: alice pushes a correct sum.sh and the push
// hook queues a submission.
func testCloneAndPushHTTP(t *testing.T, e *env) {
	e.aliceDir = filepath.Join(e.root, "alice")
	httpCloneURL := fmt.Sprintf("http://alice:%s@127.0.0.1:%d/git/alice/course.git", e.aliceToken, httpPortOf(t, e))
	git(t, e.root, nil, "clone", httpCloneURL, e.aliceDir)
	setIdentity(t, e.aliceDir)

	writeFile(t, filepath.Join(e.aliceDir, "tasks", "sum", "sum.sh"), sumSolution)
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "solve sum")
	out := git(t, e.aliceDir, nil, "push", "origin", "main")

	// The personal repo is seeded with an intake baseline at provisioning, so
	// the first push diffs against the course template: only the task alice
	// actually changed (sum) is detected, not the untouched greet/late.
	if !strings.Contains(out, "1 task(s) detected") {
		t.Fatalf("first push should detect exactly one task (sum), got:\n%s", out)
	}
	for _, untouched := range []string{"greet", "late"} {
		if regexp.MustCompile(untouched + `\s+submission #\d+ queued`).MatchString(out) {
			t.Fatalf("untouched task %q should not be queued on first push:\n%s", untouched, out)
		}
	}
	e.aliceSumSubID = taskSubmissionID(t, out, "sum")
}

// taskSubmissionID extracts the submission id queued for one specific task
// from git push output.
func taskSubmissionID(t *testing.T, out, taskID string) int {
	t.Helper()
	re := regexp.MustCompile(taskID + `\s+submission #(\d+) queued`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no queued submission for task %q in push output:\n%s", taskID, out)
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse submission id %q: %v", m[1], err)
	}
	return id
}

// httpPortOf recovers the HTTP port from e.baseURL (the fixture never stores
// it separately; sshPort has its own field).
func httpPortOf(t *testing.T, e *env) int {
	t.Helper()
	u, err := url.Parse(e.baseURL)
	if err != nil {
		t.Fatalf("parse baseURL: %v", err)
	}
	port := u.Port()
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		t.Fatalf("parse http port from %q: %v", e.baseURL, err)
	}
	return p
}

// 5. graded done: the submission finishes and the score lands in the CSV
// export.
func testGradedDone(t *testing.T, e *env) {
	pollSubmission(t, e.aliceClient, e, e.aliceSumSubID)
	scores := fetchScores(t, e)
	if got := scores["alice"]["sum"]; got != "100" {
		t.Fatalf("alice/sum after first solve: got %q, want 100", got)
	}
}

// 6. best policy keeps top score: a worse resubmission does not lower the
// recorded score under the "best" scoring policy.
func testBestPolicy(t *testing.T, e *env) {
	writeFile(t, filepath.Join(e.aliceDir, "tasks", "sum", "sum.sh"), "#!/bin/sh\necho nope\n")
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "regress sum")
	out := git(t, e.aliceDir, nil, "push", "origin", "main")
	id := taskSubmissionID(t, out, "sum")

	pollSubmission(t, e.aliceClient, e, id)
	scores := fetchScores(t, e)
	if got := scores["alice"]["sum"]; got != "100" {
		t.Fatalf("alice/sum after regression: got %q, want 100 (best policy)", got)
	}
}

// 7. hard deadline rejects: the push succeeds, but the "late" task is
// rejected (not queued) and stays ungraded.
func testHardDeadline(t *testing.T, e *env) {
	writeFile(t, filepath.Join(e.aliceDir, "tasks", "late", "notes.txt"), "changed\n")
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "touch late")
	out, err := gitErr(e.aliceDir, nil, "push", "origin", "main")
	if err != nil {
		t.Fatalf("push (late task): %v\n%s", err, out)
	}
	if !strings.Contains(out, "rejected") {
		t.Fatalf("push output missing rejection for late task:\n%s", out)
	}
	if reSubmission.MatchString(out) {
		t.Fatalf("push output should not queue the late task:\n%s", out)
	}

	// The rejection is durable: the submission page explains it, not just the
	// push output the student has already scrolled past.
	status, page := get(t, e.aliceClient, e.baseURL+"/tasks/late")
	if status != http.StatusOK {
		t.Fatalf("GET /tasks/late: status %d", status)
	}
	m := reSubmissionLink.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("late task page lists no submission:\n%s", page)
	}
	status, body := get(t, e.aliceClient, e.baseURL+"/submissions/"+m[1])
	if status != http.StatusOK {
		t.Fatalf("GET /submissions/%s: status %d", m[1], status)
	}
	if !strings.Contains(body, "hard deadline passed") {
		t.Fatalf("submission #%s page missing the stored reject reason:\n%s", m[1], body)
	}

	scores := fetchScores(t, e)
	if got := scores["alice"]["late"]; got != "0" && got != "" {
		t.Fatalf("alice/late after hard-deadline push: got %q, want 0 or empty (not graded)", got)
	}
}

// reSubmissionLink matches a submission link on a task page.
var reSubmissionLink = regexp.MustCompile(`/submissions/(\d+)"`)

// 8. recheck marker: an empty commit with a `[recheck <task>]` marker
// re-queues the task without new file changes.
func testRecheckMarker(t *testing.T, e *env) {
	writeFile(t, filepath.Join(e.aliceDir, "tasks", "sum", "sum.sh"), sumSolution)
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "restore sum")
	out := git(t, e.aliceDir, nil, "push", "origin", "main")
	id := taskSubmissionID(t, out, "sum")
	pollSubmission(t, e.aliceClient, e, id)

	git(t, e.aliceDir, nil, "commit", "--allow-empty", "-q", "-m", "[recheck sum]")
	out = git(t, e.aliceDir, nil, "push", "origin", "main")
	if !reSubmission.MatchString(out) {
		t.Fatalf("recheck push did not queue a submission:\n%s", out)
	}
	id = taskSubmissionID(t, out, "sum")
	pollSubmission(t, e.aliceClient, e, id)

	scores := fetchScores(t, e)
	if got := scores["alice"]["sum"]; got != "100" {
		t.Fatalf("alice/sum after recheck: got %q, want 100", got)
	}
}

// 9. invite and ssh transport: bob is invited, activates with an SSH key,
// and pushes over the SSH transport; the greet task's hidden-tests overlay
// (a file:// git repo) is fetched and executed.
func testInviteAndSSH(t *testing.T, e *env) {
	out := runBin(t, "", "user", "invite",
		"--login", "bob", "--name", "Bob",
		"--data-dir", e.dataDir, "--base-url", e.baseURL)
	if !strings.Contains(out, "invite for bob: "+e.baseURL) {
		t.Fatalf("invite output missing expected line:\n%s", out)
	}
	invTok := reInvite.FindString(out)
	if invTok == "" {
		t.Fatalf("invite output missing invite token:\n%s", out)
	}

	keyPath := filepath.Join(e.root, "bob_key")
	genKey(t, keyPath)
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatalf("read bob's public key: %v", err)
	}

	e.bobClient = newClient(t)
	resp, body := postForm(t, e.bobClient, e.baseURL+"/invite/"+invTok, url.Values{
		"key": {string(pub)},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("activate invite: status %d, body:\n%s", resp.StatusCode, body)
	}
	tok := reToken.FindString(body)
	if tok == "" {
		t.Fatalf("activation page missing token:\n%s", body)
	}
	e.bobToken = tok
	e.bobKey = keyPath

	e.bobDir = filepath.Join(e.root, "bob")
	sshEnv := []string{"GIT_SSH_COMMAND=ssh -i " + keyPath +
		" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes"}
	cloneURL := fmt.Sprintf("ssh://git@127.0.0.1:%d/bob/course.git", e.sshPort)
	git(t, e.root, sshEnv, "clone", cloneURL, e.bobDir)
	setIdentity(t, e.bobDir)

	writeFile(t, filepath.Join(e.bobDir, "tasks", "greet", "greet.sh"), greetSolution)
	git(t, e.bobDir, sshEnv, "add", "-A")
	git(t, e.bobDir, sshEnv, "commit", "-q", "-m", "solve greet")
	pushOut := git(t, e.bobDir, sshEnv, "push", "origin", "main")
	e.bobGreetSubID = taskSubmissionID(t, pushOut, "greet")

	// The invite activation logs bob in via a cookie; log in explicitly if
	// that ever changes so pollSubmission has a valid session either way.
	if status, _ := get(t, e.bobClient, e.baseURL+"/"); status != http.StatusOK {
		login(t, e, e.bobClient, "bob", e.bobToken)
	}
	pollSubmission(t, e.bobClient, e, e.bobGreetSubID)

	scores := fetchScores(t, e)
	if got := scores["bob"]["greet"]; got != "50" {
		t.Fatalf("bob/greet: got %q, want 50 (proves the hidden-tests overlay ran)", got)
	}
}

// genKey generates a fresh, passphrase-less ed25519 key pair at path.
func genKey(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
}

// 10. teacher pages: the teacher-only pages render and list the students.
func testTeacherPages(t *testing.T, e *env) {
	status, body := get(t, e.profClient, e.baseURL+"/matrix")
	if status != http.StatusOK {
		t.Fatalf("GET /matrix: status %d", status)
	}
	if !strings.Contains(body, "alice") || !strings.Contains(body, "bob") {
		t.Fatalf("/matrix missing alice/bob:\n%s", body)
	}

	if status, _ := get(t, e.profClient, e.baseURL+"/students"); status != http.StatusOK {
		t.Fatalf("GET /students: status %d", status)
	}
	if status, _ := get(t, e.profClient, e.baseURL+"/queue"); status != http.StatusOK {
		t.Fatalf("GET /queue: status %d", status)
	}
	status, body = get(t, e.profClient, e.baseURL+"/audit")
	if status != http.StatusOK {
		t.Fatalf("GET /audit: status %d", status)
	}
	if !strings.Contains(body, "user.register") {
		t.Fatalf("/audit missing user.register event:\n%s", body)
	}
}

// 11. teacher pushes course update: a broken course push is rejected; the
// same push, reverted, is accepted and reloads the course metadata.
func testTeacherCourseUpdate(t *testing.T, e *env) {
	e.profCloneDir = filepath.Join(e.root, "prof")
	cloneURL := fmt.Sprintf("http://prof:%s@127.0.0.1:%d/git/course.git", e.profToken, httpPortOf(t, e))
	git(t, e.root, nil, "clone", cloneURL, e.profCloneDir)
	setIdentity(t, e.profCloneDir)

	taskPath := filepath.Join(e.profCloneDir, "tasks", "late", "task.yaml")
	orig, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read %s: %v", taskPath, err)
	}
	if err := os.WriteFile(taskPath, append(orig, []byte("\ndeadline: [unclosed\n")...), 0o644); err != nil {
		t.Fatalf("write %s: %v", taskPath, err)
	}
	git(t, e.profCloneDir, nil, "add", "-A")
	git(t, e.profCloneDir, nil, "commit", "-q", "-m", "break late task")
	out, err := gitErr(e.profCloneDir, nil, "push", "origin", "main")
	if err == nil {
		t.Fatalf("push with broken yaml unexpectedly succeeded:\n%s", out)
	}
	if !regexpRejected.MatchString(out) {
		t.Fatalf("push rejection output missing expected marker:\n%s", out)
	}

	git(t, e.profCloneDir, nil, "revert", "--no-edit", "HEAD")
	out = git(t, e.profCloneDir, nil, "push", "origin", "main")
	if !strings.Contains(out, "course metadata reloaded") {
		t.Fatalf("revert push output missing reload confirmation:\n%s", out)
	}
}

// 12. auth and rate limit: unauthenticated access redirects to login,
// students get 404 on teacher routes, and repeated bad logins trip the
// shared rate limiter.
// 12b. language switcher: the public /lang endpoint flips the anonymous login
// page to Russian via a cookie, and rejects unsupported codes. The fixture
// course sets no `language:`, so the default is English.
func testLanguageSwitcher(t *testing.T, e *env) {
	c := newClient(t)

	status, body := get(t, c, e.baseURL+"/login")
	if status != http.StatusOK || !strings.Contains(body, "Sign in") {
		t.Fatalf("default /login: status %d, want English 'Sign in':\n%s", status, body)
	}

	resp, _ := postForm(t, c, e.baseURL+"/lang", url.Values{"lang": {"ru"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /lang ru: status %d, want 303", resp.StatusCode)
	}

	status, body = get(t, c, e.baseURL+"/login")
	if status != http.StatusOK {
		t.Fatalf("/login after switch: status %d", status)
	}
	if !strings.Contains(body, "Вход") {
		t.Errorf("/login after switch: missing Russian heading:\n%s", body)
	}
	if !strings.Contains(body, `lang="ru"`) {
		t.Errorf("/login after switch: missing lang=\"ru\" attribute")
	}

	resp, _ = postForm(t, c, e.baseURL+"/lang", url.Values{"lang": {"xx"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /lang xx: status %d, want 400", resp.StatusCode)
	}
}

func testAuthAndRateLimit(t *testing.T, e *env) {
	anon := newClient(t)
	resp, _ := get2(t, anon, e.baseURL+"/")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("anonymous GET /: status %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("anonymous GET /: Location %q does not start with /login", loc)
	}

	if status, _ := get(t, e.aliceClient, e.baseURL+"/matrix"); status != http.StatusNotFound {
		t.Fatalf("student GET /matrix: status %d, want 404", status)
	}

	fresh := newClient(t)
	var last *http.Response
	for i := range 11 {
		resp, body := postForm(t, fresh, e.baseURL+"/login", url.Values{
			"login": {"mallory"}, "token": {"ag_wrong"}, "next": {"/"},
		})
		last = resp
		if i < 10 {
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("bad login attempt %d: status %d, body:\n%s", i+1, resp.StatusCode, body)
			}
		}
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("11th bad login: status %d, want 429", last.StatusCode)
	}
}

// get2 is like get but also returns the response (for header inspection);
// the body is drained and discarded.
func get2(t *testing.T, client *http.Client, target string) (*http.Response, string) {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body from GET %s: %v", target, err)
	}
	return resp, string(body)
}

// 12c. serve --local: a second server started with --local serves the whole UI
// to an anonymous browser as the implicit teacher account (SPEC §8), with no
// way (and no need) to log in or out.
func testServeLocal(t *testing.T, e *env) {
	baseURL := startLocalServer(t, e)
	anon := newClient(t)

	status, body := get(t, anon, baseURL+"/")
	if status != http.StatusOK {
		t.Fatalf("local GET /: status %d, want 200", status)
	}
	if !strings.Contains(body, "Local User") {
		t.Fatalf("local GET /: body does not name the implicit user:\n%s", body)
	}
	if strings.Contains(body, `action="/logout"`) {
		t.Errorf("local GET /: log out must be hidden:\n%s", body)
	}
	// The implicit user is a teacher, so teacher-only routes are open too.
	if status, _ := get(t, anon, baseURL+"/matrix"); status != http.StatusOK {
		t.Fatalf("local GET /matrix: status %d, want 200", status)
	}
	// Nothing to sign in to: the login form redirects to the dashboard.
	resp, _ := get2(t, anon, baseURL+"/login")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("local GET /login: status %d, Location %q, want 302 to /",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

// startLocalServer runs a second `serve --local` over the same course fixture
// with its own data dir and ports, and returns its base URL.
func startLocalServer(t *testing.T, e *env) string {
	t.Helper()
	httpPort := freePort(t)
	sshPort := freePort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	logPath := filepath.Join(e.root, "serve-local.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create serve-local.log: %v", err)
	}
	cmd := exec.Command(bin, "serve",
		"--repo", e.courseDir,
		"--data-dir", filepath.Join(e.root, "data-local"),
		"--http-addr", fmt.Sprintf("127.0.0.1:%d", httpPort),
		"--ssh-addr", fmt.Sprintf("127.0.0.1:%d", sshPort),
		"--workers", "1",
		"--local",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve --local: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		logFile.Close()
		if t.Failed() {
			if b, err := os.ReadFile(logPath); err == nil {
				t.Logf("serve-local.log:\n%s", b)
			}
		}
	})
	waitReady(t, baseURL)
	return baseURL
}

// 13. local self-check: `anygrade check` passes against alice's clone, which
// now holds the correct sum.sh solution.
func testLocalSelfCheck(t *testing.T, e *env) {
	// Flags must precede the positional task argument: the stdlib flag
	// package stops parsing at the first non-flag argument.
	out, err := runBinErr(e.aliceDir, "check", "--runner", "local", "--data-dir", t.TempDir(), "sum")
	if err != nil {
		t.Fatalf("check sum: %v\n%s", err, out)
	}
}

// 14. force push after submission: a graded commit that a force push drops
// from the branch survives under refs/anygrade/submissions/<id> (SPEC §6 step
// 7), and the rewritten branch keeps being graded.
func testForcePushAfterSubmission(t *testing.T, e *env) {
	sumPath := filepath.Join(e.aliceDir, "tasks", "sum", "sum.sh")
	writeFile(t, sumPath, sumSolution+"# graded before the force push\n")
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "solve sum once more")
	graded := strings.TrimSpace(git(t, e.aliceDir, nil, "rev-parse", "HEAD"))
	out := git(t, e.aliceDir, nil, "push", "origin", "main")
	gradedID := taskSubmissionID(t, out, "sum")
	pollSubmission(t, e.aliceClient, e, gradedID)

	// Rewrite history: the graded commit is no longer reachable from main.
	// The push itself is a change like any other, so it is graded too - and
	// it moves the baseline off the dropped commit.
	git(t, e.aliceDir, nil, "reset", "--hard", "HEAD~1")
	out = git(t, e.aliceDir, nil, "push", "--force", "origin", "main")
	pollSubmission(t, e.aliceClient, e, taskSubmissionID(t, out, "sum"))

	bare := filepath.Join(e.dataDir, "repos", "students", "alice.git")
	ref := fmt.Sprintf("refs/anygrade/submissions/%d", gradedID)
	if got := strings.TrimSpace(git(t, bare, nil, "rev-parse", ref)); got != graded {
		t.Fatalf("%s = %q, want the graded commit %q", ref, got, graded)
	}
	// gc drops every unreachable object: the pin is the only thing left
	// holding the graded tree.
	git(t, bare, nil, "gc", "--prune=now", "--quiet")
	if out, err := gitErr(bare, nil, "cat-file", "-e", graded+"^{commit}"); err != nil {
		t.Fatalf("graded commit %s gone after the force push and gc: %v\n%s", graded, err, out)
	}
	status, body := get(t, e.aliceClient, fmt.Sprintf("%s/submissions/%d", e.baseURL, gradedID))
	if status != http.StatusOK || !strings.Contains(body, ">done<") {
		t.Fatalf("submission #%d after the force push: status %d, body:\n%s", gradedID, status, body)
	}

	// Grading continues on the rewritten branch.
	writeFile(t, sumPath, sumSolution+"# after the force push\n")
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "solve sum after the force push")
	out = git(t, e.aliceDir, nil, "push", "origin", "main")
	pollSubmission(t, e.aliceClient, e, taskSubmissionID(t, out, "sum"))
	if got := fetchScores(t, e)["alice"]["sum"]; got != "100" {
		t.Fatalf("alice/sum after the force push: got %q, want 100", got)
	}
}

// 15. tamper notes: a push that rewrites a workspace.include file and drops
// the solution file is still graded against the authoritative course versions,
// and the teacher sees both facts in the worker note (SPEC §6.1).
func testTamperNotes(t *testing.T, e *env) {
	// A syntactically broken include: were it not restored from the course
	// repo, the task's `common` gate would fail and skip the rest.
	writeFile(t, filepath.Join(e.aliceDir, "common.sh"), "if [\n")
	if err := os.Remove(filepath.Join(e.aliceDir, "tasks", "sum", "sum.sh")); err != nil {
		t.Fatalf("remove sum.sh: %v", err)
	}
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "tamper with common.sh, drop sum.sh")
	out := git(t, e.aliceDir, nil, "push", "origin", "main")

	body := pollSubmission(t, e.aliceClient, e, taskSubmissionID(t, out, "sum"))
	for _, note := range []string{
		"modified outside solution_files (restored): common.sh",
		"solution file missing in the submitted commit (template used): tasks/sum/sum.sh",
	} {
		if !strings.Contains(body, note) {
			t.Errorf("submission page missing worker note %q:\n%s", note, body)
		}
	}
	// Nothing was skipped: both gates ran against the restored authoritative
	// files, not against alice's versions.
	if strings.Contains(body, "st-skipped") {
		t.Errorf("a check was skipped: the authoritative files were not restored:\n%s", body)
	}
}

// regexpRejected matches either push-rejection phrasing intake.go uses.
var regexpRejected = regexp.MustCompile(`push rejected|validation failed`)
