//go:build e2e

// Package e2e drives the real anygrade binary end to end: CLI, HTTP forms,
// git clones and pushes over both transports. Build with `-tags e2e` (see
// `make e2e`); it is skipped by plain `go test ./...`.
package e2e

import (
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bin is the path to the anygrade binary built once by TestMain.
var bin string

var (
	reToken      = regexp.MustCompile(`ag_[0-9a-f]{64}`)
	reInvite     = regexp.MustCompile(`inv_[0-9a-f]{64}`)
	reChallenge  = regexp.MustCompile(`agc_[0-9a-f]{64}`)
	reProofCmd   = regexp.MustCompile(`printf '%s' '([^']*)'`)
	reSubmission = regexp.MustCompile(`submission #(\d+) queued`)
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "anygrade-e2e-bin-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: mkdtemp:", err)
		os.Exit(1)
	}
	bin = filepath.Join(tmp, "anygrade")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/anygrade")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build anygrade: %v\n%s\n", err, out)
		os.RemoveAll(tmp)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// env is the shared, mutable state of one running server across the ordered
// TestE2E subtests.
type env struct {
	root      string
	baseURL   string
	httpPort  int
	sshPort   int
	courseDir string
	dataDir   string
	hiddenDir string
	profToken string

	// The running server, so a scenario can kill and respawn it.
	serverCmd *exec.Cmd
	serverLog *os.File

	// Set only by the TLS scenario's own env: serve the HTTP listener over
	// TLS and poll readiness with a client that trusts the generated
	// certificate.
	tlsCert   string
	tlsKey    string
	tlsClient *http.Client

	profClient *http.Client

	// Filled in by later scenarios.
	aliceToken  string
	aliceDir    string
	aliceClient *http.Client

	bobToken  string
	bobDir    string
	bobKey    string // path to the ed25519 private key (no extension)
	bobClient *http.Client

	profCloneDir string

	// Submission ids threaded between ordered subtests.
	aliceSumSubID int
	bobGreetSubID int
}

func TestE2E(t *testing.T) {
	e := startEnv(t)

	t.Run("validate", func(t *testing.T) { testValidate(t, e) })
	t.Run("teacher login", func(t *testing.T) { testTeacherLogin(t, e) })
	t.Run("open registration", func(t *testing.T) { testOpenRegistration(t, e) })
	t.Run("clone and push over http", func(t *testing.T) { testCloneAndPushHTTP(t, e) })
	t.Run("graded done", func(t *testing.T) { testGradedDone(t, e) })
	t.Run("best policy keeps top score", func(t *testing.T) { testBestPolicy(t, e) })
	t.Run("hard deadline rejects", func(t *testing.T) { testHardDeadline(t, e) })
	t.Run("recheck marker", func(t *testing.T) { testRecheckMarker(t, e) })
	t.Run("invite and ssh transport", func(t *testing.T) { testInviteAndSSH(t, e) })
	t.Run("hidden tests boundary", func(t *testing.T) { testHiddenTestsBoundary(t, e) })
	t.Run("teacher pages", func(t *testing.T) { testTeacherPages(t, e) })
	t.Run("teacher pushes course update", func(t *testing.T) { testTeacherCourseUpdate(t, e) })
	t.Run("language switcher", func(t *testing.T) { testLanguageSwitcher(t, e) })
	t.Run("auth and rate limit", func(t *testing.T) { testAuthAndRateLimit(t, e) })
	t.Run("serve --local", func(t *testing.T) { testServeLocal(t, e) })
	t.Run("local self-check", func(t *testing.T) { testLocalSelfCheck(t, e) })
	t.Run("restart requeues a running submission", func(t *testing.T) { testRestartRequeue(t, e) })
	t.Run("teacher cancels a running submission", func(t *testing.T) { testCancelRunning(t, e) })
	t.Run("check timeout", func(t *testing.T) { testCheckTimeout(t, e) })
	t.Run("soft deadline penalty", func(t *testing.T) { testSoftDeadlinePenalty(t, e) })
	t.Run("attempt limit", func(t *testing.T) { testAttemptLimit(t, e) })
	t.Run("hidden tests unavailable", func(t *testing.T) { testHiddenTestsScrubbed(t, e) })
	t.Run("multiple tasks in one push", func(t *testing.T) { testMultiTaskPush(t, e) })
	t.Run("non-default branch", func(t *testing.T) { testNonDefaultBranch(t, e) })
	t.Run("push without task changes", func(t *testing.T) { testNonTaskPush(t, e) })
	t.Run("cross-student access", func(t *testing.T) { testCrossStudentAccess(t, e) })
	t.Run("cli export against a live server", func(t *testing.T) { testCLIExport(t, e) })
	t.Run("token reset", func(t *testing.T) { testTokenReset(t, e) })
	t.Run("deactivate and reactivate a student", func(t *testing.T) { testDeactivateStudent(t, e) })
	t.Run("leaderboard", func(t *testing.T) { testLeaderboard(t, e) })
	t.Run("max push size", func(t *testing.T) { testMaxPushSize(t, e) })
	t.Run("tls listener", func(t *testing.T) { testTLSListener(t, e) })
	t.Run("force push after submission", func(t *testing.T) { testForcePushAfterSubmission(t, e) })
	t.Run("score override", func(t *testing.T) { testScoreOverride(t, e) })
	t.Run("tamper notes", func(t *testing.T) { testTamperNotes(t, e) })
}

