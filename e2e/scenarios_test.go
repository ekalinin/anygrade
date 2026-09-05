//go:build e2e

package e2e

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html"
	"io"
	"math/big"
	"net"
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
	// The greet task mixes a two-phase check with a run-only one and configures
	// hidden tests, so the run-only one executes after the boundary removed
	// them. Harmless here - it only reads the open template - but it is exactly
	// the shape that silently mis-grades, so it is reported as a warning and
	// does not fail the course.
	if !strings.Contains(out, "runs after the hidden tests are removed") {
		t.Fatalf("validate: missing the run-only-beside-a-build-phase warning:\n%s", out)
	}
	// The documented workflow runs `validate` from inside the course repo, where
	// --repo keeps its default. The course root is then relative, and the
	// workspace.include entries must still resolve against it.
	out, err = runBinErr(e.courseDir, "validate")
	if err != nil {
		t.Fatalf("validate from the course dir: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK: course is valid") {
		t.Fatalf("validate from the course dir: missing success line:\n%s", out)
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

	// SPEC §7: the personal repo is created at activation, so the clone URL
	// just printed already works. This scenario runs before the first git
	// access, so nothing else could have provisioned it.
	bare := filepath.Join(e.dataDir, "repos", "students", "alice.git")
	if _, err := os.Stat(bare); err != nil {
		t.Fatalf("register: personal repo not provisioned at %s: %v", bare, err)
	}

	// SPEC §8: the fixture caps self-registration at one account, so the code
	// alone is no longer enough - alice already spent the course's only place.
	// Bob still joins later by invite, which the cap does not count.
	resp, body = postForm(t, newClient(t), e.baseURL+"/register", url.Values{
		"login": {"mallory"}, "name": {"Mallory"}, "course_code": {"e2e-code"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("register over max_accounts: status %d, body:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "registration is closed") {
		t.Fatalf("register over max_accounts: no cap message:\n%s", body)
	}
}

// 4. clone and push over http: alice pushes a correct sum.sh and the push
// hook queues a submission.
func testCloneAndPushHTTP(t *testing.T, e *env) {
	e.aliceDir = filepath.Join(e.root, "alice")
	httpCloneURL := fmt.Sprintf("http://alice:%s@127.0.0.1:%d/git/alice/course.git", e.aliceToken, e.httpPort)
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

	// The rejected row is pinned like any other submission: its page links to
	// the submitted code, and this ref is what keeps that tree readable after
	// a later force push (SPEC §6 step 7). The push touched only `late`, so
	// nothing else could have pinned this commit.
	head := strings.TrimSpace(git(t, e.aliceDir, nil, "rev-parse", "HEAD"))
	bare := filepath.Join(e.dataDir, "repos", "students", "alice.git")
	pinned := git(t, bare, nil, "for-each-ref", "--format=%(objectname)", "refs/anygrade/submissions/")
	if !strings.Contains(pinned, head) {
		t.Fatalf("rejected commit %s is not pinned; submission refs point at:\n%s", head, pinned)
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
	resp, body := postForm(t, e.bobClient, e.baseURL+"/invite/"+invTok, url.Values{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("activate invite: status %d, body:\n%s", resp.StatusCode, body)
	}
	tok := reToken.FindString(body)
	if tok == "" {
		t.Fatalf("activation page missing token:\n%s", body)
	}
	e.bobToken = tok
	e.bobKey = keyPath

	// Activation no longer takes an SSH key: bob registers it in settings and
	// proves possession by signing the server's challenge with his own
	// ssh-keygen, exactly as a student would (SPEC §8).
	resp, body = postForm(t, e.bobClient, e.baseURL+"/settings/keys", url.Values{
		"key": {string(pub)},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("key challenge: status %d, body:\n%s", resp.StatusCode, body)
	}
	// Read the command off the page and sign exactly what it prints - the line
	// names bob and his fingerprint, not just an opaque nonce.
	m := reProofCmd.FindStringSubmatch(html.UnescapeString(body))
	if m == nil {
		t.Fatalf("challenge page missing the sign command:\n%s", body)
	}
	message := m[1]
	nonce := reChallenge.FindString(message)
	if nonce == "" {
		t.Fatalf("the printed message carries no nonce: %q", message)
	}
	if !strings.Contains(message, "user=bob") {
		t.Fatalf("the signed line does not name the account: %q", message)
	}
	signature := signChallenge(t, keyPath, message)

	// A signature over somebody else's challenge is not a proof.
	resp, _ = postForm(t, e.bobClient, e.baseURL+"/settings/keys/verify", url.Values{
		"nonce": {nonce}, "signature": {signChallenge(t, keyPath, message+"x")},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad proof: status %d, want 422", resp.StatusCode)
	}

	resp, body = postForm(t, e.bobClient, e.baseURL+"/settings/keys/verify", url.Values{
		"nonce": {nonce}, "signature": {signature},
	})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings" {
		t.Fatalf("proof: status %d location %q, body:\n%s",
			resp.StatusCode, resp.Header.Get("Location"), body)
	}

	// The nonce is single-use: replaying the very same pair registers nothing.
	resp, _ = postForm(t, e.bobClient, e.baseURL+"/settings/keys/verify", url.Values{
		"nonce": {nonce}, "signature": {signature},
	})
	if got := resp.Header.Get("Location"); got != "/settings?flash=key_challenge_expired" {
		t.Fatalf("replayed proof: location %q", got)
	}

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

// signChallenge is the command the challenge page tells the student to run.
// The e2e suite deliberately uses the real ssh-keygen here: the server verifies
// the SSHSIG in pure Go, and this is what keeps that parser honest against the
// client students actually have.
func signChallenge(t *testing.T, keyPath, message string) string {
	t.Helper()
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-f", keyPath, "-n", "anygrade", "-")
	cmd.Stdin = strings.NewReader(message)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("ssh-keygen -Y sign: %v", err)
	}
	return stdout.String()
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

// 11a. TA rights: a course assistant reviews work without touching accounts
// (SPEC §8). The reviewing half answers exactly as it does for a teacher -
// build-phase log included - and every route that changes the record answers
// 404, not 403, so the refusal does not confirm the route exists.
func testTARights(t *testing.T, e *env) {
	out := runBin(t, "", "user", "add", "--login", "tanya", "--role", "ta", "--data-dir", e.dataDir)
	token := reToken.FindString(out)
	if token == "" {
		t.Fatalf("no token in `user add --role ta` output:\n%s", out)
	}
	e.taClient = newClient(t)
	login(t, e, e.taClient, "tanya", token)

	sub := fmt.Sprintf("/submissions/%d", e.bobGreetSubID)
	for _, target := range []string{
		"/matrix", "/queue", "/students", "/students/bob", "/export/scores.csv",
		sub, sub + "/logs/hidden", sub + "/logs/hidden?phase=build",
	} {
		if status, body := get(t, e.taClient, e.baseURL+target); status != http.StatusOK {
			t.Errorf("ta GET %s: status %d, want 200, body:\n%s", target, status, body)
		}
	}
	// The build phase is the one that compiled against the hidden tests, and
	// the TA is meant to read it - that is the whole point of the decision.
	if _, body := get(t, e.taClient, e.baseURL+sub+"/logs/hidden?phase=build"); !strings.Contains(body, hiddenSecret) {
		t.Errorf("ta build log does not carry the hidden test output:\n%s", body)
	}

	if status, _ := get(t, e.taClient, e.baseURL+"/audit"); status != http.StatusNotFound {
		t.Errorf("ta GET /audit: status %d, want 404", status)
	}
	for _, c := range []struct {
		target string
		form   url.Values
	}{
		{"/students/bob/token/reset", nil},
		{"/students/bob/state", url.Values{"state": {"disabled"}}},
		{"/students/bob/tasks/greet/override", url.Values{"score": {"100"}, "comment": {"nope"}}},
	} {
		resp, body := postForm(t, e.taClient, e.baseURL+c.target, c.form)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("ta POST %s: status %d, want 404, body:\n%s", c.target, resp.StatusCode, body)
		}
	}
	// bob is untouched by the refusals above, and his page - which the TA does
	// read - offers none of the controls behind them.
	if status, body := get(t, e.taClient, e.baseURL+"/students/bob"); status != http.StatusOK {
		t.Errorf("ta GET /students/bob: status %d", status)
	} else if strings.Contains(body, "/token/reset") || strings.Contains(body, "/override") {
		t.Errorf("the TA's student page offers account controls:\n%s", body)
	}
	if status, _ := get(t, e.bobClient, e.baseURL+"/"); status != http.StatusOK {
		t.Errorf("bob's account was disabled by a refused request: status %d", status)
	}
}

// 11. teacher pushes course update: a broken course push is rejected; the
// same push, reverted, is accepted and reloads the course metadata.
func testTeacherCourseUpdate(t *testing.T, e *env) {
	e.profCloneDir = filepath.Join(e.root, "prof")
	cloneURL := fmt.Sprintf("http://prof:%s@127.0.0.1:%d/git/course.git", e.profToken, e.httpPort)
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

// 18. restart requeues a running submission: a server killed mid-check comes
// back, requeues what was `running`, and reruns it from scratch (SPEC §13).
func testRestartRequeue(t *testing.T, e *env) {
	writeFile(t, filepath.Join(e.aliceDir, "tasks", "slow", "notes.txt"), "restart\n")
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "slow: before the restart")
	out := git(t, e.aliceDir, nil, "push", "origin", "main")
	id := taskSubmissionID(t, out, "slow")

	pollStatus(t, e.aliceClient, e, id, "running")
	restartServer(t, e)

	// The rerun gets a fresh workspace, so the check passes on its own merits;
	// what the restart must not do is leave the submission stuck in `running`.
	pollSubmission(t, e.aliceClient, e, id)
	if got := fetchScores(t, e)["alice"]["slow"]; got != "10" {
		t.Fatalf("alice/slow after the restart: got %q, want 10", got)
	}
}

// 19. teacher cancels a running submission: the run stops, the row carries the
// canceled note, and the retry loop never re-arms it (SPEC §13, §12). The
// status stored is infra_error - there is no `canceled` status - but the page
// refines it to `canceled` because canceled_at is set.
func testCancelRunning(t *testing.T, e *env) {
	writeFile(t, filepath.Join(e.aliceDir, "tasks", "slow", "notes.txt"), "cancel\n")
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "slow: to be canceled")
	out := git(t, e.aliceDir, nil, "push", "origin", "main")
	id := taskSubmissionID(t, out, "slow")

	pollStatus(t, e.aliceClient, e, id, "running")
	resp, body := postForm(t, e.profClient, fmt.Sprintf("%s/queue/%d/cancel", e.baseURL, id), nil)
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("cancel submission #%d: status %d, body:\n%s", id, resp.StatusCode, body)
	}

	pollStatus(t, e.aliceClient, e, id, "canceled")

	// The note reaches the teacher's queue row.
	status, queue := get(t, e.profClient, e.baseURL+"/queue")
	if status != http.StatusOK {
		t.Fatalf("GET /queue: status %d", status)
	}
	if !strings.Contains(queue, "canceled by teacher") {
		t.Fatalf("queue row for canceled submission #%d missing the note:\n%s", id, queue)
	}

	// A cancel that re-armed the retry loop would flip the row back to running
	// moments later; give it the chance to and confirm it does not.
	time.Sleep(2 * time.Second)
	_, page := get(t, e.aliceClient, fmt.Sprintf("%s/submissions/%d", e.baseURL, id))
	if !strings.Contains(page, ">canceled<") {
		t.Fatalf("canceled submission #%d was re-armed:\n%s", id, page)
	}
	// And the student is told, on their own page: a cancel records no check
	// results, so the note is the only thing the page has to show.
	if !strings.Contains(page, "canceled by teacher") {
		t.Errorf("student page for canceled submission #%d missing the note:\n%s", id, page)
	}
	// The cancel is a teacher action, so it is in the audit log.
	_, audit := get(t, e.profClient, e.baseURL+"/audit")
	if !strings.Contains(audit, "submission.cancel") {
		t.Errorf("/audit missing the submission.cancel event:\n%s", audit)
	}
}

// 20. check timeout: the hanging check is killed at the task's runner timeout
// and marked as such, the check after it still runs, and the score reflects
// exactly one of the two weights (SPEC §13).
func testCheckTimeout(t *testing.T, e *env) {
	writeFile(t, filepath.Join(e.aliceDir, "tasks", "timeout", "notes.txt"), "go\n")
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "trigger the timeout task")
	out := git(t, e.aliceDir, nil, "push", "origin", "main")

	body := pollSubmission(t, e.aliceClient, e, taskSubmissionID(t, out, "timeout"))
	if !strings.Contains(body, "timed out after 2s") {
		t.Errorf("submission page missing the timeout note:\n%s", body)
	}
	if strings.Contains(body, "st-skipped") {
		t.Errorf("the check after a non-gate timeout was skipped:\n%s", body)
	}
	// hang failed, after passed: half the weight, half the score.
	if got := fetchScores(t, e)["alice"]["timeout"]; got != "10" {
		t.Fatalf("alice/timeout: got %q, want 10 (one of two checks passed)", got)
	}
}

// 21. soft deadline penalty: a submission past the soft deadline is accepted,
// carries the capped penalty, and the penalised score is what lands in the CSV
// (SPEC §9).
func testSoftDeadlinePenalty(t *testing.T, e *env) {
	writeFile(t, filepath.Join(e.aliceDir, "tasks", "soft", "notes.txt"), "late but accepted\n")
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "solve soft")
	out := git(t, e.aliceDir, nil, "push", "origin", "main")

	body := pollSubmission(t, e.aliceClient, e, taskSubmissionID(t, out, "soft"))
	if !strings.Contains(body, "late penalty 50%") {
		t.Errorf("submission page missing the capped late penalty:\n%s", body)
	}
	if got := fetchScores(t, e)["alice"]["soft"]; got != "50" {
		t.Fatalf("alice/soft: got %q, want 50 (100 raw, 50%% capped penalty)", got)
	}
}

// 22. attempt limit: the third push to a two-attempt task is rejected in the
// push output, stored with its reason, and never runs (SPEC §4.3, §12).
func testAttemptLimit(t *testing.T, e *env) {
	notes := filepath.Join(e.aliceDir, "tasks", "limited", "notes.txt")
	for i := range 2 {
		writeFile(t, notes, fmt.Sprintf("attempt %d\n", i+1))
		git(t, e.aliceDir, nil, "add", "-A")
		git(t, e.aliceDir, nil, "commit", "-q", "-m", fmt.Sprintf("limited attempt %d", i+1))
		out := git(t, e.aliceDir, nil, "push", "origin", "main")
		pollSubmission(t, e.aliceClient, e, taskSubmissionID(t, out, "limited"))
	}

	writeFile(t, notes, "attempt 3\n")
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "limited attempt 3")
	out := git(t, e.aliceDir, nil, "push", "origin", "main")
	if !strings.Contains(out, "attempt limit reached (2 of 2)") {
		t.Fatalf("third push not rejected by the attempt limit:\n%s", out)
	}
	if regexp.MustCompile(`limited\s+submission #\d+ queued`).MatchString(out) {
		t.Fatalf("the rejected third attempt was queued anyway:\n%s", out)
	}
}

// 23. hidden tests unavailable: with the overlay repo unreachable the student
// sees only the scrubbed message - never the URL, never git's error - while the
// server log keeps the detail. The broken URL is a cache key of its own, so the
// offline fallback to the pinned ref cannot mask the failure.
func testHiddenTestsScrubbed(t *testing.T, e *env) {
	gone := filepath.Join(e.root, "hidden-gone")
	taskPath := filepath.Join(e.profCloneDir, "tasks", "greet", "task.yaml")
	orig, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read %s: %v", taskPath, err)
	}
	broken := strings.Replace(string(orig), "file://"+e.hiddenDir, "file://"+gone, 1)
	if broken == string(orig) {
		t.Fatalf("hidden_tests url not found in %s:\n%s", taskPath, orig)
	}
	writeFile(t, taskPath, broken)
	git(t, e.profCloneDir, nil, "add", "-A")
	git(t, e.profCloneDir, nil, "commit", "-q", "-m", "point greet at a missing hidden repo")
	git(t, e.profCloneDir, nil, "push", "origin", "main")

	// Restore the course before asserting, so a failed assertion cannot leave
	// the fixture broken for whatever runs next.
	t.Cleanup(func() {
		writeFile(t, taskPath, string(orig))
		git(t, e.profCloneDir, nil, "add", "-A")
		git(t, e.profCloneDir, nil, "commit", "-q", "-m", "restore greet hidden tests")
		git(t, e.profCloneDir, nil, "push", "origin", "main")
	})

	sshEnv := []string{"GIT_SSH_COMMAND=ssh -i " + e.bobKey +
		" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes"}
	writeFile(t, filepath.Join(e.bobDir, "tasks", "greet", "greet.sh"), greetSolution+"# retry\n")
	git(t, e.bobDir, sshEnv, "add", "-A")
	git(t, e.bobDir, sshEnv, "commit", "-q", "-m", "greet with hidden tests down")
	out := git(t, e.bobDir, sshEnv, "push", "origin", "main")

	id := taskSubmissionID(t, out, "greet")
	page := pollStatus(t, e.bobClient, e, id, "retrying")
	if strings.Contains(page, gone) || strings.Contains(page, "file://") {
		t.Errorf("student page leaks the hidden-tests location:\n%s", page)
	}
	// The scrubbed message is the whole point of the scrubbing: it is what the
	// student is meant to read (SPEC §14), from the first retry on - a run that
	// recorded no check results has nothing else to explain itself with.
	if !strings.Contains(page, "hidden tests temporarily unavailable") {
		t.Errorf("student page does not say why the submission is stuck:\n%s", page)
	}

	// The same note reaches the teacher's queue row.
	status, queue := get(t, e.profClient, e.baseURL+"/queue")
	if status != http.StatusOK {
		t.Fatalf("GET /queue: status %d", status)
	}
	if !strings.Contains(queue, "hidden tests temporarily unavailable") {
		t.Errorf("queue row for submission #%d missing the scrubbed message:\n%s", id, queue)
	}
	if strings.Contains(queue, gone) {
		t.Errorf("queue row leaks the hidden-tests location:\n%s", queue)
	}
}

// 24. multiple tasks in one push: one submission per changed task, queued
// independently (SPEC §13). Scenario 4 asserts the negative - untouched tasks
// stay out of the queue - this asserts the positive.
func testMultiTaskPush(t *testing.T, e *env) {
	writeFile(t, filepath.Join(e.aliceDir, "tasks", "sum", "sum.sh"), sumSolution+"# multi\n")
	writeFile(t, filepath.Join(e.aliceDir, "tasks", "greet", "greet.sh"), greetSolution)
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "solve sum and greet in one commit")
	out := git(t, e.aliceDir, nil, "push", "origin", "main")

	if !strings.Contains(out, "2 task(s) detected") {
		t.Fatalf("push touching two tasks should detect both:\n%s", out)
	}
	sumID := taskSubmissionID(t, out, "sum")
	greetID := taskSubmissionID(t, out, "greet")
	if sumID == greetID {
		t.Fatalf("both tasks share submission #%d:\n%s", sumID, out)
	}
	pollSubmission(t, e.aliceClient, e, sumID)
	pollSubmission(t, e.aliceClient, e, greetID)
}

