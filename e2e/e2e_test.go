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
	sshPort   int
	courseDir string
	dataDir   string
	hiddenDir string
	profToken string

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
	t.Run("teacher pages", func(t *testing.T) { testTeacherPages(t, e) })
	t.Run("teacher pushes course update", func(t *testing.T) { testTeacherCourseUpdate(t, e) })
	t.Run("language switcher", func(t *testing.T) { testLanguageSwitcher(t, e) })
	t.Run("auth and rate limit", func(t *testing.T) { testAuthAndRateLimit(t, e) })
	t.Run("serve --local", func(t *testing.T) { testServeLocal(t, e) })
	t.Run("local self-check", func(t *testing.T) { testLocalSelfCheck(t, e) })
	t.Run("force push after submission", func(t *testing.T) { testForcePushAfterSubmission(t, e) })
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

	cmd := exec.Command(bin, "serve",
		"--repo", courseDir,
		"--data-dir", dataDir,
		"--http-addr", fmt.Sprintf("127.0.0.1:%d", httpPort),
		"--ssh-addr", fmt.Sprintf("127.0.0.1:%d", sshPort),
		"--workers", "2",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
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
				t.Logf("serve.log:\n%s", b)
			}
		}
	})

	waitReady(t, baseURL)

	return &env{
		root:       root,
		baseURL:    baseURL,
		sshPort:    sshPort,
		courseDir:  courseDir,
		dataDir:    dataDir,
		hiddenDir:  hiddenDir,
		profToken:  profToken,
		profClient: newClient(t),
	}
}

// waitReady polls GET /login until the server accepts connections.
func waitReady(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/login")
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