// startEnv builds the course + hidden-tests fixtures, creates the teacher,
// and starts the server; t.Cleanup tears it down.
func startEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()

	hiddenDir := writeHiddenFixture(t, root)
	courseDir := writeCourseFixture(t, root, hiddenDir)
	dataDir := filepath.Join(root, "data")

	out := runBin(t, "", "user", "add", "--login", "prof", "--role", "teacher", "--data-dir", dataDir)
	profToken := reToken.FindString(out)
	if profToken == "" {
		t.Fatalf("no token in `user add` output:\n%s", out)
	}

	httpPort := freePort(t)
	sshPort := freePort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	logPath := filepath.Join(root, "serve.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create serve.log: %v", err)
	}

	e := &env{
		root:       root,
		baseURL:    baseURL,
		httpPort:   httpPort,
		sshPort:    sshPort,
		courseDir:  courseDir,
		dataDir:    dataDir,
		hiddenDir:  hiddenDir,
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

	startServer(t, e)
	return e
}

// startServer starts `anygrade serve` on the fixture and records it on env.
// The log file is opened once by startEnv and appended to by every restart.
func startServer(t *testing.T, e *env) {
	t.Helper()
	args := []string{"serve",
		"--repo", e.courseDir,
		"--data-dir", e.dataDir,
		"--http-addr", fmt.Sprintf("127.0.0.1:%d", e.httpPort),
		"--ssh-addr", fmt.Sprintf("127.0.0.1:%d", e.sshPort),
		"--workers", "2",
	}
	ready := http.DefaultClient
	if e.tlsCert != "" {
		args = append(args, "--tls-cert", e.tlsCert, "--tls-key", e.tlsKey)
		ready = e.tlsClient
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = e.serverLog
	cmd.Stderr = e.serverLog
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	e.serverCmd = cmd
	waitReadyWith(t, ready, e.baseURL)
}

// stopServer signals the running server and waits for it to exit, escalating
// to SIGKILL if it does not go within 10s.
func stopServer(t *testing.T, e *env, sig syscall.Signal) {
	t.Helper()
	if e.serverCmd == nil {
		return
	}
	_ = e.serverCmd.Process.Signal(sig)
	done := make(chan error, 1)
	go func() { done <- e.serverCmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = e.serverCmd.Process.Kill()
		<-done
	}
	e.serverCmd = nil
}

// restartServer kills the server the way a crash would - no graceful shutdown,
// no chance to finish the run in flight - and brings a fresh one up on the same
// data dir and ports.
func restartServer(t *testing.T, e *env) {
	t.Helper()
	stopServer(t, e, syscall.SIGKILL)
	startServer(t, e)
}

// waitReady polls GET /login until the server accepts connections.
func waitReady(t *testing.T, baseURL string) {
	t.Helper()
	waitReadyWith(t, http.DefaultClient, baseURL)
}

// waitReadyWith is waitReady over a caller-supplied client, so an HTTPS
// listener can be polled with the certificate it was started with.
func waitReadyWith(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/login")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready", baseURL)
}