// 25. non-default branch: accepted and stored, but not graded, and the push
// output says so (SPEC §13).
func testNonDefaultBranch(t *testing.T, e *env) {
	git(t, e.aliceDir, nil, "checkout", "-q", "-b", "scratch")
	writeFile(t, filepath.Join(e.aliceDir, "tasks", "sum", "sum.sh"), sumSolution+"# on scratch\n")
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "sum on a side branch")
	out := git(t, e.aliceDir, nil, "push", "origin", "scratch")
	git(t, e.aliceDir, nil, "checkout", "-q", "main")

	if !strings.Contains(out, "branch scratch stored; only main is graded") {
		t.Fatalf("side-branch push output missing the stored-not-graded line:\n%s", out)
	}
	if reSubmission.MatchString(out) {
		t.Fatalf("a side-branch push queued a submission:\n%s", out)
	}
	// Stored means stored: the ref exists in the personal repo.
	bare := filepath.Join(e.dataDir, "repos", "students", "alice.git")
	if out, err := gitErr(bare, nil, "rev-parse", "--verify", "refs/heads/scratch"); err != nil {
		t.Fatalf("scratch branch not stored in the personal repo: %v\n%s", err, out)
	}
}

// 26. push touching only non-task files: accepted, nothing queued, and the
// student is told (SPEC §13).
func testNonTaskPush(t *testing.T, e *env) {
	writeFile(t, filepath.Join(e.aliceDir, "NOTES.md"), "just a note\n")
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "add NOTES.md")
	out := git(t, e.aliceDir, nil, "push", "origin", "main")

	if !strings.Contains(out, "no tasks changed") {
		t.Fatalf("non-task push output missing the informational line:\n%s", out)
	}
	if reSubmission.MatchString(out) {
		t.Fatalf("a non-task push queued a submission:\n%s", out)
	}
}

// 27. cross-student access: alice cannot read bob's submission, and gets the
// same 404 the teacher routes give her - not a 403 that would confirm the row
// exists (SPEC §8).
func testCrossStudentAccess(t *testing.T, e *env) {
	target := fmt.Sprintf("%s/submissions/%d", e.baseURL, e.bobGreetSubID)
	if status, _ := get(t, e.aliceClient, target); status != http.StatusNotFound {
		t.Fatalf("alice GET bob's submission: status %d, want 404", status)
	}
	// The teacher can, which is what makes the 404 an authorization answer and
	// not a missing row.
	if status, _ := get(t, e.profClient, target); status != http.StatusOK {
		t.Fatalf("teacher GET bob's submission: status %d, want 200", status)
	}
	if status, _ := get(t, e.aliceClient, e.baseURL+"/students/bob"); status != http.StatusNotFound {
		t.Fatalf("alice GET /students/bob: status %d, want 404", status)
	}
}

// 34. json api: the token a student pushes with is also the bearer that reads
// the machine-readable contract, with no login form in between - and the role
// boundary of the pages holds there too (SPEC §10.2, §14).
func testJSONAPI(t *testing.T, e *env) {
	resp, body := apiGet(t, e, e.aliceToken, "/api/v1/me")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alice GET /api/v1/me: status %d, body:\n%s", resp.StatusCode, body)
	}
	var me struct {
		Login string `json:"login"`
		Role  string `json:"role"`
	}
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatalf("decode /api/v1/me: %v\n%s", err, body)
	}
	if me.Login != "alice" || me.Role != "student" {
		t.Errorf("/api/v1/me: %+v, want alice/student", me)
	}
	// The API never starts a session: a bot polling it must not accumulate one.
	if c := resp.Header.Get("Set-Cookie"); c != "" {
		t.Errorf("/api/v1/me set a cookie: %q", c)
	}

	// No token is a JSON 401, not the redirect the same anonymous GET gets on a
	// page - a script has no login form to follow.
	resp, body = apiGet(t, e, "", "/api/v1/tasks")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /api/v1/tasks: status %d, want 401", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("anonymous GET /api/v1/tasks: Location %q, want none", loc)
	}
	if !strings.Contains(body, `"code":"unauthorized"`) {
		t.Errorf("anonymous GET /api/v1/tasks: body has no error code:\n%s", body)
	}

	// The submission boundary: 404 for the classmate, 200 for the teacher, so
	// the refusal cannot be read as "this id exists".
	target := fmt.Sprintf("/api/v1/submissions/%d", e.bobGreetSubID)
	if resp, body = apiGet(t, e, e.aliceToken, target); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("alice GET %s: status %d, want 404, body:\n%s", target, resp.StatusCode, body)
	}
	if resp, body = apiGet(t, e, e.profToken, target); resp.StatusCode != http.StatusOK {
		t.Fatalf("teacher GET %s: status %d, want 200, body:\n%s", target, resp.StatusCode, body)
	}

	// Teacher-only endpoints are absent for a student, exactly like their pages.
	if resp, _ = apiGet(t, e, e.aliceToken, "/api/v1/matrix"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("alice GET /api/v1/matrix: status %d, want 404", resp.StatusCode)
	}
	resp, body = apiGet(t, e, e.profToken, "/api/v1/matrix")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("teacher GET /api/v1/matrix: status %d, body:\n%s", resp.StatusCode, body)
	}
	var matrix struct {
		Rows []struct {
			Login string `json:"login"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &matrix); err != nil {
		t.Fatalf("decode /api/v1/matrix: %v\n%s", err, body)
	}
	seen := map[string]bool{}
	for _, row := range matrix.Rows {
		seen[row.Login] = true
	}
	for _, want := range []string{"alice", "bob"} {
		if !seen[want] {
			t.Errorf("/api/v1/matrix has no row for %s:\n%s", want, body)
		}
	}
}

// 28. cli export against a live server: a second process reads the same SQLite
// file the server is writing and produces the same matrix as the web export
// (AGENTS.md: MaxOpenConns(1) + WAL).
func testCLIExport(t *testing.T, e *env) {
	out := runBin(t, "", "export", "scores", "--format", "csv",
		"--repo", e.courseDir, "--data-dir", e.dataDir)

	records, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatalf("parse cli csv: %v\n%s", err, out)
	}
	if len(records) == 0 {
		t.Fatalf("cli csv: empty")
	}
	cli := map[string]map[string]string{}
	header := records[0]
	for _, rec := range records[1:] {
		row := map[string]string{}
		for i, col := range header {
			if i < len(rec) {
				row[col] = rec[i]
			}
		}
		cli[row["login"]] = row
	}

	web := fetchScores(t, e)
	for _, login := range []string{"alice", "bob"} {
		if len(web[login]) == 0 {
			t.Fatalf("web export has no row for %s", login)
		}
		for task, want := range web[login] {
			if got := cli[login][task]; got != want {
				t.Errorf("cli export %s/%s = %q, web export = %q", login, task, got, want)
			}
		}
	}
}

// 29. token reset: rotating bob's token from the CLI invalidates the old one
// for both surfaces it authenticates - web login and git HTTP basic auth -
// while his SSH key keeps working (SPEC §8, §12). Bob rather than alice: he
// pushes over SSH, so no HTTP remote a later scenario needs gets invalidated.
func testTokenReset(t *testing.T, e *env) {
	old := e.bobToken
	httpURL := func(tok string) string {
		return fmt.Sprintf("http://bob:%s@127.0.0.1:%d/git/bob/course.git", tok, e.httpPort)
	}
	if out, err := gitErr(e.root, nil, "ls-remote", httpURL(old)); err != nil {
		t.Fatalf("bob's token should work before the reset: %v\n%s", err, out)
	}

	out := runBin(t, "", "user", "reset-token", "--login", "bob", "--data-dir", e.dataDir)
	fresh := reToken.FindString(out)
	if fresh == "" || fresh == old {
		t.Fatalf("reset-token did not issue a new token:\n%s", out)
	}
	e.bobToken = fresh

	if out, err := gitErr(e.root, nil, "ls-remote", httpURL(old)); err == nil {
		t.Fatalf("the old token still authenticates git over http:\n%s", out)
	}
	if out, err := gitErr(e.root, nil, "ls-remote", httpURL(fresh)); err != nil {
		t.Fatalf("the new token does not authenticate git over http: %v\n%s", err, out)
	}

	stale := newClient(t)
	resp, body := postForm(t, stale, e.baseURL+"/login", url.Values{
		"login": {"bob"}, "token": {old}, "next": {"/"},
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("web login with the old token: status %d, body:\n%s", resp.StatusCode, body)
	}

	// SSH authenticates by key, so the rotation must not have touched it.
	sshEnv := []string{"GIT_SSH_COMMAND=ssh -i " + e.bobKey +
		" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes"}
	if out, err := gitErr(e.bobDir, sshEnv, "ls-remote", "origin"); err != nil {
		t.Fatalf("bob's ssh key stopped working after a token reset: %v\n%s", err, out)
	}

	// Leave bob logged in for whatever runs next.
	e.bobClient = newClient(t)
	login(t, e, e.bobClient, "bob", fresh)
}

// 36. deactivate and reactivate a student: disabling bob from the student page
// closes every credential path at once, and reactivating him reopens all four.
// The three things that authenticate him - his token, his live session, his SSH
// key - are three independent `users.state = 'active'` filters in internal/store
// (tokens.go, sessions.go, sshkeys.go), so what this proves is that they keep
// agreeing: a disabled student who can still reach one transport is not
// disabled (SPEC §8, §12).
//
// It drives the web surface in both directions because it is the only one that
// has both: `anygrade user remove` deactivates without recording an event and
// has no reactivate counterpart, so the CLI cannot close this loop.
//
// Bob rather than alice: he is the only student holding all three credentials.
// Reactivating is one of the scenario's own assertions rather than a courtesy
// at the end, which is what leaves the shared fixture exactly as it was found.
func testDeactivateStudent(t *testing.T, e *env) {
	setState := func(state string) {
		t.Helper()
		resp, body := postForm(t, e.profClient, e.baseURL+"/students/bob/state",
			url.Values{"state": {state}})
		if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
			t.Fatalf("set bob's state to %s: status %d, body:\n%s", state, resp.StatusCode, body)
		}
	}

	// The baseline every rejection below is measured against: without it a
	// credential that was already broken would read as a successful lockout.
	assertBobAccess(t, e, true)

	setState("disabled")
	// The reactivation further down is the assertion; this is the backstop, so
	// a failure in between cannot hand a disabled bob to the scenarios after
	// this one.
	t.Cleanup(func() { setState("active") })

	assertBobAccess(t, e, false)

	// A deactivation is a teacher action against a student, so it is where a
	// teacher looks for it - filtered, and with the actor and the new state on
	// the row rather than just the kind.
	_, page := get(t, e.profClient, e.baseURL+"/audit?kind=user.state&target=bob")
	if !reStateEvent.MatchString(page) {
		t.Fatalf("/audit has no user.state row naming prof, bob and disabled:\n%s", page)
	}

	setState("active")
	assertBobAccess(t, e, true)
}

// reStateEvent matches the audit row a deactivation writes. It spans actor,
// actor role, kind, target and detail on purpose: the filtered page echoes
// "user.state" and "bob" back in its own filter widgets, so a looser match
// would pass with no events at all. The role cell is what tells a teacher's
// deactivation from a TA's, so it is pinned rather than skipped over.
var reStateEvent = regexp.MustCompile(
	`<td>prof</td><td>teacher</td><td>user\.state</td><td>bob</td><td>disabled</td>`)

// assertBobAccess exercises the four credential paths a deactivation has to
// close and requires them to answer the same way. want=true means every path
// works, want=false that none does.
//
// Neither git assertion moves a ref: bob's clone is already up to date, so each
// push authenticates and then stops at "Everything up-to-date", which keeps the
// scenario out of the gradebook and the CSV assertions elsewhere in the suite.
func assertBobAccess(t *testing.T, e *env, want bool) {
	t.Helper()

	// The token as a web login credential (store.VerifyToken).
	resp, body := postForm(t, newClient(t), e.baseURL+"/login", url.Values{
		"login": {"bob"}, "token": {e.bobToken}, "next": {"/"},
	})
	if ok := resp.StatusCode == http.StatusFound; ok != want {
		t.Fatalf("web login with bob's token: status %d, want ok=%v, body:\n%s",
			resp.StatusCode, want, body)
	}

	// The session bob is already holding (store.LookupSession). Deactivation
	// deletes no session row and the failed lookup does not clear the cookie,
	// so this is that filter alone - and the same cookie has to work again once
	// he is back.
	if status, _ := get(t, e.bobClient, e.baseURL+"/"); (status == http.StatusOK) != want {
		t.Fatalf("bob's existing session: GET / status %d, want ok=%v", status, want)
	}

	// git over http: basic auth with the same token, against receive-pack.
	httpURL := fmt.Sprintf("http://bob:%s@127.0.0.1:%d/git/bob/course.git", e.bobToken, e.httpPort)
	out, err := gitErr(e.bobDir, nil, "push", httpURL, "main")
	if ok := err == nil; ok != want {
		t.Fatalf("git push over http: err=%v, want ok=%v\n%s", err, want, out)
	}

	// git over ssh: the registered key, which the token rotation above never
	// touched, so this is the ssh_keys lookup and nothing else.
	sshEnv := []string{"GIT_SSH_COMMAND=ssh -i " + e.bobKey +
		" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes"}
	out, err = gitErr(e.bobDir, sshEnv, "push", "origin", "main")
	if ok := err == nil; ok != want {
		t.Fatalf("git push over ssh: err=%v, want ok=%v\n%s", err, want, out)
	}
}

// 30. leaderboard: with anonymize off every authenticated user sees real
// logins; flipping anonymize on through a teacher push hides other students'
// logins from a student but not from the teacher (SPEC §10).
func testLeaderboard(t *testing.T, e *env) {
	status, body := get(t, e.aliceClient, e.baseURL+"/leaderboard")
	if status != http.StatusOK {
		t.Fatalf("GET /leaderboard: status %d", status)
	}
	for _, login := range []string{"alice", "bob"} {
		if !strings.Contains(body, login) {
			t.Fatalf("/leaderboard missing %q with anonymize off:\n%s", login, body)
		}
	}

	coursePath := filepath.Join(e.profCloneDir, "course.yaml")
	orig, err := os.ReadFile(coursePath)
	if err != nil {
		t.Fatalf("read course.yaml: %v", err)
	}
	writeFile(t, coursePath, strings.Replace(string(orig), "anonymize: false", "anonymize: true", 1))
	git(t, e.profCloneDir, nil, "add", "-A")
	git(t, e.profCloneDir, nil, "commit", "-q", "-m", "anonymize the leaderboard")
	git(t, e.profCloneDir, nil, "push", "origin", "main")
	t.Cleanup(func() {
		writeFile(t, coursePath, string(orig))
		git(t, e.profCloneDir, nil, "add", "-A")
		git(t, e.profCloneDir, nil, "commit", "-q", "-m", "de-anonymize the leaderboard")
		git(t, e.profCloneDir, nil, "push", "origin", "main")
	})

	_, body = get(t, e.aliceClient, e.baseURL+"/leaderboard")
	if strings.Contains(body, "bob") {
		t.Errorf("anonymized /leaderboard leaks bob's login to a student:\n%s", body)
	}
	// Staff keep the logins: anonymization is there so students cannot rank
	// each other, and a TA who may open every submission would only be sent to
	// the matrix for the same names (SPEC §8, §10).
	if _, staff := get(t, e.taClient, e.baseURL+"/leaderboard"); !strings.Contains(staff, "bob") {
		t.Errorf("anonymized /leaderboard hides the logins from a TA:\n%s", staff)
	}
	_, body = get(t, e.profClient, e.baseURL+"/leaderboard")
	if !strings.Contains(body, "bob") {
		t.Errorf("anonymized /leaderboard hides bob from the teacher too:\n%s", body)
	}
}

// 31. max_push_size: a teacher lowers the limit without a restart, a student's
// oversized push is refused with anygrade's own message naming the limit and
// how to recover, and the limit is lifted again the same way (SPEC §13).
func testMaxPushSize(t *testing.T, e *env) {
	coursePath := filepath.Join(e.profCloneDir, "course.yaml")
	orig, err := os.ReadFile(coursePath)
	if err != nil {
		t.Fatalf("read course.yaml: %v", err)
	}
	// Single-letter suffix: the byte-size parser takes 64K, not 64KB.
	writeFile(t, coursePath, string(orig)+"\nlimits:\n  max_push_size: 64K\n")
	git(t, e.profCloneDir, nil, "add", "-A")
	git(t, e.profCloneDir, nil, "commit", "-q", "-m", "cap the push size")
	if out := git(t, e.profCloneDir, nil, "push", "origin", "main"); !strings.Contains(out, "course metadata reloaded") {
		t.Fatalf("the limit push was not applied:\n%s", out)
	}
	t.Cleanup(func() {
		writeFile(t, coursePath, string(orig))
		git(t, e.profCloneDir, nil, "add", "-A")
		git(t, e.profCloneDir, nil, "commit", "-q", "-m", "lift the push size cap")
		git(t, e.profCloneDir, nil, "push", "origin", "main")
	})

	// Random bytes: a compressible blob would slip under the cap in the pack.
	blob := make([]byte, 1<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(filepath.Join(e.aliceDir, "big.bin"), blob, 0o644); err != nil {
		t.Fatalf("write big.bin: %v", err)
	}
	git(t, e.aliceDir, nil, "add", "-A")
	git(t, e.aliceDir, nil, "commit", "-q", "-m", "push something oversized")
	out, err := gitErr(e.aliceDir, nil, "push", "origin", "main")
	if err == nil {
		t.Fatalf("oversized push unexpectedly succeeded:\n%s", out)
	}
	if !strings.Contains(out, "anygrade: push rejected: it is larger than max_push_size (64") {
		t.Fatalf("oversized push rejection is not anygrade's message naming the limit:\n%s", out)
	}
	if !strings.Contains(out, "drop the large files from the commit") {
		t.Fatalf("oversized push rejection does not say how to recover:\n%s", out)
	}

	// Drop the blob so later scenarios push a normal-sized history again.
	git(t, e.aliceDir, nil, "reset", "--hard", "HEAD~1")
}

// 33. TLS listener: a server started with --tls-cert/--tls-key serves the one
// HTTP port over HTTPS, and both things that ride it - the web UI and git smart
// HTTP - work over the encrypted listener (SPEC §11, §14). The certificate is
// generated here and handed to the Go client as its only root CA and to git as
// GIT_SSL_CAINFO, so every request below verifies a real chain instead of
// skipping verification.
func testTLSListener(t *testing.T, e *env) {
	certPath, keyPath, certPEM := genTLSCert(t, e.root)
	client := newTLSClient(t, certPEM)
	tlsEnv := startTLSServer(t, e, certPath, keyPath, client)

	status, body := get(t, client, tlsEnv.baseURL+"/login")
	if status != http.StatusOK {
		t.Fatalf("GET %s/login: status %d, body:\n%s", tlsEnv.baseURL, status, body)
	}
	if !strings.Contains(body, "Sign in") {
		t.Fatalf("GET %s/login did not render the login form:\n%s", tlsEnv.baseURL, body)
	}

	// Without the generated CA the same URL must be refused; otherwise the
	// request above would prove nothing about the certificate.
	if resp, err := newClient(t).Get(tlsEnv.baseURL + "/login"); err == nil {
		resp.Body.Close()
		t.Errorf("%s/login verified without the generated CA", tlsEnv.baseURL)
	}

	// TLS is the only thing the port speaks: a plaintext request does not get
	// the app back, whatever the transport answers with.
	if resp, err := newClient(t).Get(fmt.Sprintf("http://127.0.0.1:%d/login", tlsEnv.httpPort)); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("the TLS port served the app over plaintext http")
		}
	}

	out := runBin(t, "", "user", "add", "--login", "tina", "--role", "student", "--data-dir", tlsEnv.dataDir)
	token := reToken.FindString(out)
	if token == "" {
		t.Fatalf("no token in `user add` output:\n%s", out)
	}

	// The session cookie is a bearer credential, and the whole point of running
	// the listener over TLS is that it may then be marked Secure. The flag is
	// decided from the connection actually being encrypted, so only a real
	// HTTPS listener can prove the decision - which is what makes this the one
	// assertion the unit tests cannot make for themselves.
	resp, body := postForm(t, newTLSClient(t, certPEM), tlsEnv.baseURL+"/login", url.Values{
		"login": {"tina"}, "token": {token}, "next": {"/"},
	})
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login tina over https: status %d, body:\n%s", resp.StatusCode, body)
	}
	session := findCookie(resp.Cookies(), "ag_session")
	if session == nil {
		t.Fatalf("login over https issued no session cookie: %v", resp.Cookies())
	}
	if !session.Secure {
		t.Errorf("the session cookie issued over https is not Secure: %q", session.Raw)
	}
	if !session.HttpOnly {
		t.Errorf("the session cookie issued over https is not HttpOnly: %q", session.Raw)
	}

	// git over the same listener. -c http.sslVerify=true throughout so a
	// machine that has turned verification off globally cannot turn either
	// half of this into a pass.
	cloneURL := fmt.Sprintf("https://tina:%s@127.0.0.1:%d/git/tina/course.git", token, tlsEnv.httpPort)
	if out, err := gitErr(e.root, nil, "-c", "http.sslVerify=true", "ls-remote", cloneURL); err == nil {
		t.Fatalf("git verified the certificate without GIT_SSL_CAINFO:\n%s", out)
	}

	caEnv := []string{"GIT_SSL_CAINFO=" + certPath}
	cloneDir := filepath.Join(e.root, "tina")
	git(t, e.root, caEnv, "-c", "http.sslVerify=true", "clone", cloneURL, cloneDir)
	setIdentity(t, cloneDir)
	writeFile(t, filepath.Join(cloneDir, "tasks", "sum", "sum.sh"), sumSolution)
	git(t, cloneDir, nil, "add", "-A")
	git(t, cloneDir, nil, "commit", "-q", "-m", "solve sum over https")
	pushOut := git(t, cloneDir, caEnv, "-c", "http.sslVerify=true", "push", "origin", "main")
	// receive-pack, the hook, and intake all ran behind TLS: the push output is
	// anygrade's, not git's default.
	taskSubmissionID(t, pushOut, "sum")

	// Half a pair is a misconfiguration that must be refused at startup, not
	// silently served as plaintext: a deployment that believes it is encrypted
	// and is not is worse than one that failed to come up. The check runs
	// before anything binds, so neither invocation needs a port or a data dir.
	for _, tc := range []struct{ args, want string }{
		{"--tls-cert " + certPath, "--tls-cert requires --tls-key"},
		{"--tls-key " + keyPath, "--tls-key requires --tls-cert"},
	} {
		out, err := runBinErr(e.root, append([]string{"serve"}, strings.Fields(tc.args)...)...)
		if err == nil {
			t.Errorf("`serve %s` started without its pair:\n%s", tc.args, out)
			continue
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("`serve %s` did not name the missing flag (want %q):\n%s", tc.args, tc.want, out)
		}
	}
}

// findCookie returns the named cookie, or nil. http.Response.Cookies() is the
// parsed Set-Cookie list, so this reads the attributes the server actually
// sent rather than what the jar decided to keep.
func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// startTLSServer brings up a second server over HTTPS on its own data dir and
// ports. It reuses startServer/stopServer through a second env value, so the
// ordered suite's own server, ports and data dir are left untouched. client
// must trust certPath: startServer polls readiness with it.
func startTLSServer(t *testing.T, e *env, certPath, keyPath string, client *http.Client) *env {
	t.Helper()
	httpPort := freePort(t)

	logPath := filepath.Join(e.root, "serve-tls.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create serve-tls.log: %v", err)
	}
	tlsEnv := &env{
		root:      e.root,
		baseURL:   fmt.Sprintf("https://127.0.0.1:%d", httpPort),
		httpPort:  httpPort,
		sshPort:   freePort(t),
		courseDir: e.courseDir,
		dataDir:   filepath.Join(e.root, "data-tls"),
		serverLog: logFile,
		tlsCert:   certPath,
		tlsKey:    keyPath,
		tlsClient: client,
	}
	t.Cleanup(func() {
		stopServer(t, tlsEnv, syscall.SIGTERM)
		logFile.Close()
		if t.Failed() {
			if b, err := os.ReadFile(logPath); err == nil {
				t.Logf("serve-tls.log:\n%s", b)
			}
		}
	})
	startServer(t, tlsEnv)
	return tlsEnv
}

// genTLSCert writes a self-signed ECDSA certificate and its key into dir and
// returns their paths plus the certificate PEM. The certificate is its own CA
// so the one file works as a trust anchor for both Go's RootCAs and git's
// GIT_SSL_CAINFO.
func genTLSCert(t *testing.T, dir string) (certPath, keyPath string, certPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate tls key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "anygrade e2e"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create tls certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal tls key: %v", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	certPath = filepath.Join(dir, "tls-cert.pem")
	keyPath = filepath.Join(dir, "tls-key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("write %s: %v", certPath, err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write %s: %v", keyPath, err)
	}
	return certPath, keyPath, certPEM
}

// newTLSClient is newClient with caPEM as its only trust anchor, so a request
// that succeeds against the TLS listener proves the chain verified.
func newTLSClient(t *testing.T, caPEM []byte) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("the generated certificate is not a usable trust anchor")
	}
	c := newClient(t)
	c.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	return c
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

// 32. score override: a teacher's manual score beats the computed one in the
// CSV export and shows up in the audit log; clearing it restores the computed
// score (SPEC §9). Runs after every scenario that asserts alice's computed sum
// score, so a stray failure here cannot cascade into them.
func testScoreOverride(t *testing.T, e *env) {
	target := e.baseURL + "/students/alice/tasks/sum/override"
	resp, body := postForm(t, e.profClient, target, url.Values{
		"score": {"77"}, "comment": {"partial credit, discussed in class"},
	})
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("set override: status %d, body:\n%s", resp.StatusCode, body)
	}
	if got := fetchScores(t, e)["alice"]["sum"]; got != "77" {
		t.Fatalf("alice/sum with an override: got %q, want 77", got)
	}

	_, page := get(t, e.profClient, e.baseURL+"/audit")
	if !strings.Contains(page, "override") {
		t.Errorf("/audit does not record the override:\n%s", page)
	}

	resp, body = postForm(t, e.profClient, target+"/delete", nil)
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("clear override: status %d, body:\n%s", resp.StatusCode, body)
	}
	if got := fetchScores(t, e)["alice"]["sum"]; got != "100" {
		t.Fatalf("alice/sum after clearing the override: got %q, want 100", got)
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

// 38. task removed from the course repo: a teacher push deletes a task while a
// submission for it is still queued. That submission fails terminally with the
// reason recorded, the graded history stays readable, and everything the course
// snapshot drives - matrix column, CSV column, task page - drops the task
// (SPEC §13).
//
// Runs last, on a task of its own: it is the only scenario that takes a task
// away, so nothing may still need it afterwards.
func testTaskRemoved(t *testing.T, e *env) {
	notes := filepath.Join(e.aliceDir, "tasks", "retired", "notes.txt")
	// The task's only check is `sleep "$(cat notes.txt)"`, so the solution file
	// decides how long a submission occupies its worker.
	submit := func(sleep, msg string) int {
		t.Helper()
		writeFile(t, notes, sleep+"\n")
		git(t, e.aliceDir, nil, "add", "-A")
		git(t, e.aliceDir, nil, "commit", "-q", "-m", msg)
		return taskSubmissionID(t, git(t, e.aliceDir, nil, "push", "origin", "main"), "retired")
	}

	// The history the removal must not erase: graded and scored while the task
	// is still part of the course.
	graded := submit("0", "retired: solve")
	pollSubmission(t, e.aliceClient, e, graded)
	if got := fetchScores(t, e)["alice"]["retired"]; got != "10" {
		t.Fatalf("alice/retired before the removal: got %q, want 10", got)
	}

	// Catching a submission in `queued` is not a matter of being quick about
	// the push: the claim query skips a row while an *earlier* submission of
	// the same (student, task) pair is running (SPEC §13, ordering), so a
	// submission that sleeps pins the next one in `queued` for as long as this
	// scenario wants - no worker can take it in the meantime.
	blocker := submit("30", "retired: hold the pair's queue slot")
	pollStatus(t, e.aliceClient, e, blocker, "running")
	victim := submit("0", "retired: still queued when the task disappears")
	pollStatus(t, e.aliceClient, e, victim, "queued")

	git(t, e.profCloneDir, nil, "rm", "-r", "-q", "tasks/retired")
	git(t, e.profCloneDir, nil, "commit", "-q", "-m", "retire the task")
	// The reload line is the confirmation that the snapshot has already been
	// swapped, so nothing below races the teacher's push.
	if out := git(t, e.profCloneDir, nil, "push", "origin", "main"); !strings.Contains(out, "course metadata reloaded") {
		t.Fatalf("the removal push did not reload the course:\n%s", out)
	}

	// Release the slot: the blocker was only ever scaffolding, and the victim
	// is the one that now gets prepared against a course without the task.
	resp, body := postForm(t, e.profClient, fmt.Sprintf("%s/queue/%d/cancel", e.baseURL, blocker), nil)
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("cancel the blocker #%d: status %d, body:\n%s", blocker, resp.StatusCode, body)
	}

	// `error`, not `retrying`: the display status is exactly the difference
	// between a cleared retry_at and a scheduled one, so reaching it proves the
	// failure is terminal rather than a backoff that will come round again.
	page := pollStatus(t, e.aliceClient, e, victim, "error")
	if !strings.Contains(page, "task no longer exists in the course repo") {
		t.Fatalf("submission #%d does not record why it failed:\n%s", victim, page)
	}
	// A prepare that never ran a check has nothing but its note to explain
	// itself, so the note must not be traded for the empty-results hint.
	if strings.Contains(page, "No check results recorded.") {
		t.Errorf("submission #%d shows the empty-results hint instead of its note:\n%s", victim, page)
	}
	// The same reason reaches the teacher's queue row.
	status, queue := get(t, e.profClient, e.baseURL+"/queue")
	if status != http.StatusOK {
		t.Fatalf("GET /queue: status %d", status)
	}
	if !strings.Contains(queue, "task no longer exists in the course repo") {
		t.Errorf("queue row for submission #%d missing the reason:\n%s", victim, queue)
	}

	// The course is the source of truth for every task-keyed view, so all of
	// them lose the column the moment the snapshot swaps.
	status, matrix := get(t, e.profClient, e.baseURL+"/matrix")
	if status != http.StatusOK {
		t.Fatalf("GET /matrix: status %d", status)
	}
	if strings.Contains(matrix, ">retired<") {
		t.Fatalf("/matrix still has a column for the removed task:\n%s", matrix)
	}
	if _, ok := fetchScores(t, e)["alice"]["retired"]; ok {
		t.Errorf("scores.csv still exports a column for the removed task")
	}
	if status, _ := get(t, e.aliceClient, e.baseURL+"/tasks/retired"); status != http.StatusNotFound {
		t.Fatalf("GET /tasks/retired after the removal: status %d, want 404", status)
	}

	// The graded submission is the exception: history, not course metadata. It
	// keeps its status and its score with no course entry left to name it.
	status, page = get(t, e.aliceClient, fmt.Sprintf("%s/submissions/%d", e.baseURL, graded))
	if status != http.StatusOK {
		t.Fatalf("GET the graded submission after the removal: status %d", status)
	}
	if !strings.Contains(page, ">done<") || !strings.Contains(page, "raw 10") {
		t.Fatalf("submission #%d lost its result when the task was removed:\n%s", graded, page)
	}

	// The terminal row offers the teacher a recheck button, and there is no
	// task behind it any more: intake refuses (recheck.go, "unknown task") and
	// queues nothing in its place. Only the refusal is pinned - today's generic
	// 500 is not the message this deserves.
	resp, body = postForm(t, e.profClient, fmt.Sprintf("%s/queue/%d/recheck", e.baseURL, victim), nil)
	if resp.StatusCode < 400 {
		t.Fatalf("recheck of the removed task: status %d, Location %q, want a refusal:\n%s",
			resp.StatusCode, resp.Header.Get("Location"), body)
	}
}

// regexpRejected matches either push-rejection phrasing intake.go uses.
var regexpRejected = regexp.MustCompile(`push rejected|validation failed`)

// 10. hidden-tests boundary: the greet task's hidden check is two phases
// (SPEC §6.1). The build phase runs the hidden test and stamps an artifact;
// the run phase asserts the artifact survived and the hidden source did not.
// bob's greet submission already scored 50 in the previous scenario, which is
// only reachable if both phases passed - so what is left to prove is where the
// output of the phase that read the hidden tests ended up.
func testHiddenTestsBoundary(t *testing.T, e *env) {
	logURL := func(check, query string) string {
		return fmt.Sprintf("%s/submissions/%d/logs/%s%s", e.baseURL, e.bobGreetSubID, check, query)
	}

	// Staff, and only staff, can read the build phase (the TA scenario checks
	// the other half of that).
	status, body := get(t, e.profClient, logURL("hidden", "?phase=build"))
	if status != http.StatusOK {
		t.Fatalf("teacher GET build log: status %d, body:\n%s", status, body)
	}
	if !strings.Contains(body, hiddenSecret) {
		t.Fatalf("build log does not carry the hidden test output:\n%s", body)
	}
	if status, _ := get(t, e.bobClient, logURL("hidden", "?phase=build")); status != http.StatusNotFound {
		t.Fatalf("the student read the build log: status %d", status)
	}

	// The run phase happened after the removal, so even the teacher's copy of
	// it carries nothing of the hidden tests.
	status, body = get(t, e.profClient, logURL("hidden", ""))
	if status != http.StatusOK {
		t.Fatalf("teacher GET run log: status %d", status)
	}
	if strings.Contains(body, hiddenSecret) {
		t.Fatalf("the run phase saw the hidden tests:\n%s", body)
	}

	// And the student's own page - excerpts, live panes, download links - has
	// no trace of the phase that did.
	status, body = get(t, e.bobClient, fmt.Sprintf("%s/submissions/%d", e.baseURL, e.bobGreetSubID))
	if status != http.StatusOK {
		t.Fatalf("student GET submission: status %d", status)
	}
	if strings.Contains(body, hiddenSecret) || strings.Contains(body, "phase=build") {
		t.Fatalf("the student page leaks the build phase:\n%s", body)
	}
}