// freePort picks an ephemeral loopback TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runBin runs the anygrade binary with dir as its working directory ("" =
// inherit) and fails the test on a nonzero exit.
func runBin(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runBinErr(dir, args...)
	if err != nil {
		t.Fatalf("anygrade %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func runBinErr(dir string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ---- git helpers ----

func git(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	out, err := gitErr(dir, env, args...)
	if err != nil {
		t.Fatalf("git %s (dir=%s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func gitErr(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// setIdentity configures the commit identity used by every fixture clone.
func setIdentity(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, nil, "config", "user.email", "e2e@test")
	git(t, dir, nil, "config", "user.name", "e2e")
}

// ---- HTTP helpers ----

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func postForm(t *testing.T, client *http.Client, target string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := client.PostForm(target, form)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body from POST %s: %v", target, err)
	}
	return resp, string(body)
}

func get(t *testing.T, client *http.Client, target string) (int, string) {
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
	return resp.StatusCode, string(body)
}

// login authenticates client via the web login form.
func login(t *testing.T, e *env, client *http.Client, loginName, token string) {
	t.Helper()
	resp, body := postForm(t, client, e.baseURL+"/login", url.Values{
		"login": {loginName}, "token": {token}, "next": {"/"},
	})
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login %s: status %d, body:\n%s", loginName, resp.StatusCode, body)
	}
	status, _ := get(t, client, e.baseURL+"/")
	if status != http.StatusOK {
		t.Fatalf("login %s: GET / after login: status %d", loginName, status)
	}
}

// pollSubmission waits for a submission to finish, returning the rendered
// page. Fails the test on infra_error or timeout.
func pollSubmission(t *testing.T, client *http.Client, e *env, id int) string {
	t.Helper()
	target := fmt.Sprintf("%s/submissions/%d", e.baseURL, id)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		status, body := get(t, client, target)
		if status != http.StatusOK {
			t.Fatalf("GET %s: status %d", target, status)
		}
		if strings.Contains(body, ">infra_error<") {
			t.Fatalf("submission #%d: infra_error:\n%s", id, body)
		}
		if strings.Contains(body, ">done<") {
			return body
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("submission #%d did not finish within 90s", id)
	return ""
}

// pollStatus waits for a submission to reach one specific status and returns
// the rendered page. Unlike pollSubmission it does not treat infra_error as a
// failure - scenarios that expect one ask for it by name.
func pollStatus(t *testing.T, client *http.Client, e *env, id int, want string) string {
	t.Helper()
	target := fmt.Sprintf("%s/submissions/%d", e.baseURL, id)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		status, body := get(t, client, target)
		if status != http.StatusOK {
			t.Fatalf("GET %s: status %d", target, status)
		}
		if strings.Contains(body, ">"+want+"<") {
			return body
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("submission #%d never reached %q within 90s", id, want)
	return ""
}

// fetchScores downloads the teacher CSV export and indexes it by login and
// column name.
func fetchScores(t *testing.T, e *env) map[string]map[string]string {
	t.Helper()
	target := e.baseURL + "/export/scores.csv"
	resp, err := e.profClient.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", target, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("GET %s: unexpected Content-Type %q", target, ct)
	}
	records, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("parse scores.csv: %v", err)
	}
	if len(records) == 0 {
		t.Fatalf("scores.csv: empty")
	}
	header := records[0]
	out := map[string]map[string]string{}
	for _, rec := range records[1:] {
		row := map[string]string{}
		for i, col := range header {
			if i < len(rec) {
				row[col] = rec[i]
			}
		}
		out[row["login"]] = row
	}
	return out
}
